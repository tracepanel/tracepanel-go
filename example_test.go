package tracepanel_test

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"time"

	tracepanel "github.com/tracepanel/tracepanel-go"
)

// The app's ServeMux, wrapped: each request a trace, its handler a span, the badge on its pages.
func ExampleWrapHandler() {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /books/{id}", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "book %s", r.PathValue("id"))
	})
	log.Fatal(http.ListenAndServe("127.0.0.1:8080", tracepanel.WrapHandler(mux)))
}

// The database opened through the wrapped driver: each statement run with a request's context is recorded, with
// the line that ran it.
func ExampleWrapDriver() {
	db, err := sql.Open(tracepanel.WrapDriver("pgx"), os.Getenv("DATABASE_URL"))
	if err != nil {
		log.Fatal(err)
	}
	http.HandleFunc("GET /authors", func(w http.ResponseWriter, r *http.Request) {
		// With the request's context: QueryContext, not Query.
		rows, err := db.QueryContext(r.Context(), "SELECT id, name FROM authors")
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		defer rows.Close()
	})
}

// A second database, a replica, named apart, of a kind its driver's name doesn't tell.
func ExampleWrapDriver_options() {
	replica, err := sql.Open(
		tracepanel.WrapDriver("pgx", tracepanel.DBAlias("replica"), tracepanel.DBSystem("postgresql")),
		os.Getenv("REPLICA_URL"),
	)
	if err != nil {
		log.Fatal(err)
	}
	defer replica.Close()
}

// An HTTP client of the app's own, whose calls are recorded and carry the trace's traceparent.
func ExampleWrapTransport() {
	client := &http.Client{Transport: tracepanel.WrapTransport(nil), Timeout: 10 * time.Second}
	http.HandleFunc("GET /rates", func(w http.ResponseWriter, r *http.Request) {
		request, _ := http.NewRequestWithContext(r.Context(), "GET", "https://api.example.com/rates", nil)
		response, err := client.Do(request)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		defer response.Body.Close()
	})
}

// Work that outlives the request, as a task: a trace of its own, linked to the request that started it.
func ExampleGo() {
	http.HandleFunc("POST /orders", func(w http.ResponseWriter, r *http.Request) {
		tracepanel.Go(r.Context(), "send receipt", func(ctx context.Context) error {
			slog.InfoContext(ctx, "receipt sent")
			return nil
		})
		w.WriteHeader(http.StatusAccepted)
	})
}

// A command or a scheduled job, a trace of its own; Flush before the program exits.
func ExampleCommand() {
	tracepanel.Setup()
	defer tracepanel.Flush(5 * time.Second)
	err := tracepanel.Command(context.Background(), "nightly cleanup", func(ctx context.Context) error {
		slog.InfoContext(ctx, "old sessions removed", "count", 42)
		return nil
	})
	if err != nil {
		log.Print(err)
	}
}

// The signed-in user, named on the request's trace.
func ExampleSetUser() {
	type user struct {
		ID    int64
		Email string
	}
	http.HandleFunc("GET /account", func(w http.ResponseWriter, r *http.Request) {
		signedIn := user{ID: 7, Email: "ada@example.com"}
		tracepanel.SetUser(r.Context(), strconv.FormatInt(signedIn.ID, 10), signedIn.Email)
	})
}

// A piece of the app's own code as a span.
func ExampleSpan() {
	http.HandleFunc("GET /invoice", func(w http.ResponseWriter, r *http.Request) {
		ctx, end := tracepanel.Span(r.Context(), "render invoice")
		defer end()
		slog.InfoContext(ctx, "rendered")
	})
}
