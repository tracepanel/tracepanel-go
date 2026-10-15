// Package tracepanel is tracepanel for Go, on top of OpenTelemetry: the app traces itself with OTel (its HTTP server,
// database/sql through otelsql), and tracepanel's span processor turns each request's spans into a tracepanel trace,
// sent to the tracepanel agent, which finds its issues and serves the console. Its middleware adds what the badge
// reads (Server-Timing, Tracepanel-Id) and puts widget.min.js into HTML pages.
package tracepanel

import (
	"context"
	"fmt"
	"os"
	"runtime"
	"strings"
	"sync"
	"time"

	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
)

// This module's path: its own frames are never the app's.
const modulePath = "github.com/tracepanel/tracepanel-go"

type Config struct {
	// The project's name on the service map.
	Service string
	// Where the agent serves the console: the widget loads from there and opens it. Empty: the address the project's
	// agent writes (console.addr), on whatever port it got.
	ConsoleURL string
	// The project's store directory (.tracepanel), where the agent leaves its address.
	StoreDir string
	// The app's module path: frames under it are the project's code (code.origin, in_app).
	Module    string
	MaxTraces int
}

type Tracepanel struct {
	config  Config
	starter *starter
	console *console
	sender  *sender
	mu      sync.Mutex
	open    map[trace.TraceID]*building
}

// building is one request's trace while its spans end.
type building struct {
	trace   *Trace
	ids     map[trace.SpanID]string
	origins map[trace.SpanID]string
	queries int
	dbTime  int64
}

func New(config Config) *Tracepanel {
	if config.MaxTraces == 0 {
		config.MaxTraces = 2000
	}
	start := &starter{storeDir: config.StoreDir}
	return &Tracepanel{
		config:  config,
		starter: start,
		console: &console{configured: config.ConsoleURL, storeDir: config.StoreDir},
		sender:  newSender(start, config.MaxTraces),
		open:    map[trace.TraceID]*building{},
	}
}

// ---------- the span processor ----------

var _ sdktrace.SpanProcessor = (*Tracepanel)(nil)

func (t *Tracepanel) OnStart(_ context.Context, span sdktrace.ReadWriteSpan) {
	sc := span.SpanContext()
	t.mu.Lock()
	defer t.mu.Unlock()
	b := t.open[sc.TraceID()]
	if !span.Parent().IsValid() {
		if span.SpanKind() != trace.SpanKindServer {
			// Work outside a request (a background job): not traced yet.
			return
		}
		b = &building{
			trace: &Trace{ID: newID(), Kind: "http", Status: "ok", Attrs: Attrs{}, Spans: []Span{}, Logs: []Log{},
				Errors: []Error{}, Links: []any{}, Issues: []any{}},
			ids:     map[trace.SpanID]string{},
			origins: map[trace.SpanID]string{},
		}
		t.open[sc.TraceID()] = b
	}
	if b == nil {
		return
	}
	b.ids[sc.SpanID()] = newID()
	if origin := t.origin(); origin != "" {
		b.origins[sc.SpanID()] = origin
	}
}

func (t *Tracepanel) OnEnd(span sdktrace.ReadOnlySpan) {
	sc := span.SpanContext()
	t.mu.Lock()
	b := t.open[sc.TraceID()]
	if b == nil {
		t.mu.Unlock()
		return
	}
	root := !span.Parent().IsValid()
	if converted, ok := t.convert(b, span); ok {
		b.trace.Spans = append(b.trace.Spans, converted)
		if converted.Kind == "sql" {
			b.queries++
			b.dbTime += converted.Duration
		}
	}
	if !root {
		t.mu.Unlock()
		return
	}
	delete(t.open, sc.TraceID())
	t.mu.Unlock()
	t.finish(b, span)
	t.sender.submit(limited(b.trace))
}

func (t *Tracepanel) Shutdown(context.Context) error   { return nil }
func (t *Tracepanel) ForceFlush(context.Context) error { return nil }

func attrsOf(span sdktrace.ReadOnlySpan) Attrs {
	attrs := Attrs{}
	for _, kv := range span.Attributes() {
		attrs[string(kv.Key)] = kv.Value.AsInterface()
	}
	return attrs
}

// convert maps an OTel span: the request is the trace's root span, a statement an `sql` span, a handler a `view`;
// otelsql's bookkeeping spans (rows, connections) are left out.
func (t *Tracepanel) convert(b *building, span sdktrace.ReadOnlySpan) (Span, bool) {
	attrs := attrsOf(span)
	id := b.ids[span.SpanContext().SpanID()]
	var parent *string
	if span.Parent().IsValid() {
		parentID, ok := b.ids[span.Parent().SpanID()]
		if !ok {
			return Span{}, false
		}
		parent = &parentID
	}
	converted := Span{
		ID: id, ParentID: parent, Name: span.Name(), Kind: "function",
		Start:    micros(span.StartTime()),
		Duration: span.EndTime().Sub(span.StartTime()).Microseconds(),
		Status:   "ok",
		Attrs:    Attrs{},
	}
	if span.Status().Code == codes.Error {
		converted.Status = "error"
	}
	origin := b.origins[span.SpanContext().SpanID()]
	switch {
	case parent == nil:
		converted.Kind = "http"
	case attrs["tracepanel.kind"] == "view":
		converted.Kind = "view"
		converted.Attrs["code.function"] = attrs["code.function"]
	case statementOf(attrs) != "":
		sql := statementOf(attrs)
		converted.Kind = "sql"
		converted.Name = spanName(sql)
		converted.Attrs = Attrs{
			"db.client": "go", "db.system": attrs["db.system"], "db.vendor": attrs["db.system"], "db.alias": "default",
			"db.statement": statement(sql), "db.fingerprint": fingerprint(sql), "db.many": false,
			"code.origin": nilIfEmpty(origin),
		}
	case strings.HasPrefix(span.Name(), "sql."):
		return Span{}, false
	default:
		for key, value := range attrs {
			converted.Attrs[key] = value
		}
		converted.Attrs["code.origin"] = nilIfEmpty(origin)
	}
	return converted, true
}

func statementOf(attrs Attrs) string {
	for _, key := range []string{"db.query.text", "db.statement"} {
		if text, ok := attrs[key].(string); ok && text != "" {
			return text
		}
	}
	return ""
}

func nilIfEmpty(text string) any {
	if text == "" {
		return nil
	}
	return text
}

// finish fills the trace from its root span: name, timing, the request's attributes.
func (t *Tracepanel) finish(b *building, root sdktrace.ReadOnlySpan) {
	attrs := attrsOf(root)
	tr := b.trace
	tr.Name = root.Name()
	tr.Start = micros(root.StartTime())
	tr.Duration = root.EndTime().Sub(root.StartTime()).Microseconds()
	status, _ := attrs["http.response.status_code"].(int64)
	if status >= 500 || root.Status().Code == codes.Error {
		tr.Status = "error"
	}
	tr.Attrs = Attrs{
		"http.method":      attrs["http.request.method"],
		"http.path":        attrs["url.path"],
		"http.route":       attrs["http.route"],
		"http.status_code": status,
		"code.view":        attrs["code.function"],
		"service.name":     t.config.Service,
		"service.role":     "web",
		// What ran it, as OpenTelemetry names it (and tracepanel's other SDKs record it).
		"telemetry.sdk.language":  "go",
		"process.runtime.name":    "go",
		"process.runtime.version": goVersion(),
		"tracepanel.framework":    "net/http",
	}
}

// origin is `file:line in function` of the innermost frame of the app's own code calling into the current span.
func (t *Tracepanel) origin() string {
	pcs := make([]uintptr, 48)
	frames := runtime.CallersFrames(pcs[:runtime.Callers(3, pcs)])
	for {
		frame, more := frames.Next()
		if t.inApp(frame.Function) {
			return fmt.Sprintf("%s:%d in %s", t.relative(frame.File), frame.Line, shortFunction(frame.Function))
		}
		if !more {
			return ""
		}
	}
}

// inApp: the app's own code, its main package or its module's packages (tracepanel's own excepted).
func (t *Tracepanel) inApp(function string) bool {
	if strings.HasPrefix(function, modulePath+".") || strings.HasPrefix(function, modulePath+"/") {
		return false
	}
	return strings.HasPrefix(function, "main.") || strings.HasPrefix(function, t.config.Module+"/")
}

func (t *Tracepanel) relative(file string) string {
	if wd, err := os.Getwd(); err == nil {
		if rel, ok := strings.CutPrefix(file, wd+"/"); ok {
			return rel
		}
	}
	return file
}

func shortFunction(function string) string {
	if slash := strings.LastIndex(function, "/"); slash >= 0 {
		function = function[slash+1:]
	}
	if dot := strings.Index(function, "."); dot >= 0 {
		return function[dot+1:]
	}
	return function
}

// ---------- what the request adds to its trace ----------

// current is the trace being built for the request in ctx.
func (t *Tracepanel) current(ctx context.Context) *building {
	span := trace.SpanContextFromContext(ctx)
	if !span.IsValid() {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.open[span.TraceID()]
}

func (t *Tracepanel) spanID(b *building, ctx context.Context) *string {
	id, ok := b.ids[trace.SpanContextFromContext(ctx).SpanID()]
	if !ok {
		return nil
	}
	return &id
}

// timing is what the widget shows before the request's span ends: its queries so far and its time.
func (t *Tracepanel) timing(ctx context.Context, start time.Time) (id string, queries int, dbMs, totalMs float64) {
	b := t.current(ctx)
	if b == nil {
		return "", 0, 0, 0
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return b.trace.ID, b.queries, float64(b.dbTime) / 1000, float64(time.Since(start).Microseconds()) / 1000
}

// recordPanic adds an unhandled panic to the request's trace, with the stack it was raised in.
func (t *Tracepanel) recordPanic(ctx context.Context, recovered any, pcs []uintptr) {
	b := t.current(ctx)
	if b == nil {
		return
	}
	frames := []Frame{}
	callers := runtime.CallersFrames(pcs)
	// The stack seen from recover() starts with the deferred function and the panic machinery: from the frame below
	// runtime.gopanic on, it is where the panic happened.
	panicking := false
	for {
		frame, more := callers.Next()
		if panicking {
			module, function := splitFunction(frame.Function)
			inApp := t.inApp(frame.Function)
			start, context := sourceAround(frame.File, frame.Line, inApp)
			frames = append(frames, Frame{
				Filename: t.relative(frame.File), Module: module, Function: function, Lineno: frame.Line,
				InApp: inApp, ContextStart: start, Context: context,
			})
		}
		panicking = panicking || frame.Function == "runtime.gopanic"
		if !more {
			break
		}
	}
	// Outermost first, as the console shows every SDK's frames.
	for i, j := 0, len(frames)-1; i < j; i, j = i+1, j-1 {
		frames[i], frames[j] = frames[j], frames[i]
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	b.trace.Errors = append(b.trace.Errors, Error{
		SpanID: t.spanID(b, ctx), Time: micros(time.Now()), Type: fmt.Sprintf("panic(%T)", recovered),
		Message: fmt.Sprint(recovered), Frames: frames, Chain: []any{},
	})
}

func splitFunction(function string) (string, string) {
	slash := strings.LastIndex(function, "/")
	dot := strings.Index(function[slash+1:], ".")
	if dot < 0 {
		return function, function
	}
	return function[:slash+1+dot], function[slash+1+dot+1:]
}

// goVersion is "1.27.1" (runtime.Version() says "go1.27.1").
func goVersion() string { return strings.TrimPrefix(runtime.Version(), "go") }

// runtimeLabel is what the panel shows next to its logo: "Go 1.27 · net/http".
func runtimeLabel() string {
	parts := strings.SplitN(goVersion(), ".", 3)
	return "Go " + strings.Join(parts[:min(len(parts), 2)], ".") + " · net/http"
}

const contextLines = 5

// sourceAround is the source around a line of the app's own code, which the console shows with the frame.
func sourceAround(file string, line int, inApp bool) (*int, []string) {
	if !inApp {
		return nil, []string{}
	}
	data, err := os.ReadFile(file)
	if err != nil {
		return nil, []string{}
	}
	lines := strings.Split(string(data), "\n")
	start := max(line-contextLines, 1)
	end := min(line+contextLines, len(lines))
	return &start, lines[start-1 : end]
}
