package tracepanel

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"html"
	"net"
	"net/http"
	"reflect"
	"runtime"
	"strconv"
	"strings"
)

// WrapHandler is Tracepanel in one line, around the app's handler (its ServeMux):
//
//	http.ListenAndServe(addr, tracepanel.WrapHandler(mux))
//
// It turns Tracepanel on (Setup), and traces every request: its handler on a ServeMux is a `view` span named after
// its function, and the response gets what the badge reads. With TRACEPANEL_ENABLED=0, it returns the handler as it
// is.
func WrapHandler(handler http.Handler, config ...Config) http.Handler {
	t := setup(config...)
	if t == nil {
		return handler
	}
	return t.middleware(handler)
}

// Middleware is WrapHandler as a router's middleware, for chi's r.Use, gorilla's or echo's WrapMiddleware:
//
//	r.Use(tracepanel.Middleware)
//
// Setup, called before it, gives it a Config. With TRACEPANEL_ENABLED=0, it returns the handler as it is.
func Middleware(next http.Handler) http.Handler {
	return WrapHandler(next)
}

// middleware traces every request, and gives each response what the badge reads: the trace's id and timing in
// headers, and the badge's script in an HTML page. The response is held until the handler returns; one the handler
// flushes (a stream) or takes over (a WebSocket) goes out as it is written, with the headers and without the badge.
func (t *tracepanel) middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attrs := attrMap{"http.method": r.Method, "http.path": r.URL.Path, "tracepanel.framework": "net/http"}
		for key, value := range incoming(r.Header.Get(traceparentHeader)) {
			attrs[key] = value
		}
		rec := t.begin("http", "http", r.Method+" "+r.URL.Path, attrs)
		req := r.WithContext(within(r.Context(), rec, rec.root.ID))
		held := &heldResponse{w: w, header: http.Header{}, status: http.StatusOK, t: t, rec: rec}
		func() {
			defer func() {
				if recovered := recover(); recovered != nil {
					if recovered == http.ErrAbortHandler {
						panic(recovered)
					}
					recordPanic(rec, recovered, callers(1))
					held.failed()
				}
			}()
			t.serve(held, req, next)
		}()
		route := t.route(req)
		rec.rename(r.Method + " " + route)
		rec.set("http.route", route)
		rec.set("http.status_code", held.status)
		if held.status >= 500 {
			rec.fail()
		}
		held.finish()
		t.finish(rec)
	})
}

// route is the request's route: the app's Route, else the pattern ServeMux matched ("GET /books/{id}" names
// /books/{id}), else the path.
func (t *tracepanel) route(r *http.Request) string {
	if t.config.Route != nil {
		if route := t.config.Route(r); route != "" {
			return route
		}
	}
	if _, path, found := strings.Cut(r.Pattern, " "); found {
		return path
	}
	if r.Pattern != "" {
		return r.Pattern
	}
	return r.URL.Path
}

// serve runs the request through the app's handler; on a ServeMux, the handler it routes to is a `view` span named
// after its function.
func (t *tracepanel) serve(w http.ResponseWriter, r *http.Request, next http.Handler) {
	mux, ok := next.(*http.ServeMux)
	if !ok {
		next.ServeHTTP(w, r)
		return
	}
	handler, pattern := mux.Handler(r)
	name := handlerName(handler)
	s := scopeOf(r.Context())
	if pattern == "" || name == "" || s == nil {
		// No route (404) or not a function (a file server): nothing to name.
		next.ServeHTTP(w, r)
		return
	}
	span := s.rec.begin(s.span, "view", shortFunction(name), attrMap{"code.function": name})
	s.rec.setView(span.ID)
	s.rec.set("code.view", name)
	failed := true
	defer func() { s.rec.end(span, failed) }()
	routed := r.WithContext(within(r.Context(), s.rec, span.ID))
	next.ServeHTTP(w, routed)
	failed = false
	// The mux sets the matched pattern on the request it routed: the middleware names the request after it.
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

// heldResponse keeps the handler's response until it returns, to add the trace's headers and the badge.
type heldResponse struct {
	w       http.ResponseWriter
	header  http.Header
	body    bytes.Buffer
	status  int
	wrote   bool
	t       *tracepanel
	rec     *recording
	passing bool // flushed or hijacked: written through as it comes
}

func (h *heldResponse) Header() http.Header {
	if h.passing {
		return h.w.Header()
	}
	return h.header
}

func (h *heldResponse) Write(data []byte) (int, error) {
	if !h.wrote {
		h.WriteHeader(http.StatusOK)
	}
	if h.passing {
		return h.w.Write(data)
	}
	return h.body.Write(data)
}

func (h *heldResponse) WriteHeader(status int) {
	if h.wrote {
		return
	}
	h.wrote = true
	h.status = status
	if h.passing {
		h.w.WriteHeader(status)
	}
}

// Flush starts the response now: a stream (server-sent events, a long download) is written as it comes.
func (h *heldResponse) Flush() {
	h.pass()
	if flusher, ok := h.w.(http.Flusher); ok {
		flusher.Flush()
	}
}

// Hijack hands the connection to the handler (a WebSocket): what it does then is its own.
func (h *heldResponse) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	hijacker, ok := h.w.(http.Hijacker)
	if !ok {
		return nil, nil, errors.New("tracepanel: the response can't be hijacked")
	}
	conn, buffer, err := hijacker.Hijack()
	if err == nil {
		h.passing = true
		h.wrote = true
	}
	return conn, buffer, err
}

// pass writes what is held, with the trace's headers, and every later write goes straight out.
func (h *heldResponse) pass() {
	if h.passing {
		return
	}
	h.passing = true
	header := h.w.Header()
	for key, values := range h.header {
		header[key] = values
	}
	h.headers(header)
	if h.wrote {
		h.w.WriteHeader(h.status)
	}
	if h.body.Len() > 0 {
		_, _ = h.w.Write(h.body.Bytes())
		h.body.Reset()
	}
}

// failed answers 500 in place of what the handler wrote before it panicked.
func (h *heldResponse) failed() {
	h.status = http.StatusInternalServerError
	if h.passing {
		return
	}
	h.wrote = true
	h.header = http.Header{"Content-Type": {"text/plain; charset=utf-8"}}
	h.body.Reset()
	h.body.WriteString("Internal Server Error\n")
}

// finish writes the held response, with the trace's headers and, in an HTML page, the badge.
func (h *heldResponse) finish() {
	if h.passing {
		return
	}
	header := h.w.Header()
	for key, values := range h.header {
		header[key] = values
	}
	h.headers(header)
	body := h.body.Bytes()
	if strings.Contains(header.Get("Content-Type"), "text/html") && header.Get("Content-Encoding") == "" {
		if consoleURL := h.consoleURL(true); consoleURL != "" {
			queries, dbMs, totalMs := h.rec.timing()
			body = inject(body, scriptTag(consoleURL, h.rec.doc.ID, h.rec.attr("http.method"), h.status, queries, dbMs, totalMs))
		}
	}
	if header.Get("Content-Length") != "" || len(body) > 0 {
		header.Set("Content-Length", strconv.Itoa(len(body)))
	}
	h.w.WriteHeader(h.status)
	_, _ = h.w.Write(body)
}

// consoleURL is where the project's agent serves the console; for a page, the agent starts if it isn't running.
func (h *heldResponse) consoleURL(page bool) string {
	url := h.t.console.URL()
	if url == "" && page && h.t.starter.ensure() {
		// The first page of a fresh project: its agent is starting (once), so the page gets the badge too.
		url = h.t.console.URL()
	}
	return url
}

// headers are what the badge reads: the trace's id, where it is in the console, and its timing so far.
func (h *heldResponse) headers(header http.Header) {
	id := h.rec.doc.ID
	header.Set("Tracepanel-Id", id)
	if url := h.consoleURL(false); url != "" {
		header.Set("Tracepanel-Url", url+"#/t/"+id)
	}
	queries, dbMs, totalMs := h.rec.timing()
	noun := "queries"
	if queries == 1 {
		noun = "query"
	}
	// The agent finds the issues: the badge asks the console for them once the trace is stored.
	header.Add("Server-Timing", fmt.Sprintf(`db;dur=%.1f;desc="%d %s", app;dur=%.1f, total;dur=%.1f, tracepanel-issues;desc="pending"`,
		dbMs, queries, noun, max(totalMs-dbMs, 0), totalMs))
	// A single-page app on another origin reads them too.
	header.Add("Access-Control-Expose-Headers", "Tracepanel-Id, Tracepanel-Url, Server-Timing")
}

// scriptTag is widget.min.js with the page's own request in data-tracepanel-* attributes.
func scriptTag(consoleURL, id string, method any, status, queries int, dbMs, totalMs float64) string {
	api := html.EscapeString(consoleURL)
	return fmt.Sprintf(`<script src="%swidget.min.js" data-tracepanel-api="%s" data-tracepanel-id="%s" `+
		`data-tracepanel-method="%s" data-tracepanel-status="%d" data-tracepanel-total="%.1f" data-tracepanel-db="%.1f" `+
		`data-tracepanel-queries="%d" data-tracepanel-issues="pending" data-tracepanel-runtime="%s"></script>`,
		api, api, id, html.EscapeString(fmt.Sprint(method)), status, totalMs, dbMs, queries, html.EscapeString(runtimeLabel()))
}

// runtimeLabel is what the panel shows next to its logo: "Go 1.27 · net/http".
func runtimeLabel() string {
	parts := strings.SplitN(goVersion(), ".", 3)
	return "Go " + strings.Join(parts[:min(len(parts), 2)], ".") + " · net/http"
}

// inject puts the tag first in <head> (the badge wraps fetch before the page's scripts keep a reference to it).
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
