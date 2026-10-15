package tracepanel

import (
	"fmt"
	"testing"
)

// deep is a trace of n spans, each the child of the one before, ending children first as OpenTelemetry ends them.
func deep(n int) *Trace {
	spans := make([]Span, 0, n)
	for i := n - 1; i >= 0; i-- {
		var parent *string
		if i > 0 {
			id := fmt.Sprintf("s%d", i-1)
			parent = &id
		}
		spans = append(spans, Span{ID: fmt.Sprintf("s%d", i), ParentID: parent, Kind: "function", Start: int64(i)})
	}
	return &Trace{ID: "T", Attrs: Attrs{}, Spans: spans}
}

func TestARunawayTraceIsCutToItsEarliestSpansEachWithItsParent(t *testing.T) {
	trace := limited(deep(maxSpans + 500))
	kept := map[string]bool{}
	for _, span := range trace.Spans {
		kept[span.ID] = true
	}

	if len(trace.Spans) != maxSpans {
		t.Fatalf("kept %d spans, want %d", len(trace.Spans), maxSpans)
	}
	if !kept["s0"] {
		t.Fatal("the root went")
	}
	for _, span := range trace.Spans {
		if span.ParentID != nil && !kept[*span.ParentID] {
			t.Fatalf("%s kept without its parent %s", span.ID, *span.ParentID)
		}
	}
	if got := trace.Attrs["tracepanel.dropped_spans"]; got != 500 {
		t.Fatalf("dropped_spans = %v, want 500", got)
	}
}

func TestATraceWithinTheLimitIsSentAsItIs(t *testing.T) {
	trace := limited(deep(3))

	if len(trace.Spans) != 3 || len(trace.Attrs) != 0 {
		t.Fatalf("got %d spans and attrs %v", len(trace.Spans), trace.Attrs)
	}
}
