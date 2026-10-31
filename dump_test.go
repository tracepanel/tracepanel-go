package tracepanel

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime"
	"strings"
	"testing"
	"time"
)

type dumpItem struct {
	SKU string
	Qty int
}

type dumpOrder struct {
	ID    int
	Items []dumpItem
	note  string
}

type dumpNode struct {
	Name string
	Next *dumpNode
}

// printed is what Dump prints while the test runs.
func printed(t *testing.T) *bytes.Buffer {
	t.Helper()
	var out bytes.Buffer
	dumpMu.Lock()
	before := dumpOutput
	dumpOutput = &out
	dumpMu.Unlock()
	t.Cleanup(func() {
		dumpMu.Lock()
		dumpOutput = before
		dumpMu.Unlock()
	})
	return &out
}

// nextLine is the line after the caller's.
func nextLine() int {
	_, _, line, _ := runtime.Caller(1)
	return line + 1
}

func TestADumpInARequestIsPrintedAndRecordedOnItsSpan(t *testing.T) {
	recorded := record(t)
	out := printed(t)
	var line int
	mux := http.NewServeMux()
	mux.HandleFunc("GET /checkout", func(w http.ResponseWriter, r *http.Request) {
		order := &dumpOrder{ID: 7, Items: []dumpItem{{SKU: "tea", Qty: 2}}, note: "gift"}
		tags := []string{"new", "vip"}
		stock := map[string]int{"tea": 3, "coffee": 0}
		line = nextLine()
		Dump(r.Context(), order, tags, stock, "here")
	})
	server := httptest.NewServer(recorded.t.middleware(mux))
	defer server.Close()

	response, err := http.Get(server.URL + "/checkout")
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	doc := recorded.wait(t, 1)[0]

	message := fmt.Sprintf(`dump_test.go:%d TestADumpInARequestIsPrintedAndRecordedOnItsSpan.func1
    order: &tracepanel.dumpOrder{
        ID:    7,
        Items: []tracepanel.dumpItem{
            tracepanel.dumpItem{
                SKU: "tea",
                Qty: 2,
            },
        },
        note:  "gift",
    } (*tracepanel.dumpOrder)
    tags: []string{"new", "vip"} ([]string) len=2
    stock: map[string]int{"coffee": 0, "tea": 3} (map[string]int) len=2
    "here" (string) len=4`, line)
	if out.String() != message+"\n" {
		t.Errorf("printed:\n%s\nwant:\n%s", out.String(), message)
	}

	if len(doc.Logs) != 1 {
		t.Fatalf("logs: %+v", doc.Logs)
	}
	want := logDoc{
		SpanID: ptr(spans(doc, "view")[0].ID), Time: doc.Logs[0].Time, Level: "debug", Logger: "tracepanel",
		Message: message,
		Attrs: attrMap{
			"log.kind":  "dump",
			"dump.lang": "go",
			"dump.args": []any{
				map[string]any{
					"expr": "order",
					"repr": "&tracepanel.dumpOrder{\n    ID:    7,\n    Items: []tracepanel.dumpItem{\n" +
						"        tracepanel.dumpItem{\n            SKU: \"tea\",\n            Qty: 2,\n        },\n" +
						"    },\n    note:  \"gift\",\n}",
					"type": "*tracepanel.dumpOrder",
					"len":  nil,
				},
				map[string]any{"expr": "tags", "repr": `[]string{"new", "vip"}`, "type": "[]string", "len": 2},
				map[string]any{
					"expr": "stock", "repr": `map[string]int{"coffee": 0, "tea": 3}`, "type": "map[string]int", "len": 2,
				},
				map[string]any{"expr": nil, "repr": `"here"`, "type": "string", "len": 4},
			},
			"code.origin": fmt.Sprintf("dump_test.go:%d in TestADumpInARequestIsPrintedAndRecordedOnItsSpan.func1", line),
		},
	}
	if !equalJSON(doc.Logs[0], want) {
		t.Errorf("log:\n%+v\nwant:\n%+v", doc.Logs[0], want)
	}
}

func TestNestedPointersCyclesAndTextsArePrintedAsGo(t *testing.T) {
	loop := &dumpNode{Name: "a"}
	loop.Next = &dumpNode{Name: "b", Next: loop}
	self := []any{1}
	self[0] = self
	deep := []any{[]any{[]any{[]any{[]any{[]any{[]any{1}}}}}}}
	when := time.Date(2026, 10, 31, 9, 0, 0, 0, time.UTC)

	cases := []struct {
		value any
		want  string
	}{
		{loop, "&tracepanel.dumpNode{\n    Name: \"a\",\n    Next: &tracepanel.dumpNode{\n        Name: \"b\",\n" +
			"        Next: <cycle: *tracepanel.dumpNode>,\n    },\n}"},
		{self, "[]interface {}{<cycle: []interface {}>}"},
		{deep, "[]interface {}{\n    []interface {}{\n        []interface {}{\n" +
			"            []interface {}{[]interface {}{[]interface {}{[]interface {}{…}}}},\n        },\n    },\n}"},
		{nil, "nil"},
		{(*dumpNode)(nil), "(*tracepanel.dumpNode)(nil)"},
		{[]int(nil), "([]int)(nil)"},
		{map[int]bool{10: true, 2: false}, "map[int]bool{2: false, 10: true}"},
		{when, `time.Time("2026-10-31 09:00:00 +0000 UTC")`},
		{errors.New("boom"), `*errors.errorString("boom")`},
		{1500 * time.Millisecond, `time.Duration("1.5s")`},
		{[]byte(`{"a":1}`), `[]uint8("{\"a\":1}")`},
		{[]byte{0, 1, 0xff}, "[]uint8{0x00, 0x01, 0xff}"},
		{struct{}{}, "struct {}{}"},
		{3.5, "3.5"},
	}
	for _, c := range cases {
		if got := prettyValue(c.value); got != c.want {
			t.Errorf("%#v:\n%s\nwant:\n%s", c.value, got, c.want)
		}
	}
}

func TestAValueNamedAsASecretIsMasked(t *testing.T) {
	recorded := record(t)
	out := printed(t)
	apiKey := "k-123"

	_ = Command(context.Background(), "job", func(ctx context.Context) error {
		Dump(ctx, apiKey)
		return nil
	})
	doc := recorded.wait(t, 1)[0]

	if !strings.HasSuffix(out.String(), "\n    apiKey: ******** (string)\n") {
		t.Errorf("printed: %q", out.String())
	}

	if !equalJSON(doc.Logs[0].Attrs["dump.args"], []any{
		map[string]any{"expr": "apiKey", "repr": "********", "type": "string", "len": nil},
	}) {
		t.Errorf("dump.args: %v", doc.Logs[0].Attrs["dump.args"])
	}
}

func TestADumpOutsideATracePrintsAndRecordsNothing(t *testing.T) {
	recorded := record(t)
	out := printed(t)
	count := 3

	line := nextLine()
	Dump(context.Background(), count)

	want := fmt.Sprintf("dump_test.go:%d TestADumpOutsideATracePrintsAndRecordsNothing\n    count: 3 (int)\n", line)
	if out.String() != want {
		t.Errorf("printed %q, want %q", out.String(), want)
	}

	recorded.mu.Lock()
	defer recorded.mu.Unlock()
	if len(recorded.traces) != 0 {
		t.Errorf("traces: %+v", recorded.traces)
	}
}

func TestALongValueIsCut(t *testing.T) {
	long := strings.Repeat("é", maxRepr*2)

	got := prettyValue(long)

	if want := `"` + strings.Repeat("é", maxRepr-1) + cut; got != want {
		t.Errorf("%d characters, ending %q", len([]rune(got)), got[len(got)-40:])
	}
	if got := prettyValue(make([]int, 100_000)); len([]rune(got)) != maxRepr+len([]rune(cut)) {
		t.Errorf("a long slice: %d characters", len([]rune(got)))
	}
}

func TestAnUnreadableSourceLeavesTheExpressionsOut(t *testing.T) {
	recorded := record(t)
	_ = printed(t)
	sourcesMu.Lock()
	clear(sources)
	sourcesMu.Unlock()
	readSource = func(string) ([]byte, error) { return nil, os.ErrNotExist }
	t.Cleanup(func() {
		readSource = os.ReadFile
		sourcesMu.Lock()
		clear(sources)
		sourcesMu.Unlock()
	})
	count := 3

	_ = Command(context.Background(), "job", func(ctx context.Context) error {
		Dump(ctx, count)
		return nil
	})
	doc := recorded.wait(t, 1)[0]

	if !equalJSON(doc.Logs[0].Attrs["dump.args"], []any{
		map[string]any{"expr": nil, "repr": "3", "type": "int", "len": nil},
	}) {
		t.Errorf("dump.args: %v", doc.Logs[0].Attrs["dump.args"])
	}
}
