// Package echo5trace is Tracepanel's middleware for Echo v5: each request is traced as tracepanel.WrapHandler
// traces it, named after its route (c.Path(): GET /books/:id). For Echo v4, see echotrace.
//
//	e := echo.New()
//	e.Use(echo5trace.Middleware())
//
// A statement, a log record or a call joins the request's trace through its context: pass
// c.Request().Context() down.
package echo5trace

import (
	"context"
	"net/http"

	"github.com/labstack/echo/v5"

	tracepanel "github.com/tracepanel/tracepanel-go"
	"github.com/tracepanel/tracepanel-go/internal/hook"
)

type contextKey struct{}

// call is one request through the middleware: its context, the next handler, and what it returned.
type call struct {
	c        *echo.Context
	response *echo.Response
	next     echo.HandlerFunc
	err      error
}

// Middleware traces each request. It turns Tracepanel on (tracepanel.Setup, which takes a Config when called
// before it). With TRACEPANEL_ENABLED=0, it only calls the next handler.
func Middleware() echo.MiddlewareFunc {
	traced := tracepanel.Middleware(http.HandlerFunc(serve))
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c *echo.Context) error {
			response, err := echo.UnwrapResponse(c.Response())
			if !hook.Enabled() || err != nil {
				return next(c)
			}
			request, writer := c.Request(), response.ResponseWriter
			defer func() {
				c.SetRequest(request)
				response.ResponseWriter = writer
			}()
			run := &call{c: c, response: response, next: next}
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
	run.response.ResponseWriter = w
	c.SetRequest(r)
	defer func() {
		// Tracepanel names the request after the pattern its handler set, as ServeMux sets it.
		if route := c.Path(); route != "" {
			r.Pattern = route
		}
	}()
	if run.err = run.next(c); run.err != nil {
		c.Echo().HTTPErrorHandler(c, run.err)
	}
}
