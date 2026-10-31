package tracepanel

import (
	"runtime/debug"
	"runtime/metrics"
	"sync"
	"sync/atomic"
	"time"
)

// What a profiled trace's memory and the garbage collector did while it ran: the profile's `memory` and `gc`.
//
// Go's heap is the process's, and none of what the runtime tells of it (runtime/metrics, its heap profile) can be
// told per goroutine: the heap profile's samples carry no labels, and its allocations show only after the collections
// that follow them. So a trace's memory is the heap's, while it ran, as the other SDKs' is (Python's tracemalloc,
// Node's heap): exactly the trace's when it ran alone, and marked `shared` when another trace ran meanwhile, whose
// allocations are in there too. Only for a trace that asks for it ("memory" in X-Tracepanel-Profile or the panel's
// cookie, or TRACEPANEL_PROFILE_MEMORY=1 for every profiled one):
//
//   - allocated: the bytes the heap allocated while the trace ran (/gc/heap/allocs:bytes), freed or not. Go counts
//     them as its allocator hands out spans of memory, so a few kilobytes either way.
//   - peak: the heap's highest level while the trace ran, above its level at the start
//     (/memory/classes/heap/objects:bytes, read every 10 ms and at the end). Garbage counts until it is collected.
//   - net: the heap's level at the end less at the start, garbage counted until collected: negative where a
//     collection freed more meanwhile.
//
// No allocation sites: the heap profile samples one allocation in 512 KB and publishes them only after a collection,
// so a request's would need a collection at its start and its end, and would still be the process's.
//
// The collector's runs (`gc`) are in every profile: how many ended while the trace ran and how long the program
// stood still for them (runtime/debug's pause history), each pause [from the trace's start, how long, 2: Go collects
// the whole heap]. A pause stops every goroutine, so it is the trace's however many ran.

type profileMemory struct {
	Peak      int64 `json:"peak"`
	Net       int64 `json:"net"`
	Allocated int64 `json:"allocated"`
	Shared    bool  `json:"shared"`
}

type profileGC struct {
	Collections int        `json:"collections"`
	PauseUs     int64      `json:"pause_us"`
	Pauses      [][3]int64 `json:"pauses"`
}

// How often the heap's level is read while a trace's memory is measured, for its peak.
const heapEvery = 10 * time.Millisecond

const (
	allocsMetric = "/gc/heap/allocs:bytes"
	heapMetric   = "/memory/classes/heap/objects:bytes"
)

// traces counts the traces running and those begun, for `shared`: another trace ran while one did.
var traces struct {
	running atomic.Int64
	begun   atomic.Int64
}

// heapLevels reads what the heap allocated in all, and its level now.
func heapLevels() (allocated, level int64) {
	samples := []metrics.Sample{{Name: allocsMetric}, {Name: heapMetric}}
	metrics.Read(samples)
	if samples[0].Value.Kind() == metrics.KindUint64 {
		allocated = int64(samples[0].Value.Uint64())
	}
	if samples[1].Value.Kind() == metrics.KindUint64 {
		level = int64(samples[1].Value.Uint64())
	}
	return allocated, level
}

// memoryWindow is a trace's memory while it runs: where the heap stood at its start, its highest level since.
type memoryWindow struct {
	allocated, level int64
	peak             atomic.Int64
	alone            bool
	begun            int64
}

// heapWatch reads the heap's level every heapEvery while a trace's memory is measured, for their peaks.
var heapWatch struct {
	mu      sync.Mutex
	windows map[*memoryWindow]bool
	stop    chan struct{}
}

// measureMemory begins measuring a trace's memory, until its end.
func measureMemory() *memoryWindow {
	w := &memoryWindow{}
	w.allocated, w.level = heapLevels()
	w.peak.Store(w.level)
	// Others running now, or begun before it ends: shared.
	w.alone = traces.running.Load() <= 1
	w.begun = traces.begun.Load()
	heapWatch.mu.Lock()
	defer heapWatch.mu.Unlock()
	if heapWatch.windows == nil {
		heapWatch.windows = map[*memoryWindow]bool{}
	}
	heapWatch.windows[w] = true
	if heapWatch.stop == nil {
		heapWatch.stop = make(chan struct{})
		go watchHeap(heapWatch.stop)
	}
	return w
}

func watchHeap(stop chan struct{}) {
	ticker := time.NewTicker(heapEvery)
	defer ticker.Stop()
	samples := []metrics.Sample{{Name: heapMetric}}
	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
		}
		metrics.Read(samples)
		if samples[0].Value.Kind() != metrics.KindUint64 {
			continue
		}
		level := int64(samples[0].Value.Uint64())
		heapWatch.mu.Lock()
		for w := range heapWatch.windows {
			w.raise(level)
		}
		heapWatch.mu.Unlock()
	}
}

func (w *memoryWindow) raise(level int64) {
	for {
		peak := w.peak.Load()
		if level <= peak || w.peak.CompareAndSwap(peak, level) {
			return
		}
	}
}

// end is the trace's memory, as it ends.
func (w *memoryWindow) end() *profileMemory {
	allocated, level := heapLevels()
	w.raise(level)
	heapWatch.mu.Lock()
	delete(heapWatch.windows, w)
	if len(heapWatch.windows) == 0 && heapWatch.stop != nil {
		close(heapWatch.stop)
		heapWatch.stop = nil
	}
	heapWatch.mu.Unlock()
	shared := !w.alone || traces.begun.Load() != w.begun || traces.running.Load() > 1
	return &profileMemory{
		Peak: max(w.peak.Load()-w.level, 0), Net: level - w.level, Allocated: max(allocated-w.allocated, 0), Shared: shared,
	}
}

// gcWindow is where the collector stood when a trace began: its collections so far.
type gcWindow struct {
	start time.Time
	count int64
}

func watchGC() gcWindow {
	var stats debug.GCStats
	debug.ReadGCStats(&stats)
	return gcWindow{start: time.Now(), count: stats.NumGC}
}

// end is the collector's runs since the trace began: those whose pauses runtime/debug still has (the last 256).
func (g gcWindow) end() *profileGC {
	var stats debug.GCStats
	debug.ReadGCStats(&stats)
	doc := &profileGC{Pauses: [][3]int64{}}
	ran := int(stats.NumGC - g.count)
	doc.Collections = ran
	for i := min(ran, len(stats.Pause), len(stats.PauseEnd)) - 1; i >= 0; i-- {
		pause := stats.Pause[i]
		at := stats.PauseEnd[i].Add(-pause).Sub(g.start)
		doc.Pauses = append(doc.Pauses, [3]int64{max(at.Microseconds(), 0), pause.Microseconds(), 2})
		doc.PauseUs += pause.Microseconds()
	}
	return doc
}
