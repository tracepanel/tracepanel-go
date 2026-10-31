// Package tracepanel is Tracepanel for Go: it records each request of an app (its SQL with the line of code that
// ran each statement, its logs, its outgoing HTTP calls, its panics) and sends it to the Tracepanel agent, which
// finds the issues (N+1 queries, duplicates, slow SQL), keeps the traces in .tracepanel/ and serves the console. The
// middleware adds what the badge on a page reads (Server-Timing, Tracepanel-Id) and puts the badge into HTML pages.
//
//	db, err := sql.Open(tracepanel.Driver("pgx"), dsn)
//	http.ListenAndServe(addr, tracepanel.WrapHandler(mux))
//
// It has no dependencies, and records nothing until WrapHandler or Setup turns it on, in development only: under
// `go run`, with TRACEPANEL_ENABLED=1 or Config.Enabled; a binary `go build` wrote records nothing, and
// TRACEPANEL_ENABLED=0 always wins. The agent comes with it: the
// first trace starts the one the SDK downloads (once, into the user's cache). Its MCP server, for AI coding agents,
// runs with
//
//	go run github.com/tracepanel/tracepanel-go/cmd/tracepanel mcp
package tracepanel

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"runtime/debug"
	"runtime/pprof"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Config is optional: what it leaves empty comes from TRACEPANEL_* variables, then defaults.
type Config struct {
	// The app's name in the console: TRACEPANEL_SERVICE, else the executable's name.
	Service string
	// Where the agent serves the console: the badge loads from there and opens it. TRACEPANEL_CONSOLE_URL, else the
	// address the project's agent writes (console.addr), on whatever port it got.
	ConsoleURL string
	// The project's store directory, where the agent keeps the traces: TRACEPANEL_DIR, else .tracepanel.
	StoreDir string
	// The app's module path: frames under it are the project's code. The main module's, read from the build.
	Module string
	// The most traces the agent keeps for the project: TRACEPANEL_MAX_TRACES, else 2000.
	MaxTraces int
	// The route of a request, for a router that doesn't set Request.Pattern as ServeMux does: "/books/{id}".
	Route func(*http.Request) string
	// Record in this build, not only under `go run` or with TRACEPANEL_ENABLED=1: a staging build, say. Set up
	// before Driver, so the database is recorded too. TRACEPANEL_ENABLED=0 still turns Tracepanel off.
	Enabled bool
	// Profile every trace (where its CPU time went, a flame graph in the console), not only a request that asks
	// with the panel's Profile toggle: TRACEPANEL_PROFILE=1. TRACEPANEL_PROFILE_ENABLED=0 profiles nothing.
	Profile bool
	// Measure every profiled trace's memory (what the heap allocated while it ran), not only a request's that asks
	// for "memory": TRACEPANEL_PROFILE_MEMORY=1.
	ProfileMemory bool
}

// tracepanel records an app's traces and sends them to its agent.
type tracepanel struct {
	config  Config
	starter *starter
	console *console
	sender  *sender
	// Where an ended trace goes: the sender's queue.
	deliver func(*traceDoc)
	// The CPU profiler of profiled traces: nil when profiling is off (TRACEPANEL_PROFILE_ENABLED=0).
	profiler *profiler
	// Every trace is profiled (Config.Profile, TRACEPANEL_PROFILE=1), not only those asked to be; and every profiled
	// trace's memory measured (Config.ProfileMemory, TRACEPANEL_PROFILE_MEMORY=1), not only those asked for it.
	profileAll    bool
	profileMemory bool
	// A request's headers, query, cookies and body are kept (TRACEPANEL_CAPTURE_REQUEST=0: not).
	captureRequest bool
}

var (
	active    atomic.Pointer[tracepanel]
	setupOnce sync.Mutex
)

// Setup turns Tracepanel on for the process, once: the standard slog logger's records and the default HTTP
// client's calls join the traces. WrapHandler and Middleware call it. A program without an HTTP server (a worker, a
// command) calls it itself, and Flush before it exits. It turns Tracepanel on in development only: under `go run`,
// with TRACEPANEL_ENABLED=1 or with Config.Enabled; TRACEPANEL_ENABLED=0 leaves it off whatever else says.
func Setup(config ...Config) {
	setup(config...)
}

// setup is Setup, with the Tracepanel it turned on: nil when it is off.
func setup(config ...Config) *tracepanel {
	setupOnce.Lock()
	defer setupOnce.Unlock()
	if t := active.Load(); t != nil {
		return t
	}
	var settings Config
	if len(config) > 0 {
		settings = config[0]
	}
	if !wanted(settings) {
		return nil
	}
	t := newTracepanel(withDefaults(settings))
	active.Store(t)
	installLogs()
	installTransport()
	return t
}

func on(value string) bool {
	switch value {
	case "1", "true", "True", "TRUE", "on", "yes":
		return true
	}
	return false
}

func off(value string) bool {
	switch value {
	case "0", "false", "False", "FALSE", "off", "no":
		return true
	}
	return false
}

func newTracepanel(config Config) *tracepanel {
	start := &starter{storeDir: config.StoreDir}
	t := &tracepanel{
		config:         config,
		starter:        start,
		console:        &console{configured: config.ConsoleURL, storeDir: config.StoreDir},
		profileAll:     config.Profile,
		profileMemory:  config.ProfileMemory,
		captureRequest: !off(os.Getenv("TRACEPANEL_CAPTURE_REQUEST")),
	}
	if !off(os.Getenv("TRACEPANEL_PROFILE_ENABLED")) {
		t.profiler = newProfiler()
	}
	t.sender = newSender(start, config)
	// The console offers what the app does: the Request tab, recording a request with a profile (with its memory
	// too: memory.go), profiling as on.
	if t.captureRequest {
		t.sender.capabilities = append(t.sender.capabilities, "request")
	}
	if t.profiler != nil {
		t.sender.capabilities = append(t.sender.capabilities, "profile", "profile_memory")
		if t.profileAll {
			t.sender.capabilities = append(t.sender.capabilities, "profile_all")
		}
		if t.profileMemory {
			t.sender.capabilities = append(t.sender.capabilities, "profile_memory_all")
		}
	}
	t.deliver = t.sender.submit
	return t
}

func withDefaults(config Config) Config {
	if config.Service == "" {
		config.Service = envOr("TRACEPANEL_SERVICE", filepath.Base(os.Args[0]))
	}
	if config.ConsoleURL == "" {
		config.ConsoleURL = os.Getenv("TRACEPANEL_CONSOLE_URL")
	}
	if !config.Profile {
		config.Profile = on(os.Getenv("TRACEPANEL_PROFILE"))
	}
	if !config.ProfileMemory {
		config.ProfileMemory = on(os.Getenv("TRACEPANEL_PROFILE_MEMORY"))
	}
	if config.StoreDir == "" {
		config.StoreDir = envOr("TRACEPANEL_DIR", ".tracepanel")
	}
	if config.Module == "" {
		if info, ok := debug.ReadBuildInfo(); ok {
			config.Module = info.Main.Path
		}
	}
	if config.MaxTraces == 0 {
		config.MaxTraces = 2000
		if value, err := strconv.Atoi(os.Getenv("TRACEPANEL_MAX_TRACES")); err == nil && value > 0 {
			config.MaxTraces = value
		}
	}
	appModule.Store(&config.Module)
	return config
}

func envOr(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}

// thresholdNames are the agent's limits, by the variable that sets each.
var thresholdNames = map[string]string{
	"TRACEPANEL_N_PLUS_ONE_THRESHOLD": "n_plus_one",
	"TRACEPANEL_N_PLUS_ONE_MIN_MS":    "n_plus_one_min_ms",
	"TRACEPANEL_SLOW_SQL_MS":          "slow_sql_ms",
	"TRACEPANEL_LONG_TRANSACTION_MS":  "long_transaction_ms",
	"TRACEPANEL_SLOW_HTTP_MS":         "slow_http_ms",
	"TRACEPANEL_QUEUE_WAIT_MS":        "queue_wait_ms",
	"TRACEPANEL_LARGE_RESULT_ROWS":    "large_result_rows",
}

// The limits that may be set per database too: `100,snowflake=3000`.
var byDatabase = map[string]bool{"slow_sql_ms": true, "long_transaction_ms": true}

func thresholdVariables() []string {
	names := make([]string, 0, len(thresholdNames))
	for name := range thresholdNames {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// thresholds are the limits the agent finds the issues with, those the environment sets: the agent's own otherwise.
// TRACEPANEL_SLOW_SQL_MS and TRACEPANEL_LONG_TRANSACTION_MS may set one per database too, by its db.system:
// `100,snowflake=3000` is 100 ms for every database but Snowflake's, held to 3 s (slow_sql_ms_by_db). A value that
// isn't a whole number is left out.
func thresholds() map[string]any {
	out := map[string]any{}
	for variable, key := range thresholdNames {
		for _, entry := range strings.Split(os.Getenv(variable), ",") {
			system, value, perDatabase := strings.Cut(strings.TrimSpace(entry), "=")
			if !perDatabase {
				value = system
			}
			number, err := strconv.Atoi(strings.TrimSpace(value))
			if err != nil || number < 0 {
				continue
			}
			if !perDatabase {
				out[key] = number
				continue
			}
			system = strings.ToLower(strings.TrimSpace(system))
			if !byDatabase[key] || system == "" {
				continue
			}
			systems, _ := out[key+"_by_db"].(map[string]int)
			if systems == nil {
				systems = map[string]int{}
				out[key+"_by_db"] = systems
			}
			systems[system] = number
		}
	}
	return out
}

// Flush waits until the traces recorded so far went to the agent, or `timeout` passed: a command calls it before
// it exits.
func Flush(timeout time.Duration) {
	t := active.Load()
	if t == nil {
		return
	}
	deadline := time.Now().Add(timeout)
	if t.profiler != nil {
		t.profiler.wait(timeout)
	}
	if t.sender != nil {
		t.sender.flush(time.Until(deadline))
	}
}

// roles: what a process that records this kind of trace is, on the service map.
var roles = map[string]string{"http": "web", "task": "worker"}

// begin starts a trace of the process, with what every trace says about the app that recorded it.
func (t *tracepanel) begin(kind, rootKind, name string, attrs attrMap) *recording {
	role, _ := attrs["service.role"].(string)
	if role == "" {
		role = roles[kind]
		if role == "" {
			role = kind
		}
	}
	attrs["service.name"] = t.config.Service
	attrs["service.role"] = role
	attrs["telemetry.sdk.language"] = "go"
	attrs["process.runtime.name"] = "go"
	attrs["process.runtime.version"] = goVersion()
	attrs["tracepanel.language.version"] = goVersion()
	traces.running.Add(1)
	traces.begun.Add(1)
	return newRecording(kind, rootKind, name, attrs)
}

// finish sends an ended trace to the agent; a profiled one once its profile is cut.
func (t *tracepanel) finish(rec *recording) {
	doc := rec.finish()
	traces.running.Add(-1)
	if rec.profile == nil {
		t.deliver(doc)
		return
	}
	var gc *profileGC
	var memory *profileMemory
	if rec.gc != nil {
		gc = rec.gc.end()
	}
	if rec.memory != nil {
		memory = rec.memory.end()
	}
	t.profiler.end(rec.profile, func(profile *profileDoc, gap time.Duration) {
		if profile != nil {
			profile.GC, profile.Memory = gc, memory
			doc.Profile = profile
			if profile.Dropped > 0 {
				doc.Attrs["tracepanel.dropped_samples"] = profile.Dropped
			}
			if gap >= time.Millisecond {
				// The profiler was stopped (for another trace's profile) this long while the trace ran.
				doc.Attrs["tracepanel.profile_gap_ms"] = gap.Milliseconds()
			}
		}
		t.deliver(doc)
	})
}

// profileReason is why a trace is profiled, its root's profile.reason: what asked for it ("header", "cookie",
// "parent"), else "setting" when every trace is; "" when it isn't profiled. tracepanel.Profile's is "code".
func (t *tracepanel) profileReason(rec *recording) string {
	switch {
	case t.profiler == nil:
		return ""
	case rec.asked != "" && rec.reason != "":
		return rec.reason
	case rec.asked != "":
		return "header"
	case t.profileAll:
		return "setting"
	}
	return ""
}

// profile starts the trace's profile when it is to be profiled: the current goroutine, and those it starts, carry
// its label until restore. One that isn't profiled carries no trace's label, not even the one its creator had.
func (t *tracepanel) profile(ctx context.Context, rec *recording) (context.Context, func()) {
	if t.profiler == nil {
		return ctx, func() {}
	}
	labeled := pprof.WithLabels(ctx, pprof.Labels(profileLabel, ""))
	if reason := t.profileReason(rec); reason != "" {
		labeled, rec.profile = t.profiler.start(ctx, rec.doc.ID)
		rec.gc, rec.memory = t.watch(rec.asked)
		rec.set("profile.reason", reason)
	} else if _, carried := pprof.Label(ctx, profileLabel); !carried {
		// Unless tracepanel.Profile labels the goroutine later on: then it gets its labels back too.
		return ctx, func() {
			if rec.profiledByCode() {
				pprof.SetGoroutineLabels(ctx)
			}
		}
	}
	pprof.SetGoroutineLabels(labeled)
	return labeled, func() { pprof.SetGoroutineLabels(ctx) }
}

// watch begins watching the collector for a trace being profiled, and its memory when it asked for it ("memory") or
// every profiled trace's is measured (TRACEPANEL_PROFILE_MEMORY=1).
func (t *tracepanel) watch(asked string) (*gcWindow, *memoryWindow) {
	gc := watchGC()
	if asked == "memory" || t.profileMemory {
		return &gc, measureMemory()
	}
	return &gc, nil
}

// Profile profiles the trace in ctx from here on, as the panel's Profile toggle profiles a request from its start:
// where its CPU time goes from now until it ends, a flame graph in the console (profile.reason "code"). The current
// goroutine is profiled; a goroutine the code starts for the trace is too when started with the context it returns.
//
//	ctx = tracepanel.Profile(ctx)
//
// A trace profiled already (asked for, or TRACEPANEL_PROFILE=1) stays as it is; outside a trace, or with profiling
// off (TRACEPANEL_PROFILE_ENABLED=0), it returns ctx.
func Profile(ctx context.Context) context.Context {
	t, s := active.Load(), scopeOf(ctx)
	if t == nil || t.profiler == nil || s == nil {
		return ctx
	}
	rec := s.rec
	rec.mu.Lock()
	if rec.finished || rec.profile != nil {
		rec.mu.Unlock()
		return ctx
	}
	labeled, tp := t.profiler.start(ctx, rec.doc.ID)
	rec.profile, rec.byCode = tp, true
	rec.gc, rec.memory = t.watch(rec.asked)
	rec.doc.Attrs["profile.reason"] = "code"
	if rec.asked == "" {
		// The calls it makes and the tasks it starts from now on ask the same.
		rec.asked = "1"
	}
	rec.mu.Unlock()
	pprof.SetGoroutineLabels(labeled)
	return labeled
}

// ---------- the app's own work ----------

// Span records a piece of the app's own code as a span of the trace in ctx, until the function it returns is called.
// Work done with the context it returns goes under it:
//
//	ctx, end := tracepanel.Span(ctx, "render invoice")
//	defer end()
func Span(ctx context.Context, name string) (context.Context, func()) {
	s := scopeOf(ctx)
	if s == nil {
		return ctx, func() {}
	}
	span := s.rec.begin(s.parent(), "function", name, attrMap{"code.origin": nilIfEmpty(origin())})
	return within(ctx, s.rec, span.ID), func() { s.rec.end(span, false) }
}

// SetUser names the signed-in user on the trace in ctx, as the app's authentication knows them: their id
// (user.id) and their username (user.username, left out when empty). The username stays on this machine: the agent
// drops it from what leaves it.
//
//	tracepanel.SetUser(r.Context(), strconv.FormatInt(user.ID, 10), user.Email)
func SetUser(ctx context.Context, id, username string) {
	s := scopeOf(ctx)
	if s == nil {
		return
	}
	s.rec.set("user.id", id)
	if username != "" {
		s.rec.set("user.username", username)
	}
}

// Command records fn as a trace of its own: a scheduled job, a CLI command, a migration. An error it returns, or a
// panic, fails the trace (the panic goes on).
func Command(ctx context.Context, name string, fn func(context.Context) error) error {
	t := active.Load()
	if t == nil {
		return fn(ctx)
	}
	rec := t.begin("command", "command", name, attrMap{"command.trigger": "other"})
	return t.run(ctx, rec, fn)
}

// Go runs fn on a goroutine of its own as a task: a trace of its own, linked to the trace in ctx that started it,
// where the start is a span. Its context keeps ctx's values but isn't canceled with it, so a task outlives the
// request that started it.
func Go(ctx context.Context, name string, fn func(context.Context) error) {
	detached := context.WithoutCancel(ctx)
	t := active.Load()
	if t == nil {
		go func() { _ = fn(detached) }()
		return
	}
	attrs := attrMap{"task.system": "go", "task.name": name, "task.trigger": "other"}
	var started *linkDoc
	asked := ""
	if s := scopeOf(ctx); s != nil {
		// A task of a profiled request is profiled too, as the other SDKs' tasks are.
		asked = s.rec.askedNow()
		span := s.rec.done(s.parent(), newID(), "task.enqueue", name,
			attrMap{"task.system": "go", "task.name": name, "code.origin": nilIfEmpty(origin())}, time.Now(), false)
		started = &linkDoc{Kind: "enqueued_by", TraceID: s.rec.doc.ID, SpanID: span.ID}
		attrs["task.trigger"] = "request"
		if role, ok := s.rec.attr("service.role").(string); ok {
			attrs["service.role"] = role
		}
	}
	go func() {
		rec := t.begin("task", "task.run", name, attrs)
		rec.asked = asked
		if asked != "" {
			rec.reason = "parent"
		}
		if started != nil {
			rec.link(started.Kind, started.TraceID, started.SpanID)
		}
		if t.profiler != nil {
			// The goroutine was started with its request's labels: the task's samples are its own, not the request's.
			pprof.SetGoroutineLabels(pprof.WithLabels(detached, pprof.Labels(profileLabel, "")))
		}
		_ = t.run(detached, rec, fn)
	}()
}

// run runs fn in its trace, and ends the trace: an error or a panic fails it.
func (t *tracepanel) run(ctx context.Context, rec *recording, fn func(context.Context) error) (err error) {
	ctx, restore := t.profile(ctx, rec)
	defer func() {
		restore()
		if recovered := recover(); recovered != nil {
			recordPanic(rec, recovered, callers(1))
			rec.fail()
			t.finish(rec)
			panic(recovered)
		}
		if err != nil {
			rec.addError(errorDoc{
				SpanID: ptr(rec.current()), Time: micros(time.Now()), Type: fmt.Sprintf("%T", err), Message: err.Error(),
				Frames: []frameDoc{}, Chain: []any{},
			})
			rec.fail()
		}
		t.finish(rec)
	}()
	return fn(within(ctx, rec, rec.root.ID))
}

func ptr(value string) *string { return &value }

func nilIfEmpty(text string) any {
	if text == "" {
		return nil
	}
	return text
}

// installLogs makes the standard slog logger's records (and the log package's, which go through it) join the
// trace of the context they were logged with.
func installLogs() {
	next := slog.Default().Handler()
	if fmt.Sprintf("%T", next) == "*slog.defaultHandler" {
		// slog's built-in handler writes through the log package, which SetDefault sends back to slog: wrapping it
		// would deadlock. The app hasn't chosen one, so the same text on stderr.
		next = slog.NewTextHandler(os.Stderr, nil)
	}
	slog.SetDefault(slog.New(WrapLogHandler(next)))
}
