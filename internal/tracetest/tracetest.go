// Package tracetest records the traces of an integration's tests, as the agent would get them.
package tracetest

import (
	"encoding/json"
	"sync"
	"testing"
	"time"

	// The core fills the hooks in.
	_ "github.com/tracepanel/tracepanel-go"
	"github.com/tracepanel/tracepanel-go/internal/hook"
)

// Doc is a trace, a span or a log record as JSON reads it.
type Doc = map[string]any

// Recorder keeps the traces ended while it records.
type Recorder struct {
	mu     sync.Mutex
	traces []Doc
}

// Record turns Tracepanel on for the test (service "shop"): its traces are kept by the recorder.
func Record(t testing.TB) *Recorder {
	t.Helper()
	r := &Recorder{}
	restore := hook.Capture("shop", func(encoded []byte) {
		var doc Doc
		if err := json.Unmarshal(encoded, &doc); err != nil {
			panic(err)
		}
		r.mu.Lock()
		defer r.mu.Unlock()
		r.traces = append(r.traces, doc)
	})
	t.Cleanup(restore)
	return r
}

// Wait is the traces ended so far, once there are n.
func (r *Recorder) Wait(t testing.TB, n int) []Doc {
	t.Helper()
	for deadline := time.Now().Add(3 * time.Second); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
		r.mu.Lock()
		got := append([]Doc{}, r.traces...)
		r.mu.Unlock()
		if len(got) >= n {
			return got
		}
	}
	t.Fatalf("fewer than %d traces", n)
	return nil
}

// Spans are a trace's spans of one kind.
func Spans(trace Doc, kind string) []Doc {
	var out []Doc
	for _, span := range List(trace, "spans") {
		if span["kind"] == kind {
			out = append(out, span)
		}
	}
	return out
}

// List is a trace's list of documents: "spans", "logs", "errors", "links".
func List(trace Doc, key string) []Doc {
	var out []Doc
	items, _ := trace[key].([]any)
	for _, item := range items {
		out = append(out, item.(Doc))
	}
	return out
}

// Root is a trace's root span.
func Root(trace Doc) Doc {
	for _, span := range List(trace, "spans") {
		if span["parent_id"] == nil {
			return span
		}
	}
	return nil
}

// Attrs are a document's attributes.
func Attrs(doc Doc) Doc {
	attrs, _ := doc["attrs"].(Doc)
	return attrs
}
