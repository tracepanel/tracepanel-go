package echo5trace

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/labstack/echo/v5"

	tracepanel "github.com/tracepanel/tracepanel-go"
	"github.com/tracepanel/tracepanel-go/internal/tracetest"
)

func showBook(c *echo.Context) error {
	_, end := tracepanel.Span(c.Request().Context(), "load book")
	end()
	return c.JSON(http.StatusOK, map[string]string{"id": c.Param("id")})
}

func server() *echo.Echo {
	e := echo.New()
	e.Use(Middleware())
	e.GET("/books/:id", showBook)
	e.GET("/missing", func(*echo.Context) error { return echo.NewHTTPError(http.StatusNotFound, "no such book") })
	e.GET("/broken", func(*echo.Context) error { return errors.New("database down") })
	e.GET("/boom", func(*echo.Context) error { panic("out of stock") })
	return e
}

func get(e *echo.Echo, path string) *httptest.ResponseRecorder {
	response := httptest.NewRecorder()
	e.ServeHTTP(response, httptest.NewRequest(http.MethodGet, path, nil))
	return response
}

func TestARequestIsNamedByItsRoute(t *testing.T) {
	recorder := tracetest.Record(t)

	response := get(server(), "/books/7")

	if response.Code != http.StatusOK || response.Body.String() != "{\"id\":\"7\"}\n" || response.Header().Get("Tracepanel-Id") == "" {
		t.Fatalf("response: %d %q %v", response.Code, response.Body.String(), response.Header())
	}
	trace := recorder.Wait(t, 1)[0]
	attrs := tracetest.Attrs(trace)
	if trace["name"] != "GET /books/:id" || attrs["http.route"] != "/books/:id" || attrs["tracepanel.framework"] != "echo" ||
		attrs["http.status_code"] != float64(200) {
		t.Fatalf("trace: %v %v", trace["name"], attrs)
	}
	// Echo v5 doesn't tell the route's handler: no view span, the work goes under the request.
	work := tracetest.Spans(trace, "function")
	if len(work) != 1 || work[0]["parent_id"] != tracetest.Root(trace)["id"] || len(tracetest.Spans(trace, "view")) != 0 {
		t.Fatalf("work: %v", trace["spans"])
	}
}

func TestAnErrorIsAnsweredByTheErrorHandler(t *testing.T) {
	recorder := tracetest.Record(t)
	e := server()

	notFound := get(e, "/missing")
	broken := get(e, "/broken")

	traces := recorder.Wait(t, 2)
	if notFound.Code != http.StatusNotFound || traces[0]["status"] != "ok" || tracetest.Attrs(traces[0])["http.status_code"] != float64(404) {
		t.Fatalf("404: %d %v", notFound.Code, traces[0])
	}
	if broken.Code != http.StatusInternalServerError || traces[1]["status"] != "error" {
		t.Fatalf("500: %d %v", broken.Code, traces[1]["status"])
	}
}

func TestAPanicAnswers500(t *testing.T) {
	recorder := tracetest.Record(t)

	response := get(server(), "/boom")

	trace := recorder.Wait(t, 1)[0]
	errors := tracetest.List(trace, "errors")
	if response.Code != http.StatusInternalServerError || trace["status"] != "error" || len(errors) != 1 || errors[0]["message"] != "out of stock" {
		t.Fatalf("response %d, trace %v %v", response.Code, trace["status"], errors)
	}
}

func TestNoRouteIsNamedByItsPath(t *testing.T) {
	recorder := tracetest.Record(t)

	response := get(server(), "/nowhere")

	trace := recorder.Wait(t, 1)[0]
	if response.Code != http.StatusNotFound || trace["name"] != "GET /nowhere" || len(tracetest.Spans(trace, "view")) != 0 {
		t.Fatalf("response %d, trace %v %v", response.Code, trace["name"], trace["spans"])
	}
}
