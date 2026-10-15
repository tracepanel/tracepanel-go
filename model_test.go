package tracepanel

import (
	"testing"
	"time"
)

func TestARunawayTraceKeepsItsFirstSpansAndRecordsAndCountsTheRest(t *testing.T) {
	rec := newRecording("http", "http", "GET /", Attrs{})
	for range maxSpans + 100 {
		rec.end(rec.begin("", "function", "step", Attrs{}), false)
	}
	for range maxLogs + 5 {
		rec.log(logDoc{Level: "info", Message: "again", Attrs: Attrs{}})
	}

	doc := rec.finish()

	if len(doc.Spans) != maxSpans || len(doc.Logs) != maxLogs {
		t.Fatalf("kept %d spans and %d records", len(doc.Spans), len(doc.Logs))
	}
	if doc.Spans[0].ID != rec.root.ID {
		t.Fatal("the root went")
	}
	// The root is among the spans kept: 101 of the steps went.
	if got, want := doc.Attrs, (Attrs{"tracepanel.dropped_spans": 101, "tracepanel.dropped_logs": 5}); !equalJSON(got, want) {
		t.Fatalf("attrs = %v, want %v", got, want)
	}
}

func TestASpanStillOpenWhenItsTraceEndsLastsUntilThen(t *testing.T) {
	rec := newRecording("http", "http", "GET /", Attrs{})
	open := rec.begin("", "function", "never ended", Attrs{})
	time.Sleep(2 * time.Millisecond)

	doc := rec.finish()

	if doc.Spans[1].ID != open.ID || doc.Spans[1].Duration < 2000 {
		t.Fatalf("span %+v in a trace of %d µs", doc.Spans[1], doc.Duration)
	}
	// Nothing joins a trace that ended.
	rec.log(logDoc{Message: "late"})
	if len(rec.doc.Logs) != 0 {
		t.Fatal("a record joined an ended trace")
	}
}
