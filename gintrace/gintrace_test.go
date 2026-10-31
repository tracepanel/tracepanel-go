package gintrace

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	tracepanel "github.com/tracepanel/tracepanel-go"
	"github.com/tracepanel/tracepanel-go/internal/tracetest"
)

func showBook(c *gin.Context) {
	_, end := tracepanel.Span(c.Request.Context(), "load book")
	end()
	c.JSON(http.StatusOK, gin.H{"id": c.Param("id")})
}

func router() *gin.Engine {
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	engine.Use(Middleware())
	engine.GET("/books/:id", showBook)
	engine.GET("/boom", func(*gin.Context) { panic("out of stock") })
	engine.GET("/fail", func(c *gin.Context) { c.String(http.StatusServiceUnavailable, "later") })
	return engine
}

func TestARequestIsNamedByItsRoute(t *testing.T) {
	recorder := tracetest.Record(t)
	engine := router()

	response := httptest.NewRecorder()
	engine.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/books/7?x=1", nil))

	if response.Code != http.StatusOK || response.Body.String() != `{"id":"7"}` || response.Header().Get("Tracepanel-Id") == "" {
		t.Fatalf("response: %d %q %v", response.Code, response.Body.String(), response.Header())
	}
	trace := recorder.Wait(t, 1)[0]
	attrs := tracetest.Attrs(trace)
	if trace["name"] != "GET /books/:id" || attrs["http.route"] != "/books/:id" || attrs["http.path"] != "/books/7" ||
		attrs["tracepanel.framework"] != "gin" || attrs["http.status_code"] != float64(200) || trace["id"] != response.Header().Get("Tracepanel-Id") {
		t.Fatalf("trace: %v %v", trace["name"], attrs)
	}
	views := tracetest.Spans(trace, "view")
	if len(views) != 1 || views[0]["name"] != "showBook" || attrs["code.view"] != "github.com/tracepanel/tracepanel-go/gintrace.showBook" {
		t.Fatalf("views: %v", views)
	}
	work := tracetest.Spans(trace, "function")
	if len(work) != 1 || work[0]["parent_id"] != views[0]["id"] {
		t.Fatalf("work under the view: %v", work)
	}
}

func TestAPanicAnswers500(t *testing.T) {
	recorder := tracetest.Record(t)
	engine := router()

	response := httptest.NewRecorder()
	engine.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/boom", nil))

	if response.Code != http.StatusInternalServerError {
		t.Fatalf("status: %d", response.Code)
	}
	trace := recorder.Wait(t, 1)[0]
	errors := tracetest.List(trace, "errors")
	if trace["status"] != "error" || len(errors) != 1 || errors[0]["message"] != "out of stock" {
		t.Fatalf("trace: %v %v", trace["status"], errors)
	}
	if view := tracetest.Spans(trace, "view")[0]; view["status"] != "error" {
		t.Fatalf("view: %v", view)
	}
}

func TestAServerErrorFailsTheTrace(t *testing.T) {
	recorder := tracetest.Record(t)
	engine := router()

	response := httptest.NewRecorder()
	engine.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/fail", nil))

	trace := recorder.Wait(t, 1)[0]
	if response.Code != http.StatusServiceUnavailable || response.Body.String() != "later" || trace["status"] != "error" {
		t.Fatalf("response %d %q, trace %v", response.Code, response.Body.String(), trace["status"])
	}
}

func TestNoRouteIsNamedByItsPath(t *testing.T) {
	recorder := tracetest.Record(t)
	engine := router()

	response := httptest.NewRecorder()
	engine.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/nowhere", nil))

	trace := recorder.Wait(t, 1)[0]
	if response.Code != http.StatusNotFound || response.Body.String() != "404 page not found" || trace["name"] != "GET /nowhere" || len(tracetest.Spans(trace, "view")) != 0 {
		t.Fatalf("response %d, trace %v", response.Code, trace["name"])
	}
}

func TestAnHTMLPageGetsTheBadge(t *testing.T) {
	recorder := tracetest.Record(t)
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	engine.Use(Middleware())
	engine.GET("/", func(c *gin.Context) {
		c.Data(http.StatusOK, "text/html; charset=utf-8", []byte("<html><head></head><body>hi</body></html>"))
	})

	response := httptest.NewRecorder()
	engine.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/", nil))

	recorder.Wait(t, 1)
	if body := response.Body.String(); !strings.Contains(body, "widget.min.js") {
		t.Fatalf("body: %s", body)
	}
}
