//go:build !windows

// Package e2e runs a Go app against the real agent: WrapHandler and WrapDriver on SQLite, a page with an N+1, and the
// agent's API read back, its trace, its SQL spans and the n+1 issue. The agent is TRACEPANEL_AGENT_BIN, else a build
// of the agent's repository next to this one, else the one the SDK downloads (cmd/tracepanel runs it); without any,
// the test is skipped. A module of its own, so the SDK itself keeps no dependencies.
package e2e

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	tracepanel "github.com/tracepanel/tracepanel-go"
	_ "modernc.org/sqlite"
)

// agent starts the agent on `dir` through cmd/tracepanel, until the test ends; its console's URL.
func agent(t *testing.T, dir string) string {
	t.Helper()
	command := filepath.Join(t.TempDir(), "tracepanel")
	build := exec.Command("go", "build", "-o", command, "github.com/tracepanel/tracepanel-go/cmd/tracepanel")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build cmd/tracepanel: %v\n%s", err, out)
	}
	env := append(os.Environ(), "TRACEPANEL_DIR="+dir, "TRACEPANEL_OTLP=off")
	if os.Getenv("TRACEPANEL_AGENT_BIN") == "" {
		sibling, _ := filepath.Abs(filepath.Join("..", "..", "tracepanel", "target", "debug", "tracepanel-agent"))
		if _, err := os.Stat(sibling); err == nil {
			env = append(env, "TRACEPANEL_AGENT_BIN="+sibling)
		}
	}
	var output bytes.Buffer
	cmd := exec.Command(command, "dev", "--port", "0")
	cmd.Env, cmd.Stdout, cmd.Stderr = env, &output, &output
	// Its own process group: the agent under it stops with it.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	t.Cleanup(func() {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		<-exited
	})
	deadline := time.After(2 * time.Minute)
	for {
		select {
		case err := <-exited:
			if strings.Contains(output.String(), "no tracepanel-agent could be had") {
				t.Skipf("no agent to test against: set TRACEPANEL_AGENT_BIN, or let the SDK download it\n%s", output.String())
			}
			t.Fatalf("the agent exited: %v\n%s", err, output.String())
		case <-deadline:
			t.Fatalf("the agent didn't start\n%s", output.String())
		case <-time.After(50 * time.Millisecond):
		}
		_, addrErr := os.Stat(filepath.Join(dir, "agent.addr"))
		console, consoleErr := os.ReadFile(filepath.Join(dir, "console.addr"))
		if addrErr == nil && consoleErr == nil && len(console) > 0 {
			return strings.TrimSpace(string(console))
		}
	}
}

// get decodes the agent's answer at `path` into `into`; false while it has none (404, 204).
func get(t *testing.T, console, path string, into any) bool {
	t.Helper()
	response, err := http.Get(strings.TrimSuffix(console, "/") + "/api/" + path)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, _ := io.ReadAll(response.Body)
	if response.StatusCode == http.StatusNotFound || response.StatusCode == http.StatusNoContent {
		return false
	}
	if response.StatusCode != http.StatusOK {
		t.Fatalf("%s: HTTP %d %s", path, response.StatusCode, body)
	}
	if err := json.Unmarshal(body, into); err != nil {
		t.Fatalf("%s: %v: %s", path, err, body)
	}
	return true
}

type span struct {
	Kind  string         `json:"kind"`
	Name  string         `json:"name"`
	Attrs map[string]any `json:"attrs"`
}

type trace struct {
	ID     string         `json:"id"`
	Kind   string         `json:"kind"`
	Name   string         `json:"name"`
	Status string         `json:"status"`
	Attrs  map[string]any `json:"attrs"`
	Spans  []span         `json:"spans"`
}

type issue struct {
	Kind  string `json:"kind"`
	Count int    `json:"count"`
}

// The shop's authors page: its authors, then each one's books, a query per author.
func authors(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		rows, err := db.QueryContext(ctx, "SELECT id, name FROM authors ORDER BY id")
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		var ids []int
		for rows.Next() {
			var id int
			var name string
			_ = rows.Scan(&id, &name)
			ids = append(ids, id)
		}
		_ = rows.Close()
		var page strings.Builder
		page.WriteString("<html><head><title>Authors</title></head><body>")
		for _, id := range ids {
			var count int
			// The N+1: one query per author.
			if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM books WHERE author_id = ?", id).Scan(&count); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			fmt.Fprintf(&page, "<p>author %d: %d books</p>", id, count)
		}
		page.WriteString("</body></html>")
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = io.WriteString(w, page.String())
	}
}

func TestAnNPlusOneReachesTheAgentAsATraceWithItsIssue(t *testing.T) {
	dir := filepath.Join(t.TempDir(), ".tracepanel")
	console := agent(t, dir)
	t.Setenv("TRACEPANEL_DIR", dir)
	t.Setenv("TRACEPANEL_AGENT", "")
	t.Setenv("TRACEPANEL_SERVICE", "e2e-shop")
	db, err := sql.Open(tracepanel.WrapDriver("sqlite"), filepath.Join(t.TempDir(), "shop.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, statement := range []string{
		"CREATE TABLE authors (id INTEGER PRIMARY KEY, name TEXT)",
		"CREATE TABLE books (id INTEGER PRIMARY KEY, author_id INTEGER, title TEXT)",
		"INSERT INTO authors (name) VALUES ('Le Guin'), ('Pratchett'), ('Jemisin'), ('Banks'), ('Wolfe')",
		"INSERT INTO books (author_id, title) VALUES (1, 'The Dispossessed'), (2, 'Mort'), (2, 'Guards! Guards!')",
	} {
		if _, err := db.ExecContext(context.Background(), statement); err != nil {
			t.Fatal(err)
		}
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /authors", authors(db))
	server := httptest.NewServer(tracepanel.WrapHandler(mux))
	defer server.Close()

	response, err := http.Get(server.URL + "/authors")
	if err != nil {
		t.Fatal(err)
	}
	page, _ := io.ReadAll(response.Body)
	response.Body.Close()
	id := response.Header.Get("Tracepanel-Id")
	tracepanel.Flush(5 * time.Second)
	var stored trace
	for deadline := time.Now().Add(15 * time.Second); !get(t, console, "traces/"+id, &stored); time.Sleep(100 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("the agent has no trace %q", id)
		}
	}
	var found struct {
		Issues []issue `json:"issues"`
	}
	get(t, console, "traces/"+id+"/issues", &found)
	var listed struct {
		Traces []struct {
			ID string `json:"id"`
		} `json:"traces"`
	}
	get(t, console, "traces", &listed)
	var app struct {
		Settings map[string]any `json:"settings"`
		Versions []struct {
			Name string `json:"name"`
			Kind string `json:"kind"`
		} `json:"versions"`
	}
	get(t, console, "app?service=e2e-shop", &app)

	if response.StatusCode != http.StatusOK || !strings.Contains(string(page), "widget.min.js") {
		t.Fatalf("HTTP %d, the page has no badge: %s", response.StatusCode, page)
	}
	if stored.Kind != "http" || stored.Name != "GET /authors" || stored.Status != "ok" ||
		stored.Attrs["service.name"] != "e2e-shop" || stored.Attrs["telemetry.sdk.language"] != "go" {
		t.Fatalf("trace %s %q %s %v", stored.Kind, stored.Name, stored.Status, stored.Attrs)
	}
	var queries []string
	for _, s := range stored.Spans {
		if s.Kind == "sql" {
			queries = append(queries, s.Name+" | "+fmt.Sprint(s.Attrs["db.fingerprint"])+" | "+fmt.Sprint(s.Attrs["db.system"]))
		}
	}
	want := []string{"SELECT authors | SELECT id, name FROM authors ORDER BY id | sqlite"}
	for range 5 {
		want = append(want, "SELECT COUNT books | SELECT COUNT(*) FROM books WHERE author_id = ? | sqlite")
	}
	if strings.Join(queries, "\n") != strings.Join(want, "\n") {
		t.Fatalf("SQL spans:\n%s", strings.Join(queries, "\n"))
	}
	nPlusOne := false
	for _, found := range found.Issues {
		nPlusOne = nPlusOne || found.Kind == "n+1" && found.Count == 5
	}
	if !nPlusOne {
		t.Fatalf("issues %+v", found.Issues)
	}
	if len(listed.Traces) == 0 || listed.Traces[0].ID != id {
		t.Fatalf("api/traces %+v", listed.Traces)
	}
	// The App tab: what the hello said.
	modules := map[string]string{}
	for _, version := range app.Versions {
		modules[version.Name] = version.Kind
	}
	if modules["Go"] != "runtime" || modules["modernc.org/sqlite"] != "package" || app.Settings["TRACEPANEL_SERVICE"] != "e2e-shop" {
		t.Fatalf("api/app %+v", app)
	}
}
