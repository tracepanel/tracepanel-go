package tracepanel

import (
	"bytes"
	"context"
	"log/slog"
	"runtime/pprof"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Where a trace's time goes in code: Go's CPU profiler (runtime/pprof), the trace's `profile`, a flame graph in the
// console. Nothing runs until a trace is profiled: TRACEPANEL_PROFILE=1 (or Config.Profile) profiles every one, a
// request asks with the X-Tracepanel-Profile: 1 header or the panel's tracepanel_profile=1 cookie, and a task
// started from a profiled trace (tracepanel.Go) is profiled too.
//
// The profiler is the process's, sampling every goroutine on a CPU 100 times a second, so a sample stands for 10 ms
// of CPU time. A profiled trace's goroutine carries a pprof label naming the trace (tracepanel_trace), which the
// goroutines it starts inherit; a sample is the trace's when it carries its label. That holds with any number of
// requests at once: no sample of another request is taken for this one. The profile is the CPU's time only: a
// goroutine waiting (for the database, a lock, a sleep) isn't sampled.
//
// The profiler runs while profiled traces do. runtime/pprof hands its samples over only once stopped, so it is cut
// now and then: stopped, its samples shared out (the ended traces get their profile, the running ones keep theirs),
// and started again for those still running. Stopping takes a while (on macOS up to 100 ms, as runtime/pprof reads
// its samples every 100 ms there; on Linux at once), when nothing is sampled, so it isn't stopped as each trace ends:
// a cut waits until no profiled trace has run for 200 ms, or, under steady load, until an ended trace has waited 2 s
// for its profile. Back-to-back traces are sampled without a gap. A trace that runs during a cut misses that time,
// and says so (tracepanel.profile_gap_ms). Meanwhile a trace's profile waits for its cut, its document waits (in the
// background: the response has gone). Samples the profiler lost (its buffer full) can't be told apart: each trace
// profiled then counts them as `dropped`.
//
// Its memory, when asked for, and the collector's runs: memory.go.
//
// An app that runs a CPU profile of its own (net/http/pprof's /debug/pprof/profile) has the profiler: its traces
// get no profile meanwhile, and the log says so once.

const (
	// The label a profiled trace's goroutines carry: its id.
	profileLabel = "tracepanel_trace"
	// How deep a stack is kept: a recursion beyond it is cut, outermost frames kept.
	maxProfileDepth = 128
	// Distinct stacks one trace keeps; the rest are counted as one "(other)" frame, so a profile stays small.
	maxProfileStacks = 2000
	// runtime/pprof samples 100 times a second.
	defaultIntervalUs = 10_000
)

type profileDoc struct {
	IntervalUs int64          `json:"interval_us"`
	Samples    int64          `json:"samples"`
	Dropped    int64          `json:"dropped"`
	Frames     []profileFrame `json:"frames"`
	Stacks     []profileStack `json:"stacks"`
	// What the heap did while the trace ran, when asked for, and the collector's runs (memory.go).
	Memory *profileMemory `json:"memory,omitempty"`
	GC     *profileGC     `json:"gc,omitempty"`
}

type profileFrame struct {
	Function string  `json:"function"`
	Module   *string `json:"module"`
	File     string  `json:"file"`
	Line     int     `json:"line"`
	InApp    bool    `json:"in_app"`
}

type profileStack struct {
	Frames []int `json:"frames"`
	Count  int64 `json:"count"`
}

// traceProfile is a profiled trace's samples so far: its stacks (as keys into frames) and how often each was seen.
type traceProfile struct {
	id      string
	stacks  map[string]*profileStack
	frames  []profileFrame
	index   map[cpuFrame]int
	samples int64
	dropped int64
	// The profiler couldn't run while it did (another CPU profile had it).
	missed bool
	// Ended: its profile waits for the next cut.
	ended   bool
	endedAt time.Time
	// While the profiler was stopped for a cut and the trace ran: since when (zero while it runs), and in all.
	pausedAt time.Time
	gap      time.Duration
	done     func(*profileDoc, time.Duration)
}

// profiler runs the process's CPU profile while traces are profiled, and shares its samples out among them.
//
// runtime/pprof hands a profile's samples over only once it is stopped, and stopping it takes a while (on macOS up to
// 100 ms: its writer reads the samples every 100 ms there), during which nothing is sampled. So the profile isn't
// stopped when a trace ends: it runs on, for the traces still running and those starting next, and is cut (stopped,
// its samples shared out, started again for the traces running) once no profiled trace has run for cutIdle, or, while
// they keep running, once an ended trace has waited cutWait for its profile. Back-to-back traces are sampled without
// a gap; only a trace that runs while a cut stops the profiler misses that time, and says how much
// (tracepanel.profile_gap_ms).
type profiler struct {
	mu      sync.Mutex
	running bool
	cutting bool
	out     *bytes.Buffer
	// Every profiled trace whose profile isn't cut yet: those running and those ended.
	traces map[string]*traceProfile
	// The traces running, and those ended, in the order they ended.
	live   int
	ending []*traceProfile
	// When the last profiled trace ended, and the cut waiting for its time.
	lastEnd time.Time
	timer   *time.Timer
	// Which arming of the timer is current: a timer stopped too late to keep it from firing is let be.
	armed int
	// The ended traces whose profile isn't handed over yet, and what Flush waits on until there is none.
	pending int
	drained chan struct{}
	warned  bool
	// What starts and stops the CPU profile: runtime/pprof's, the tests' own.
	startCPU func(*bytes.Buffer) error
	stopCPU  func()
	// How long the profile runs on after the last profiled trace ended, and the longest an ended trace waits for
	// its profile while others keep running.
	cutIdle, cutWait time.Duration
}

const (
	defaultCutIdle = 200 * time.Millisecond
	defaultCutWait = 2 * time.Second
)

func newProfiler() *profiler {
	return &profiler{
		traces:   map[string]*traceProfile{},
		startCPU: func(out *bytes.Buffer) error { return pprof.StartCPUProfile(out) },
		stopCPU:  pprof.StopCPUProfile,
		cutIdle:  defaultCutIdle,
		cutWait:  defaultCutWait,
	}
}

// start profiles a trace from now on: the context its goroutine runs with carries the trace's label (the caller sets
// it on the goroutine, pprof.SetGoroutineLabels).
func (p *profiler) start(ctx context.Context, id string) (context.Context, *traceProfile) {
	tp := &traceProfile{id: id, stacks: map[string]*profileStack{}, index: map[cpuFrame]int{}}
	p.mu.Lock()
	p.traces[id] = tp
	p.live++
	switch {
	case p.cutting:
		// The profiler is being stopped for a cut: the trace is sampled once it starts again.
		tp.pausedAt = time.Now()
	case !p.running:
		p.begin()
		if !p.running {
			tp.missed = true
		}
	}
	p.mu.Unlock()
	return pprof.WithLabels(ctx, pprof.Labels(profileLabel, id)), tp
}

// begin starts the CPU profile (the caller holds the lock).
func (p *profiler) begin() {
	out := &bytes.Buffer{}
	if err := p.startCPU(out); err != nil {
		for _, tp := range p.traces {
			tp.missed = true
		}
		if !p.warned {
			p.warned = true
			slog.Warn("tracepanel: no profiles while the app runs a CPU profile of its own", "error", err.Error())
		}
		return
	}
	p.out = out
	p.running = true
}

// end ends a trace's profile: done gets it (and how long the profiler was stopped while the trace ran) once a cut
// handed its samples over, in another goroutine.
func (p *profiler) end(tp *traceProfile, done func(*profileDoc, time.Duration)) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.pending++
	now := time.Now()
	tp.done, tp.ended, tp.endedAt = done, true, now
	if !tp.pausedAt.IsZero() {
		tp.gap += now.Sub(tp.pausedAt)
		tp.pausedAt = time.Time{}
	}
	p.live--
	p.lastEnd = now
	p.ending = append(p.ending, tp)
	if p.live == 0 {
		// The last one running: the cut is cutIdle from now, not when the ones before it would have had theirs.
		p.disarm()
	}
	if !p.cutting {
		p.schedule(p.cutIdle)
	}
}

// disarm stops the cut's timer (the caller holds the lock).
func (p *profiler) disarm() {
	if p.timer != nil {
		p.timer.Stop()
		p.timer = nil
		p.armed++
	}
}

// schedule arms the cut's timer, unless it is armed already (the caller holds the lock).
func (p *profiler) schedule(after time.Duration) {
	if p.timer == nil {
		p.armed++
		armed := p.armed
		p.timer = time.AfterFunc(after, func() { p.due(armed) })
	}
}

// due is the cut's time, or not yet: while profiled traces run, the ended ones wait (cutWait at most); once none
// runs, the profile runs on for cutIdle, for the next trace.
func (p *profiler) due(armed int) {
	p.mu.Lock()
	if armed != p.armed {
		p.mu.Unlock()
		return
	}
	p.timer = nil
	if p.cutting || len(p.ending) == 0 {
		p.mu.Unlock()
		return
	}
	now := time.Now()
	if waited := now.Sub(p.ending[0].endedAt); p.live > 0 && waited < p.cutWait {
		p.schedule(p.cutWait - waited)
		p.mu.Unlock()
		return
	}
	if idle := now.Sub(p.lastEnd); p.live == 0 && idle < p.cutIdle {
		p.schedule(p.cutIdle - idle)
		p.mu.Unlock()
		return
	}
	p.cutting = true
	p.mu.Unlock()
	p.cut()
}

// cut stops the profile, shares its samples out, gives the ended traces their profiles, and starts it again for the
// traces still running.
func (p *profiler) cut() {
	p.mu.Lock()
	batch := p.ending
	p.ending = nil
	wasRunning, out := p.running, p.out
	p.running, p.out = false, nil
	stopped := time.Now()
	for _, tp := range p.traces {
		if !tp.ended && tp.pausedAt.IsZero() {
			tp.pausedAt = stopped
		}
	}
	p.mu.Unlock()

	var parsed *cpuProfile
	if wasRunning {
		p.stopCPU()
		parsed, _ = parseCPUProfile(out.Bytes(), profileLabel)
	}

	p.mu.Lock()
	if parsed != nil {
		p.share(parsed)
	}
	for _, tp := range batch {
		delete(p.traces, tp.id)
	}
	if p.live > 0 {
		p.begin()
	}
	now := time.Now()
	for _, tp := range p.traces {
		if !tp.ended && !tp.pausedAt.IsZero() {
			tp.gap += now.Sub(tp.pausedAt)
			tp.pausedAt = time.Time{}
		}
	}
	p.cutting = false
	if len(p.ending) > 0 {
		// Traces that ended during the cut: theirs is the next.
		p.schedule(p.cutIdle)
	}
	p.mu.Unlock()

	for _, tp := range batch {
		if tp.missed {
			tp.done(nil, tp.gap)
		} else {
			tp.done(tp.document(parsed), tp.gap)
		}
	}
	p.mu.Lock()
	p.pending -= len(batch)
	if p.pending == 0 && p.drained != nil {
		close(p.drained)
		p.drained = nil
	}
	p.mu.Unlock()
}

// share gives each profiled trace the samples carrying its label, and each the samples the profiler lost.
func (p *profiler) share(parsed *cpuProfile) {
	var lost int64
	for _, sample := range parsed.samples {
		if len(sample.stack) > 0 && lostSample(sample.stack[0].function) {
			lost += sample.count
			continue
		}
		if tp := p.traces[sample.trace]; tp != nil && sample.trace != "" {
			tp.add(sample)
		}
	}
	for _, tp := range p.traces {
		tp.dropped += lost
	}
}

func lostSample(function string) bool {
	return function == "runtime/pprof.lostProfileEvent" || strings.HasPrefix(function, "runtime._Lost")
}

// wait cuts now what waits for a cut, and waits for the profiles being cut, at most until the deadline: Flush.
func (p *profiler) wait(timeout time.Duration) {
	p.mu.Lock()
	if !p.cutting && len(p.ending) > 0 {
		p.disarm()
		p.cutting = true
		go p.cut()
	}
	if p.pending == 0 {
		p.mu.Unlock()
		return
	}
	if p.drained == nil {
		p.drained = make(chan struct{})
	}
	drained := p.drained
	p.mu.Unlock()
	select {
	case <-drained:
	case <-time.After(timeout):
	}
}

// add counts a sample of the trace: its stack, outermost frame first, without Tracepanel's own frames.
func (tp *traceProfile) add(sample cpuSample) {
	keys := make([]int, 0, len(sample.stack))
	for i := len(sample.stack) - 1; i >= 0 && len(keys) < maxProfileDepth; i-- {
		frame := sample.stack[i]
		if frame.function == "runtime.goexit" || ownFrame(frame) {
			continue
		}
		keys = append(keys, tp.frameIndex(frame))
	}
	if len(keys) == 0 {
		return
	}
	key := stackKey(keys)
	stack := tp.stacks[key]
	if stack == nil {
		if len(tp.stacks) >= maxProfileStacks {
			keys = []int{tp.frameIndex(cpuFrame{function: "(other)"})}
			key = stackKey(keys)
			stack = tp.stacks[key]
		}
		if stack == nil {
			stack = &profileStack{Frames: keys}
			tp.stacks[key] = stack
		}
	}
	stack.Count += sample.count
	tp.samples += sample.count
}

func (tp *traceProfile) frameIndex(frame cpuFrame) int {
	// One frame per function, wherever in it a sample was.
	frame.line = 0
	if at, ok := tp.index[frame]; ok {
		return at
	}
	tp.index[frame] = len(tp.frames)
	tp.frames = append(tp.frames, profileFrameOf(frame))
	return len(tp.frames) - 1
}

func stackKey(keys []int) string {
	var b strings.Builder
	for i, key := range keys {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(strconv.Itoa(key))
	}
	return b.String()
}

// document is the trace's `profile`: its stacks, the most seen first, and only the frames they name. A trace no
// sample saw (it took less CPU than a sample stands for) has one with no stacks: its 0 samples say why.
func (tp *traceProfile) document(parsed *cpuProfile) *profileDoc {
	interval := int64(defaultIntervalUs)
	if parsed != nil && parsed.periodNs >= 1000 {
		interval = parsed.periodNs / 1000
	}
	stacks := make([]*profileStack, 0, len(tp.stacks))
	for _, stack := range tp.stacks {
		stacks = append(stacks, stack)
	}
	sort.SliceStable(stacks, func(i, j int) bool {
		if stacks[i].Count != stacks[j].Count {
			return stacks[i].Count > stacks[j].Count
		}
		return stackKey(stacks[i].Frames) < stackKey(stacks[j].Frames)
	})
	doc := &profileDoc{IntervalUs: interval, Samples: tp.samples, Dropped: tp.dropped, Frames: []profileFrame{}, Stacks: []profileStack{}}
	renumber := map[int]int{}
	for _, stack := range stacks {
		frames := make([]int, len(stack.Frames))
		for i, frame := range stack.Frames {
			to, ok := renumber[frame]
			if !ok {
				to = len(doc.Frames)
				renumber[frame] = to
				doc.Frames = append(doc.Frames, tp.frames[frame])
			}
			frames[i] = to
		}
		doc.Stacks = append(doc.Stacks, profileStack{Frames: frames, Count: stack.Count})
	}
	return doc
}

// ownFrame: Tracepanel's own code (the core and its integrations), but for its tests, which play the app.
func ownFrame(frame cpuFrame) bool {
	module, _ := splitFunction(frame.function)
	if module != modulePath && !strings.HasPrefix(module, modulePath+"/internal/") &&
		!(strings.HasPrefix(module, modulePath+"/") && strings.HasSuffix(module, "trace")) {
		return false
	}
	return !strings.HasSuffix(frame.file, "_test.go")
}

// profileFrameOf is a function as the profile names it: its package as the module, the file as the project sees it.
func profileFrameOf(frame cpuFrame) profileFrame {
	if frame.file == "" {
		return profileFrame{Function: frame.function, File: "", Line: 0}
	}
	module, function := splitFunction(frame.function)
	own := inApp(frame.function, frame.file)
	file := libraryPath(frame.file, module)
	if own {
		file = relative(frame.file)
	}
	return profileFrame{Function: function, Module: &module, File: file, Line: frame.start, InApp: own}
}

// libraryPath is a library's file from its module on (github.com/jackc/pgx/v5@v5.7.1/conn.go), the standard
// library's from its package (net/http/server.go): a package whose path has no domain is the standard library's.
func libraryPath(file, module string) string {
	if at := strings.Index(file, "/pkg/mod/"); at >= 0 {
		return file[at+len("/pkg/mod/"):]
	}
	if first, _, _ := strings.Cut(module, "/"); module != "" && !strings.Contains(first, ".") {
		if at := strings.LastIndex(file, "/src/"+module+"/"); at >= 0 {
			return file[at+len("/src/"):]
		}
	}
	return file
}
