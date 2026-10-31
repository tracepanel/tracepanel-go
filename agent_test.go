package tracepanel

import (
	"bufio"
	"database/sql"
	"encoding/binary"
	"encoding/json"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tracepanel/tracepanel-go/internal/agentbin"
)

func readFrame(t *testing.T, reader io.Reader) []byte {
	t.Helper()
	header := make([]byte, 4)
	if _, err := io.ReadFull(reader, header); err != nil {
		t.Fatal(err)
	}
	body := make([]byte, binary.BigEndian.Uint32(header))
	if _, err := io.ReadFull(reader, body); err != nil {
		t.Fatal(err)
	}
	return body
}

func writeFrame(t *testing.T, conn net.Conn, value any) {
	t.Helper()
	body, _ := json.Marshal(value)
	frame := binary.BigEndian.AppendUint32(nil, uint32(len(body)))
	if _, err := conn.Write(append(frame, body...)); err != nil {
		t.Fatal(err)
	}
}

// A fake agent on a socket: it reads the hello, asks for an EXPLAIN, and reads the answer and the traces.
func TestTheAgentGetsTheHelloTheTracesAndAnEXPLAINOnTheAppsDatabase(t *testing.T) {
	fake.reset()
	t.Setenv("TRACEPANEL_N_PLUS_ONE_THRESHOLD", "5")
	t.Setenv("TRACEPANEL_AGENT", "")
	if _, err := sql.Open(WrapDriver("fakedb", DBAlias("reports")), "shop"); err != nil {
		t.Fatal(err)
	}
	dir, err := os.MkdirTemp("", "tp")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	socket := filepath.Join(dir, "agent.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	if err := os.WriteFile(filepath.Join(dir, "agent.addr"), []byte("unix:"+socket), 0o644); err != nil {
		t.Fatal(err)
	}
	s := newSender(&starter{storeDir: dir}, Config{Service: "shop", StoreDir: dir, MaxTraces: 500})
	s.submit(&traceDoc{ID: "01M4ZYCP0KQZZPC6SD1RJ4PBY5", Kind: "http", Attrs: attrMap{}})

	conn, err := listener.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	reader := bufio.NewReader(conn)
	line, _ := reader.ReadBytes('\n')
	var hello map[string]any
	_ = json.Unmarshal(line, &hello)
	_, _ = conn.Write([]byte(`{"ok": true}` + "\n"))
	writeFrame(t, conn, map[string]any{"id": 1, "explain": map[string]any{
		"sql": "EXPLAIN SELECT title FROM books WHERE author_id = $1", "params": []any{7}, "vendor": "postgresql",
		"alias": "reports",
	}})
	frames := map[string]any{}
	for range 2 {
		var frame any
		_ = json.Unmarshal(readFrame(t, reader), &frame)
		if _, ok := frame.([]any); ok {
			frames["traces"] = frame
		} else {
			frames["answer"] = frame
		}
	}

	if !equalJSON(hello, map[string]any{
		"protocol": 1, "max_traces": 500, "thresholds": map[string]any{"n_plus_one": 5}, "service": "shop",
		"capabilities": []string{"explain"},
	}) {
		t.Fatalf("hello %v", hello)
	}
	if traces := frames["traces"].([]any); len(traces) != 1 || traces[0].(map[string]any)["id"] != "01M4ZYCP0KQZZPC6SD1RJ4PBY5" {
		t.Fatalf("traces %v", frames["traces"])
	}
	if !equalJSON(frames["answer"], map[string]any{
		"answer": 1, "columns": []string{"QUERY PLAN"},
		"rows": [][]any{{"Seq Scan on books  (cost=0.00..1.03 rows=3 width=36)"}, {"  Filter: (author_id = 7)"}},
	}) {
		t.Fatalf("answer %v", frames["answer"])
	}
	// It ran with the query's parameter, and was rolled back.
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if fake.rolledBack != 1 || fake.committed != 0 || len(fake.args) != 1 || fake.args[0][0].Value != int64(7) {
		t.Fatalf("rolled back %d, committed %d, args %v", fake.rolledBack, fake.committed, fake.args)
	}
}

func TestAnEXPLAINOnADatabaseTheAppDoesntHaveSaysSo(t *testing.T) {
	_, err := explain(explainRequest{SQL: "EXPLAIN SELECT 1", Alias: "nowhere"})

	if err == nil || !strings.Contains(err.Error(), "no database nowhere") {
		t.Fatal(err)
	}
}

func TestW3CTraceContext(t *testing.T) {
	rec := newRecording("http", "http", "GET /", attrMap{})

	header := outgoing(rec, "01M4ZYCP0KQZZPC6SD1RJ4PBY5")

	if !traceparent.MatchString(header) || !strings.HasSuffix(header, "-"+w3cSpan("01M4ZYCP0KQZZPC6SD1RJ4PBY5")+"-01") {
		t.Fatalf("traceparent %q", header)
	}
	// The trace keeps the W3C id its calls carry; one it was called with is kept as it is.
	if !strings.Contains(header, rec.attr("w3c.trace_id").(string)) {
		t.Fatalf("w3c.trace_id %v", rec.attr("w3c.trace_id"))
	}
	if got := incoming("00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"); !equalJSON(got, attrMap{
		"w3c.trace_id": "4bf92f3577b34da6a3ce929d0e0e4736", "w3c.parent_span_id": "00f067aa0ba902b7",
	}) {
		t.Fatalf("incoming %v", got)
	}
	if got := incoming("00-00000000000000000000000000000000-00f067aa0ba902b7-01"); got != nil {
		t.Fatalf("an all-zero trace id: %v", got)
	}
	if got := w3cSpan("01M4ZYCP0KQZZPC6SD1RJ4PBY5"); len(got) != 16 {
		t.Fatalf("span id %q", got)
	}
}

// The agent the SDK downloads is AgentVersion: its checksums were generated for it.
func TestTheAgentsChecksumsAreOfAgentVersion(t *testing.T) {
	if agentbin.Version != AgentVersion {
		t.Fatalf("internal/agentbin/integrity.go is of %s, AgentVersion is %s: run make integrity", agentbin.Version, AgentVersion)
	}
}
