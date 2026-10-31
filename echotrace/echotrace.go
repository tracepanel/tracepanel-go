// Package echotrace is Tracepanel's middleware for Echo v4: each request is traced as tracepanel.WrapHandler
// traces it, named after its route (c.Path(): GET /books/:id), with the handler that served it as a `view` span.
// For Echo v5, see echo5trace.
//
//	e := echo.New()
//	e.Use(echotrace.Middleware())
//
// A statement, a log record or a call joins the request's trace through its context: pass
// c.Request().Context() down.
package echotrace

import (
	"context"
	"net/http"
	"strings"

	"github.com/labstack/echo/v4"

	tracepanel "github.com/tracepanel/tracepanel-go"
	"github.com/tracepanel/tracepanel-go/internal/hook"
)

type contextKey struct{}

// call is one request through the middleware: its context, the next handler, and what it returned.
type call struct {
	c    echo.Context
	next echo.HandlerFunc
	err  error
}

// Middleware traces each request. It turns Tracepanel on (tracepanel.Setup, which takes a Config when called
// before it). When Tracepanel is off (outside development),
// it only calls the next handler.
func Middleware() echo.MiddlewareFunc {
	traced := tracepanel.Middleware(http.HandlerFunc(serve))
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			if !hook.Enabled() {
				return next(c)
			}
			request, response := c.Request(), c.Response()
			writer := response.Writer
			defer func() {
				c.SetRequest(request)
				response.Writer = writer
			}()
			run := &call{c: c, next: next}
			traced.ServeHTTP(writer, request.WithContext(context.WithValue(request.Context(), contextKey{}, run)))
			// The error handler answered already, in the traced response; outer middleware (a logger) still sees it.
			return run.err
		}
	}
}

// serve runs the next handler for a traced request: it writes to the response Tracepanel holds, and sees the
// request with the trace in its context.
func serve(w http.ResponseWriter, r *http.Request) {
	run := r.Context().Value(contextKey{}).(*call)
	c := run.c
	hook.SetTrace(r.Context(), "tracepanel.framework", "echo")
	c.Response().Writer = w
	ctx, end := r.Context(), func(bool) {}
	if name := handlerName(c, r.Method); name != "" {
		ctx, end = hook.View(ctx, name)
	}
	c.SetRequest(r.WithContext(ctx))
	failed := true
	defer func() {
		end(failed)
		// Tracepanel names the request after the pattern its handler set, as ServeMux sets it.
		if route := c.Path(); route != "" {
			r.Pattern = route
		}
	}()
	if run.err = run.next(c); run.err != nil {
		c.Error(run.err)
	}
	failed = c.Response().Status >= http.StatusInternalServerError
}

// handlerName is the function that handles the request's route, "main.(*App).showBook", as Echo named the route
// after it; "" for no route, or one Echo handles itself.
func handlerName(c echo.Context, method string) string {
	path := c.Path()
	if path == "" {
		return ""
	}
	for _, route := range c.Echo().Routes() {
		if route.Path == path && route.Method == method {
			name := strings.TrimSuffix(route.Name, "-fm")
			if strings.HasPrefix(name, "github.com/labstack/echo/") || !strings.Contains(name, ".") {
				return ""
			}
			return name
		}
	}
	return ""
}
