package tracepanel

import (
	"crypto/rand"
	"encoding/binary"
	"sort"
	"strings"
	"time"
)

// The trace as every tracepanel SDK sends it to the agent.

type Attrs map[string]any

type Trace struct {
	ID       string  `json:"id"`
	Kind     string  `json:"kind"`
	Name     string  `json:"name"`
	Start    int64   `json:"start"`
	Duration int64   `json:"duration"`
	Status   string  `json:"status"`
	Attrs    Attrs   `json:"attrs"`
	Spans    []Span  `json:"spans"`
	Logs     []Log   `json:"logs"`
	Errors   []Error `json:"errors"`
	Links    []any   `json:"links"`
	Issues   []any   `json:"issues"`
}

type Span struct {
	ID       string  `json:"id"`
	ParentID *string `json:"parent_id"`
	Kind     string  `json:"kind"`
	Name     string  `json:"name"`
	Start    int64   `json:"start"`
	Duration int64   `json:"duration"`
	Status   string  `json:"status"`
	Attrs    Attrs   `json:"attrs"`
}

type Log struct {
	SpanID    *string `json:"span_id"`
	Time      int64   `json:"time"`
	Level     string  `json:"level"`
	Logger    string  `json:"logger"`
	Message   string  `json:"message"`
	Exception *string `json:"exception"`
	Attrs     Attrs   `json:"attrs"`
}

type Error struct {
	SpanID  *string `json:"span_id"`
	Time    int64   `json:"time"`
	Type    string  `json:"type"`
	Message string  `json:"message"`
	Handled bool    `json:"handled"`
	Frames  []Frame `json:"frames"`
	Chain   []any   `json:"chain"`
}

type Frame struct {
	Filename     string            `json:"filename"`
	Module       string            `json:"module"`
	Function     string            `json:"function"`
	Lineno       int               `json:"lineno"`
	InApp        bool              `json:"in_app"`
	ContextStart *int              `json:"context_start"`
	Context      []string          `json:"context"`
	Locals       map[string]string `json:"locals"`
}

func micros(t time.Time) int64 { return t.UnixMicro() }

const crockford = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

// newID is a ULID: 48 bits of milliseconds, 80 random bits, 26 characters of Crockford base32.
func newID() string {
	var raw [16]byte
	binary.BigEndian.PutUint64(raw[0:8], uint64(time.Now().UnixMilli())<<16)
	_, _ = rand.Read(raw[6:])
	hi, lo := binary.BigEndian.Uint64(raw[0:8]), binary.BigEndian.Uint64(raw[8:16])
	var out strings.Builder
	for i := 25; i >= 0; i-- {
		shift := uint(i * 5)
		var digit uint64
		switch {
		case shift >= 64:
			digit = hi >> (shift - 64)
		case shift > 59:
			digit = lo>>shift | hi<<(64-shift)
		default:
			digit = lo >> shift
		}
		out.WriteByte(crockford[digit&31])
	}
	return out.String()
}

// The most spans a trace is sent with, as the agent stores it; the rest are counted (tracepanel.dropped_spans).
const maxSpans = 2000

// The most log records a trace keeps; the rest are counted (tracepanel.dropped_logs).
const maxLogs = 1000

// countDropped adds n to a trace's tally of what went unrecorded.
func countDropped(trace *Trace, key string, n int) {
	if trace.Attrs == nil {
		trace.Attrs = Attrs{}
	}
	before, _ := trace.Attrs[key].(int)
	trace.Attrs[key] = before + n
}

// limited cuts a trace to maxSpans: its earliest spans, its roots always. Spans end children first, so a cut as they
// end would keep children whose parent went: by start, a parent always comes before its children.
func limited(trace *Trace) *Trace {
	if len(trace.Spans) <= maxSpans {
		return trace
	}
	order := make([]int, len(trace.Spans))
	for i := range order {
		order[i] = i
	}
	sort.SliceStable(order, func(a, b int) bool {
		x, y := trace.Spans[order[a]], trace.Spans[order[b]]
		if (x.ParentID == nil) != (y.ParentID == nil) {
			return x.ParentID == nil
		}
		return x.Start < y.Start
	})
	keep := make(map[int]bool, maxSpans)
	for _, index := range order[:maxSpans] {
		keep[index] = true
	}
	spans := make([]Span, 0, maxSpans)
	for index, span := range trace.Spans {
		if keep[index] {
			spans = append(spans, span)
		}
	}
	countDropped(trace, "tracepanel.dropped_spans", len(trace.Spans)-len(spans))
	trace.Spans = spans
	return trace
}
