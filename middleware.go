package tracepanel

import (
	"bytes"
	"context"
	"fmt"
	"html"
	"log/slog"
	"net/http"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

var tracer = otel.Tracer("tracepanel")

// Middleware traces every request as an OTel server span, and gives each response what the widget reads: the trace's
// id and timing in headers, and widget.min.js in an HTML page. The response is held until the handler returns (a JSON
// answer or a page; not for streaming).
func (t *Tracepanel) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		ctx, span := tracer.Start(r.Context(), r.Method+" "+r.URL.Path, trace.WithSpanKind(trace.SpanKindServer),
			trace.WithAttributes(attribute.String("http.request.method", r.Method), attribute.String("url.path", r.URL.Path)))
		req := r.WithContext(ctx)
		held := &heldResponse{header: http.Header{}, status: http.StatusOK}
		func() {
			defer func() {
				if recovered := recover(); recovered != nil {
					pcs := make([]uintptr, 64)
					t.recordPanic(ctx, recovered, pcs[:runtime.Callers(3, pcs)])
					span.SetStatus(codes.Error, fmt.Sprint(recovered))
					held.header = http.Header{"Content-Type": {"text/plain; charset=utf-8"}}
					held.body.Reset()
					held.body.WriteString("Internal Server Error\n")
					held.status = http.StatusInternalServerError
				}
			}()
			t.serve(held, req, next)
		}()
		// ServeMux sets the matched pattern on the request it routed: "GET /books/{id}".
		route := r.URL.Path
		if _, path, found := strings.Cut(req.Pattern, " "); found {
			route = path
		} else if req.Pattern != "" {
			route = req.Pattern
		}
		span.SetName(r.Method + " " + route)
		span.SetAttributes(attribute.String("http.route", route), attribute.Int("http.response.status_code", held.status))

		id, queries, dbMs, totalMs := t.timing(ctx, start)
		header := w.Header()
		for key, values := range held.header {
			header[key] = values
		}
		body := held.body.Bytes()
		isPage := strings.Contains(header.Get("Content-Type"), "text/html")
		consoleURL := t.console.URL()
		if consoleURL == "" && isPage && t.starter.ensure() {
			// The first page of a fresh project: its agent is starting (once), so the page gets the widget too.
			consoleURL = t.console.URL()
		}
		if id != "" {
			header.Set("Tracepanel-Id", id)
			if consoleURL != "" {
				header.Set("Tracepanel-Url", consoleURL+"#/t/"+id)
			}
			noun := "queries"
			if queries == 1 {
				noun = "query"
			}
			// The agent finds the issues: the widget asks the console for them once the trace is stored.
			header.Add("Server-Timing", fmt.Sprintf(`db;dur=%.1f;desc="%d %s", app;dur=%.1f, total;dur=%.1f, tracepanel-issues;desc="pending"`,
				dbMs, queries, noun, max(totalMs-dbMs, 0), totalMs))
			// The panel is the agent's: without one there is nothing to open.
			if isPage && consoleURL != "" {
				body = inject(body, t.scriptTag(consoleURL, id, r.Method, held.status, queries, dbMs, totalMs))
			}
		}
		header.Set("Content-Length", strconv.Itoa(len(body)))
		w.WriteHeader(held.status)
		_, _ = w.Write(body)
		span.End()
	})
}

type heldResponse struct {
	header http.Header
	body   bytes.Buffer
	status int
}

func (h *heldResponse) Header() http.Header            { return h.header }
func (h *heldResponse) Write(data []byte) (int, error) { return h.body.Write(data) }
func (h *heldResponse) WriteHeader(status int)         { h.status = status }

// scriptTag is widget.min.js with the page's own request in data-tracepanel-* attributes.
func (t *Tracepanel) scriptTag(consoleURL, id, method string, status, queries int, dbMs, totalMs float64) string {
	api := html.EscapeString(consoleURL)
	return fmt.Sprintf(`<script src="%swidget.min.js" data-tracepanel-api="%s" data-tracepanel-id="%s" `+
		`data-tracepanel-method="%s" data-tracepanel-status="%d" data-tracepanel-total="%.1f" data-tracepanel-db="%.1f" `+
		`data-tracepanel-queries="%d" data-tracepanel-issues="pending" data-tracepanel-runtime="%s"></script>`,
		api, api, id, html.EscapeString(method), status, totalMs, dbMs, queries, html.EscapeString(runtimeLabel()))
}

// inject puts the tag first in <head> (the widget wraps fetch before the page's scripts keep a reference to it).
func inject(page []byte, tag string) []byte {
	lower := bytes.ToLower(page)
	if at := bytes.Index(lower, []byte("<head>")); at >= 0 {
		at += len("<head>")
		return append(append(append([]byte{}, page[:at]...), tag...), page[at:]...)
	}
	if at := bytes.LastIndex(lower, []byte("</body>")); at >= 0 {
		return append(append(append([]byte{}, page[:at]...), tag...), page[at:]...)
	}
	return page
}

// serve runs the request through the app's handler; on a ServeMux, the handler it routes to is a `view` span named
// after its function, as Django's view span is.
func (t *Tracepanel) serve(w http.ResponseWriter, r *http.Request, next http.Handler) {
	mux, ok := next.(*http.ServeMux)
	if !ok {
		next.ServeHTTP(w, r)
		return
	}
	handler, pattern := mux.Handler(r)
	name := handlerName(handler)
	if pattern == "" || name == "" {
		// No route (404) or not a function (a file server): nothing to name.
		next.ServeHTTP(w, r)
		return
	}
	ctx, span := tracer.Start(r.Context(), shortFunction(name),
		trace.WithAttributes(attribute.String("tracepanel.kind", "view"), attribute.String("code.function", name)))
	defer span.End()
	trace.SpanFromContext(r.Context()).SetAttributes(attribute.String("code.function", name))
	routed := r.WithContext(ctx)
	next.ServeHTTP(w, routed)
	// The mux sets the matched pattern on the request it routed; the middleware names the request after it.
	r.Pattern = routed.Pattern
}

// handlerName is the function behind a handler: "main.(*App).listBooks".
func handlerName(handler http.Handler) string {
	value := reflect.ValueOf(handler)
	if value.Kind() != reflect.Func {
		return ""
	}
	// A method value (app.listBooks) is compiled as a wrapper named "...listBooks-fm".
	return strings.TrimSuffix(runtime.FuncForPC(value.Pointer()).Name(), "-fm")
}

// ---------- logs ----------

// LogHandler adds the app's slog records to the trace of the request they were logged in, then passes them on.
func (t *Tracepanel) LogHandler(next slog.Handler) slog.Handler { return &logHandler{t: t, next: next} }

type logHandler struct {
	t     *Tracepanel
	next  slog.Handler
	attrs []slog.Attr
}

func (h *logHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.next.Enabled(ctx, level)
}

func (h *logHandler) Handle(ctx context.Context, record slog.Record) error {
	if b := h.t.current(ctx); b != nil {
		attrs := Attrs{}
		for _, attr := range h.attrs {
			attrs["extra."+attr.Key] = attr.Value.String()
		}
		record.Attrs(func(attr slog.Attr) bool {
			attrs["extra."+attr.Key] = attr.Value.String()
			return true
		})
		h.t.mu.Lock()
		b.trace.Logs = append(b.trace.Logs, Log{
			SpanID: h.t.spanID(b, ctx), Time: micros(record.Time), Level: levelName(record.Level), Logger: "app",
			Message: record.Message, Attrs: attrs,
		})
		h.t.mu.Unlock()
	}
	return h.next.Handle(ctx, record)
}

func (h *logHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &logHandler{t: h.t, next: h.next.WithAttrs(attrs), attrs: append(append([]slog.Attr{}, h.attrs...), attrs...)}
}

func (h *logHandler) WithGroup(name string) slog.Handler {
	return &logHandler{t: h.t, next: h.next.WithGroup(name), attrs: h.attrs}
}

func levelName(level slog.Level) string {
	switch {
	case level >= slog.LevelError:
		return "error"
	case level >= slog.LevelWarn:
		return "warning"
	case level >= slog.LevelInfo:
		return "info"
	default:
		return "debug"
	}
}
