// Package tracepanel is Tracepanel for Go: it records each request of an app (its SQL with the line of code that
// ran each statement, its logs, its outgoing HTTP calls, its panics) and sends it to the Tracepanel agent, which
// finds the issues (N+1 queries, duplicates, slow SQL), keeps the traces in .tracepanel/ and serves the console. The
// middleware adds what the badge on a page reads (Server-Timing, Tracepanel-Id) and puts the badge into HTML pages.
//
//	db, err := tracepanel.Open("pgx", dsn) // in place of sql.Open
//	http.ListenAndServe(addr, tracepanel.Wrap(mux))
//
// It has no dependencies, and records nothing until Wrap or Setup turns it on.
package tracepanel

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"runtime/debug"
	"strconv"
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
	// The most traces the agent keeps for the project (2000).
	MaxTraces int
	// The route of a request, for a router that doesn't set Request.Pattern as ServeMux does: "/books/{id}".
	Route func(*http.Request) string
}

// Tracepanel records an app's traces and sends them to its agent.
type Tracepanel struct {
	config  Config
	starter *starter
	console *console
	sender  *sender
	// Where an ended trace goes: the sender's queue.
	deliver func(*traceDoc)
}

var (
	active    atomic.Pointer[Tracepanel]
	setupOnce sync.Mutex
)

// Setup turns Tracepanel on for the process, once: the standard slog logger's records and the default HTTP
// client's calls join the traces. Wrap calls it. A program without an HTTP server (a worker, a command) calls it
// itself, and Flush before it exits. TRACEPANEL_ENABLED=0 leaves Tracepanel off: it returns nil.
func Setup(config ...Config) *Tracepanel {
	setupOnce.Lock()
	defer setupOnce.Unlock()
	if t := active.Load(); t != nil {
		return t
	}
	if off(os.Getenv("TRACEPANEL_ENABLED")) {
		return nil
	}
	var settings Config
	if len(config) > 0 {
		settings = config[0]
	}
	t := newTracepanel(withDefaults(settings))
	active.Store(t)
	installLogs()
	installTransport()
	return t
}

func off(value string) bool {
	switch value {
	case "0", "false", "False", "FALSE", "off", "no":
		return true
	}
	return false
}

func newTracepanel(config Config) *Tracepanel {
	start := &starter{storeDir: config.StoreDir}
	t := &Tracepanel{
		config:  config,
		starter: start,
		console: &console{configured: config.ConsoleURL, storeDir: config.StoreDir},
	}
	t.sender = newSender(start, config)
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

// thresholds are the limits the agent finds the issues with, those the environment sets: the agent's own otherwise.
func thresholds() map[string]int {
	out := map[string]int{}
	for key, variable := range map[string]string{
		"n_plus_one":          "TRACEPANEL_N_PLUS_ONE_THRESHOLD",
		"slow_sql_ms":         "TRACEPANEL_SLOW_SQL_MS",
		"long_transaction_ms": "TRACEPANEL_LONG_TRANSACTION_MS",
		"slow_http_ms":        "TRACEPANEL_SLOW_HTTP_MS",
		"large_result_rows":   "TRACEPANEL_LARGE_RESULT_ROWS",
	} {
		if value, err := strconv.Atoi(os.Getenv(variable)); err == nil && value > 0 {
			out[key] = value
		}
	}
	return out
}

// Flush waits until the traces recorded so far went to the agent, or `timeout` passed: a command calls it before
// it exits.
func Flush(timeout time.Duration) {
	if t := active.Load(); t != nil && t.sender != nil {
		t.sender.flush(timeout)
	}
}

// roles: what a process that records this kind of trace is, on the service map.
var roles = map[string]string{"http": "web", "task": "worker"}

// begin starts a trace of the process, with what every trace says about the app that recorded it.
func (t *Tracepanel) begin(kind, rootKind, name string, attrs Attrs) *recording {
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
	return newRecording(kind, rootKind, name, attrs)
}

// finish sends an ended trace to the agent.
func (t *Tracepanel) finish(rec *recording) {
	t.deliver(rec.finish())
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
	span := s.rec.begin(s.span, "function", name, Attrs{"code.origin": nilIfEmpty(origin())})
	return within(ctx, s.rec, span.ID), func() { s.rec.end(span, false) }
}

// Command records fn as a trace of its own: a scheduled job, a CLI command, a migration. An error it returns, or a
// panic, fails the trace (the panic goes on).
func Command(ctx context.Context, name string, fn func(context.Context) error) error {
	t := active.Load()
	if t == nil {
		return fn(ctx)
	}
	rec := t.begin("command", "command", name, Attrs{"command.trigger": "other"})
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
	attrs := Attrs{"task.system": "go", "task.name": name, "task.trigger": "other"}
	var started *linkDoc
	if s := scopeOf(ctx); s != nil {
		span := s.rec.done(s.span, newID(), "task.enqueue", name,
			Attrs{"task.system": "go", "task.name": name, "code.origin": nilIfEmpty(origin())}, time.Now(), false)
		started = &linkDoc{Kind: "enqueued_by", TraceID: s.rec.doc.ID, SpanID: span.ID}
		attrs["task.trigger"] = "request"
		if role, ok := s.rec.attr("service.role").(string); ok {
			attrs["service.role"] = role
		}
	}
	go func() {
		rec := t.begin("task", "task.run", name, attrs)
		if started != nil {
			rec.link(started.Kind, started.TraceID, started.SpanID)
		}
		_ = t.run(detached, rec, fn)
	}()
}

// run runs fn in its trace, and ends the trace: an error or a panic fails it.
func (t *Tracepanel) run(ctx context.Context, rec *recording, fn func(context.Context) error) (err error) {
	defer func() {
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
	slog.SetDefault(slog.New(LogHandler(next)))
}
