package tracepanel

import (
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

// recorder turns Tracepanel on for a test, with the traces it ends kept here instead of sent to an agent.
type recorder struct {
	mu     sync.Mutex
	traces []*traceDoc
	t      *tracepanel
}

func record(t *testing.T) *recorder {
	t.Helper()
	r := &recorder{}
	module := "github.com/tracepanel/tracepanel-go/app"
	appModule.Store(&module)
	r.t = &tracepanel{
		config:  Config{Service: "shop", StoreDir: t.TempDir(), ConsoleURL: "http://127.0.0.1:9/"},
		console: &console{configured: "http://127.0.0.1:9/"},
		starter: &starter{storeDir: t.TempDir()},
	}
	r.t.deliver = func(doc *traceDoc) {
		r.mu.Lock()
		defer r.mu.Unlock()
		r.traces = append(r.traces, doc)
	}
	active.Store(r.t)
	fake.reset()
	t.Cleanup(func() { active.Store(nil) })
	return r
}

// wait is the traces ended so far, once there are n.
func (r *recorder) wait(t *testing.T, n int) []*traceDoc {
	t.Helper()
	for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
		r.mu.Lock()
		got := append([]*traceDoc{}, r.traces...)
		r.mu.Unlock()
		if len(got) >= n {
			return got
		}
	}
	t.Fatalf("fewer than %d traces", n)
	return nil
}

// spans are a trace's spans of one kind.
func spans(doc *traceDoc, kind string) []spanDoc {
	var out []spanDoc
	for _, span := range doc.Spans {
		if span.Kind == kind {
			out = append(out, span)
		}
	}
	return out
}

// equalJSON compares two values as the agent reads them.
func equalJSON(got, want any) bool {
	a, _ := json.Marshal(got)
	b, _ := json.Marshal(want)
	var x, y any
	_ = json.Unmarshal(a, &x)
	_ = json.Unmarshal(b, &y)
	return reflect.DeepEqual(x, y)
}

func TestARequestIsATraceOfItsSQLLogsAndCalls(t *testing.T) {
	recorded := record(t)
	db, err := sql.Open(WrapDriver("fakedb"), "shop")
	if err != nil {
		t.Fatal(err)
	}
	var traceparentSeen string
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		traceparentSeen = r.Header.Get("traceparent")
		_, _ = io.WriteString(w, "{}")
	}))
	defer other.Close()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /authors/{id}", func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		rows, err := db.QueryContext(ctx, "SELECT id, title FROM books WHERE author_id = $1", 7)
		if err != nil {
			t.Error(err)
			return
		}
		for rows.Next() {
		}
		_ = rows.Close()
		tx, _ := db.BeginTx(ctx, nil)
		_, _ = tx.ExecContext(ctx, "UPDATE authors SET seen = now() WHERE id = $1", 7)
		_ = tx.Commit()
		slog.New(WrapLogHandler(slog.NewTextHandler(io.Discard, nil))).InfoContext(ctx, "author shown", "books", 3,
			slog.Group("client", "api_key", "k-123", "region", "eu"))
		request, _ := http.NewRequestWithContext(ctx, "GET", other.URL+"/rates", nil)
		response, err := (&http.Client{Transport: WrapTransport(http.DefaultTransport)}).Do(request)
		if err == nil {
			response.Body.Close()
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = io.WriteString(w, "<html><head><title>Author</title></head><body>3 books</body></html>")
	})
	server := httptest.NewServer(recorded.t.middleware(mux))
	defer server.Close()

	response, err := http.Get(server.URL + "/authors/7")
	if err != nil {
		t.Fatal(err)
	}
	page, _ := io.ReadAll(response.Body)
	response.Body.Close()
	doc := recorded.wait(t, 1)[0]

	if doc.Kind != "http" || doc.Name != "GET /authors/{id}" || doc.Status != "ok" {
		t.Fatalf("trace %s %q %s", doc.Kind, doc.Name, doc.Status)
	}
	for key, want := range map[string]any{
		"http.method": "GET", "http.path": "/authors/7", "http.route": "/authors/{id}", "http.status_code": 200,
		"service.name": "shop", "service.role": "web", "telemetry.sdk.language": "go", "tracepanel.framework": "net/http",
	} {
		if !equalJSON(doc.Attrs[key], want) {
			t.Errorf("trace attr %s = %v, want %v", key, doc.Attrs[key], want)
		}
	}

	views := spans(doc, "view")
	if len(views) != 1 || !strings.HasSuffix(views[0].Attrs["code.function"].(string), "TestARequestIsATraceOfItsSQLLogsAndCalls.func2") {
		t.Fatalf("view spans: %+v", views)
	}
	queries := spans(doc, "sql")
	if len(queries) != 2 {
		t.Fatalf("sql spans: %+v", queries)
	}
	selectQuery := queries[0]
	if !equalJSON(selectQuery.Attrs, attrMap{
		"db.system": "other_sql", "db.vendor": "other_sql", "db.client": "database/sql", "db.alias": "default",
		"db.namespace": "default", "db.statement": "SELECT id, title FROM books WHERE author_id = $1",
		"db.fingerprint": "SELECT id, title FROM books WHERE author_id = $1", "db.params": []any{7},
		"db.params_hash": selectQuery.Attrs["db.params_hash"], "db.many": false,
		"code.origin": selectQuery.Attrs["code.origin"], "code.template": nil, "db.response.returned_rows": 3,
	}) {
		t.Errorf("select attrs: %v", selectQuery.Attrs)
	}
	if selectQuery.Name != "SELECT books" || *selectQuery.ParentID != views[0].ID {
		t.Errorf("select %q under %s", selectQuery.Name, *selectQuery.ParentID)
	}
	if origin, _ := selectQuery.Attrs["code.origin"].(string); !strings.Contains(origin, "request_test.go:") {
		t.Errorf("code.origin = %q", origin)
	}
	transactions := spans(doc, "transaction")
	if len(transactions) != 1 || transactions[0].Attrs["db.transaction.outcome"] != "commit" ||
		*queries[1].ParentID != transactions[0].ID {
		t.Errorf("transaction %+v, its update under %s", transactions, *queries[1].ParentID)
	}

	if len(doc.Logs) != 1 || doc.Logs[0].Message != "author shown" || doc.Logs[0].Level != "info" ||
		!equalJSON(doc.Logs[0].Attrs["extra.books"], 3) || *doc.Logs[0].SpanID != views[0].ID ||
		doc.Logs[0].Attrs["extra.client.api_key"] != "********" || doc.Logs[0].Attrs["extra.client.region"] != "eu" {
		t.Errorf("logs: %+v", doc.Logs)
	}

	calls := spans(doc, "http.client")
	if len(calls) != 1 || calls[0].Attrs["http.response.status_code"] != 200 || calls[0].Name != "GET "+strings.TrimPrefix(other.URL, "http://")+"/rates" {
		t.Fatalf("calls: %+v", calls)
	}
	// The call carried the trace's W3C id and its span's, which the other app's trace links to.
	if want := "00-" + doc.Attrs["w3c.trace_id"].(string) + "-" + calls[0].Attrs["w3c.span_id"].(string) + "-01"; traceparentSeen != want {
		t.Errorf("traceparent %q, want %q", traceparentSeen, want)
	}

	if response.Header.Get("Tracepanel-Id") != doc.ID || !strings.Contains(response.Header.Get("Server-Timing"), `desc="2 queries"`) {
		t.Errorf("headers: %v", response.Header)
	}
	if !strings.Contains(string(page), `<head><script src="http://127.0.0.1:9/widget.min.js"`) {
		t.Errorf("no badge in %s", page)
	}
}

func TestAPanicFailsItsRequestWithItsStack(t *testing.T) {
	recorded := record(t)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /boom", func(http.ResponseWriter, *http.Request) { panic("boom") })
	server := httptest.NewServer(recorded.t.middleware(mux))
	defer server.Close()

	response, err := http.Get(server.URL + "/boom")
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	doc := recorded.wait(t, 1)[0]

	if response.StatusCode != 500 || doc.Status != "error" || len(doc.Errors) != 1 {
		t.Fatalf("status %d, trace %s, errors %+v", response.StatusCode, doc.Status, doc.Errors)
	}
	failure := doc.Errors[0]
	last := failure.Frames[len(failure.Frames)-1]
	if failure.Type != "panic(string)" || failure.Message != "boom" || !strings.HasSuffix(last.Function, "func1") ||
		last.Filename != "request_test.go" || *failure.SpanID != spans(doc, "view")[0].ID {
		t.Fatalf("error %+v, innermost frame %+v", failure, last)
	}
}

func TestAStreamGoesOutAsItIsWrittenWithTheTracesHeaders(t *testing.T) {
	recorded := record(t)
	written := make(chan struct{})
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: one\n\n")
		w.(http.Flusher).Flush()
		<-written
		_, _ = io.WriteString(w, "data: two\n\n")
	})
	server := httptest.NewServer(recorded.t.middleware(handler))
	defer server.Close()

	response, err := http.Get(server.URL + "/events")
	if err != nil {
		t.Fatal(err)
	}
	first := make([]byte, len("data: one\n\n"))
	_, err = io.ReadFull(response.Body, first)
	close(written)
	rest, _ := io.ReadAll(response.Body)
	response.Body.Close()

	if err != nil || string(first) != "data: one\n\n" || string(rest) != "data: two\n\n" {
		t.Fatalf("read %q then %q (%v)", first, rest, err)
	}
	if response.Header.Get("Tracepanel-Id") != recorded.wait(t, 1)[0].ID {
		t.Fatal("the stream has no trace id")
	}
}

func TestAStatementTheDriverDeclinesIsRecordedOnceAsItRan(t *testing.T) {
	recorded := record(t)
	db, _ := sql.Open(WrapDriver("fakedb"), "shop")
	fake.skipQueries = true

	err := Command(context.Background(), "report", func(ctx context.Context) error {
		rows, err := db.QueryContext(ctx, "SELECT title FROM books WHERE id = ?", 1)
		if err != nil {
			return err
		}
		return rows.Close()
	})
	doc := recorded.wait(t, 1)[0]

	if err != nil || len(spans(doc, "sql")) != 1 || doc.Kind != "command" || doc.Name != "report" {
		t.Fatalf("%v; trace %s %q with %d queries", err, doc.Kind, doc.Name, len(spans(doc, "sql")))
	}
}

func TestACommandsErrorFailsItsTrace(t *testing.T) {
	recorded := record(t)
	db, _ := sql.Open(WrapDriver("fakedb"), "shop")

	err := Command(context.Background(), "migrate", func(ctx context.Context) error {
		_, err := db.ExecContext(ctx, "INSERT INTO missing VALUES (1)")
		return err
	})
	doc := recorded.wait(t, 1)[0]

	if err == nil || doc.Status != "error" || len(doc.Errors) != 1 || doc.Errors[0].Message != "no such table: missing" ||
		spans(doc, "sql")[0].Status != "error" || doc.Attrs["service.role"] != "command" {
		t.Fatalf("%v; trace %+v", err, doc)
	}
}

func TestATaskIsATraceLinkedToTheRequestThatStartedIt(t *testing.T) {
	recorded := record(t)
	done := make(chan struct{})
	Command(context.Background(), "signup", func(ctx context.Context) error {
		Go(ctx, "send welcome", func(ctx context.Context) error {
			_, end := Span(ctx, "render")
			end()
			close(done)
			return nil
		})
		return nil
	})
	<-done
	traces := recorded.wait(t, 2)

	command, task := traces[0], traces[1]
	enqueued := spans(command, "task.enqueue")
	if len(enqueued) != 1 || task.Kind != "task" || task.Spans[0].Kind != "task.run" || task.Name != "send welcome" {
		t.Fatalf("command %+v, task %+v", command, task)
	}
	if !equalJSON(task.Links, []linkDoc{{Kind: "enqueued_by", TraceID: command.ID, SpanID: enqueued[0].ID}}) ||
		task.Attrs["task.trigger"] != "request" || len(spans(task, "function")) != 1 {
		t.Fatalf("task links %+v, attrs %v", task.Links, task.Attrs)
	}
}

func TestADriverIsRegisteredOnceUnderItsOwnName(t *testing.T) {
	first, again, replica := WrapDriver("fakedb"), WrapDriver("fakedb"), WrapDriver("fakedb", DBAlias("replica"))
	t.Setenv("TRACEPANEL_ENABLED", "0")
	off := WrapDriver("fakedb")

	if first != "tracepanel:fakedb" || again != first || replica != "tracepanel:fakedb:replica" || off != "fakedb" {
		t.Fatalf("%q, %q, %q, off %q", first, again, replica, off)
	}
}

func TestTheMiddlewareOfATracepanelThatIsOffIsTheHandler(t *testing.T) {
	t.Setenv("TRACEPANEL_ENABLED", "0")
	handler := http.NotFoundHandler()

	wrapped := Middleware(handler)

	if reflect.ValueOf(wrapped).Pointer() != reflect.ValueOf(handler).Pointer() {
		t.Fatal("Middleware wrapped a handler with Tracepanel off")
	}
}
