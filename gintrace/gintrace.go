// Package gintrace is Tracepanel's middleware for gin: each request is traced as tracepanel.WrapHandler traces it,
// named after its route (c.FullPath(): GET /books/:id), with the handler that served it as a `view` span.
//
//	router := gin.Default()
//	router.Use(gintrace.Middleware())
//
// A statement, a log record or a call joins the request's trace through its context: pass c.Request.Context()
// down (or c itself, with the engine's ContextWithFallback on).
package gintrace

import (
	"bufio"
	"context"
	"net"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"

	tracepanel "github.com/tracepanel/tracepanel-go"
	"github.com/tracepanel/tracepanel-go/internal/hook"
)

type contextKey struct{}

// Middleware traces each request. It turns Tracepanel on (tracepanel.Setup, which takes a Config when called
// before it). When Tracepanel is off (outside development),
// it only calls the next handler.
func Middleware() gin.HandlerFunc {
	traced := tracepanel.Middleware(http.HandlerFunc(serve))
	return func(c *gin.Context) {
		if !hook.Enabled() {
			c.Next()
			return
		}
		writer, request := c.Writer, c.Request
		defer func() { c.Writer, c.Request = writer, request }()
		traced.ServeHTTP(writer, request.WithContext(context.WithValue(request.Context(), contextKey{}, c)))
	}
}

// serve runs the rest of gin's handlers for a traced request: they write to the response Tracepanel holds, and see
// the request with the trace in its context.
func serve(w http.ResponseWriter, r *http.Request) {
	c := r.Context().Value(contextKey{}).(*gin.Context)
	hook.SetTrace(r.Context(), "tracepanel.framework", "gin")
	// gin sets the status of a request without a route (404, 405) before its handlers run.
	status := c.Writer.Status()
	writer := &responseWriter{ResponseWriter: w, status: status, size: -1}
	c.Writer = writer
	ctx, end := r.Context(), func(bool) {}
	if c.FullPath() != "" {
		ctx, end = hook.View(ctx, strings.TrimSuffix(c.HandlerName(), "-fm"))
	}
	c.Request = r.WithContext(ctx)
	failed := true
	defer func() {
		end(failed)
		// Tracepanel names the request after the pattern its handler set, as ServeMux sets it.
		if route := c.FullPath(); route != "" {
			r.Pattern = route
		}
	}()
	c.Next()
	if !writer.Written() {
		// What gin writes after its handlers, which Tracepanel's response holds now.
		if message, ok := defaultBodies[writer.status]; ok && c.FullPath() == "" {
			writer.Header().Set("Content-Type", "text/plain")
			_, _ = writer.WriteString(message)
		}
		writer.WriteHeaderNow()
	}
	failed = writer.status >= http.StatusInternalServerError
}

// defaultBodies are gin's answers to a request without a route, when no handler of the app's answers it.
var defaultBodies = map[int]string{
	http.StatusNotFound:         "404 page not found",
	http.StatusMethodNotAllowed: "405 method not allowed",
}

// responseWriter is gin's ResponseWriter over the response Tracepanel holds.
type responseWriter struct {
	http.ResponseWriter
	status int
	// -1 until the header is written.
	size int
}

var _ gin.ResponseWriter = (*responseWriter)(nil)

func (w *responseWriter) WriteHeader(code int) {
	if code > 0 && !w.Written() {
		w.status = code
	}
}

func (w *responseWriter) WriteHeaderNow() {
	if !w.Written() {
		w.size = 0
		w.ResponseWriter.WriteHeader(w.status)
	}
}

func (w *responseWriter) Write(data []byte) (int, error) {
	w.WriteHeaderNow()
	n, err := w.ResponseWriter.Write(data)
	w.size += n
	return n, err
}

func (w *responseWriter) WriteString(text string) (int, error) { return w.Write([]byte(text)) }
func (w *responseWriter) Status() int                          { return w.status }
func (w *responseWriter) Size() int                            { return w.size }
func (w *responseWriter) Written() bool                        { return w.size != -1 }

func (w *responseWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	if w.size < 0 {
		w.size = 0
	}
	return http.NewResponseController(w.ResponseWriter).Hijack()
}

func (w *responseWriter) Flush() {
	w.WriteHeaderNow()
	_ = http.NewResponseController(w.ResponseWriter).Flush()
}

// CloseNotify is gin's, deprecated in net/http: the request's context says when the client went away.
func (w *responseWriter) CloseNotify() <-chan bool {
	return make(chan bool)
}

func (w *responseWriter) Pusher() http.Pusher {
	pusher, _ := w.ResponseWriter.(http.Pusher)
	return pusher
}

// Unwrap is the response underneath, for http.ResponseController.
func (w *responseWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
