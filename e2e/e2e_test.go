//go:build !windows

// Package e2e runs a Go app against the real agent: WrapHandler and tracepanel.Driver on SQLite, a page with an N+1, and the
// agent's API read back, its trace, its SQL spans and the n+1 issue; then a request asked to be profiled, its
// profile and what it carried (the Request tab). The agent is TRACEPANEL_AGENT_BIN, else a build
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
	Logs   []struct {
		Level   string         `json:"level"`
		Logger  string         `json:"logger"`
		Message string         `json:"message"`
		Attrs   map[string]any `json:"attrs"`
	} `json:"logs"`
	Profile *struct {
		IntervalUs int `json:"interval_us"`
		Samples    int `json:"samples"`
		Frames     []struct {
			Function string `json:"function"`
			InApp    bool   `json:"in_app"`
		} `json:"frames"`
		Stacks []struct {
			Frames []int `json:"frames"`
			Count  int   `json:"count"`
		} `json:"stacks"`
	} `json:"profile"`
}

// report is a request that keeps a CPU busy: what a profile shows.
func report(w http.ResponseWriter, r *http.Request) {
	var order map[string]any
	_ = json.NewDecoder(r.Body).Decode(&order)
	sum := 0.0
	for deadline := time.Now().Add(300 * time.Millisecond); time.Now().Before(deadline); {
		for i := range 10_000 {
			sum += float64(i) * 1.0001
		}
	}
	fmt.Fprintf(w, `{"sum": %f}`, sum)
}

// stored is the agent's trace `id`, once it has it.
func stored(t *testing.T, console, id string) trace {
	t.Helper()
	var found trace
	for deadline := time.Now().Add(15 * time.Second); !get(t, console, "traces/"+id, &found); time.Sleep(100 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("the agent has no trace %q", id)
		}
	}
	return found
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
		// A debug print, in the trace too.
		tracepanel.Dump(ctx, ids)
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
	// A test binary is no development run: Tracepanel records when told to.
	t.Setenv("TRACEPANEL_ENABLED", "1")
	t.Setenv("TRACEPANEL_DIR", dir)
	t.Setenv("TRACEPANEL_AGENT", "")
	t.Setenv("TRACEPANEL_SERVICE", "e2e-shop")
	db, err := sql.Open(tracepanel.Driver("sqlite"), filepath.Join(t.TempDir(), "shop.db"))
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
	mux.HandleFunc("POST /report", report)
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
	stored := stored(t, console, id)
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
	// The dump, as the agent keeps it.
	if len(stored.Logs) != 1 || stored.Logs[0].Level != "debug" || stored.Logs[0].Logger != "tracepanel" ||
		stored.Logs[0].Attrs["log.kind"] != "dump" || stored.Logs[0].Attrs["dump.lang"] != "go" ||
		fmt.Sprint(stored.Logs[0].Attrs["dump.args"]) != "[map[expr:ids len:5 repr:[]int{1, 2, 3, 4, 5} type:[]int]]" ||
		!strings.Contains(stored.Logs[0].Message, "\n    ids: []int{1, 2, 3, 4, 5} ([]int) len=5") {
		t.Fatalf("logs %+v", stored.Logs)
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

	profiledReport(t, server, console)
}

// profiledReport: a request asked to be profiled reaches the agent with its profile, and with what it carried for
// the Request tab. (Tracepanel is set up once a process: the same app, agent and service as the N+1's.)
func profiledReport(t *testing.T, server *httptest.Server, console string) {
	t.Helper()
	request, _ := http.NewRequest(http.MethodPost, server.URL+"/report?month=10&token=abc",
		strings.NewReader(`{"month": 10, "password": "hunter2"}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer s3cr3t")
	request.AddCookie(&http.Cookie{Name: "tracepanel_profile", Value: "1"})
	response, err := http.DefaultTransport.RoundTrip(request)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, response.Body)
	response.Body.Close()
	id := response.Header.Get("Tracepanel-Id")
	tracepanel.Flush(5 * time.Second)
	found := stored(t, console, id)
	var capabilities map[string]any
	get(t, console, "capabilities?service=e2e-shop", &capabilities)

	// The Request tab: what the request carried, its secrets masked.
	for key, want := range map[string]any{
		"url.query":                              "month=10&token=********",
		"http.request.header.authorization":      "********",
		"http.request.header.content_type":       "application/json",
		"http.request.cookie.tracepanel_profile": "1",
		"http.request.body":                      `{"month":10,"password":"********"}`,
		"http.response.header.content_type":      "text/plain; charset=utf-8",
	} {
		if found.Attrs[key] != want {
			t.Errorf("%s: %v, want %v", key, found.Attrs[key], want)
		}
	}
	// Its profile: where its CPU time went, the app's function in it.
	profile := found.Profile
	if profile == nil || profile.IntervalUs != 10_000 || profile.Samples == 0 || len(profile.Stacks) == 0 {
		t.Fatalf("profile %+v", profile)
	}
	inApp := false
	for _, frame := range profile.Frames {
		inApp = inApp || frame.Function == "report" && frame.InApp
	}
	if !inApp {
		t.Fatalf("frames %+v", profile.Frames)
	}
	if capabilities["profile"] != true || capabilities["request"] != true || capabilities["profile_memory"] != true {
		t.Fatalf("capabilities %v", capabilities)
	}
}
