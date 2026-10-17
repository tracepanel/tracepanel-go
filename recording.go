package tracepanel

import (
	"context"
	"sync"
	"time"
)

// A trace while it runs: a request, a task, a command. Its spans nest as the work does: each span's context carries
// it, and the work done with that context goes under it. The trace goes to the agent once its root ends.

type contextKey struct{}

// scope is what a context carries: the trace being recorded, and the span new work goes under.
type scope struct {
	rec  *recording
	span string
	// The transaction TxContext gave the context: new work goes under it while it is open, then under span again.
	txn *openTx
}

// parent is the span new work of the context goes under.
func (s *scope) parent() string {
	if s.txn != nil && s.rec.isOpen(s.txn.span) {
		return s.txn.span.ID
	}
	return s.span
}

func scopeOf(ctx context.Context) *scope {
	if ctx == nil {
		return nil
	}
	s, _ := ctx.Value(contextKey{}).(*scope)
	return s
}

func within(ctx context.Context, rec *recording, span string) context.Context {
	return context.WithValue(ctx, contextKey{}, &scope{rec: rec, span: span})
}

type recording struct {
	mu       sync.Mutex
	doc      traceDoc
	spans    []*spanDoc
	root     *spanDoc
	finished bool
	queries  int
	dbTime   int64
	// The handler's span, where a panic in the request happened.
	view string
}

func newRecording(kind, rootKind, name string, attrs attrMap) *recording {
	now := micros(time.Now())
	rec := &recording{doc: traceDoc{
		ID: newID(), Kind: kind, Name: name, Start: now, Status: "ok", Attrs: attrs,
		Logs: []logDoc{}, Errors: []errorDoc{}, Links: []linkDoc{}, Issues: []any{},
	}}
	rec.root = &spanDoc{ID: newID(), Kind: rootKind, Name: name, Start: now, Status: "ok", Attrs: attrMap{}, open: true}
	rec.spans = []*spanDoc{rec.root}
	return rec
}

// begin opens a span under `parent` (the root when empty), until end.
func (r *recording) begin(parent, kind, name string, attrs attrMap) *spanDoc {
	return r.add(parent, newID(), kind, name, attrs, micros(time.Now()), true)
}

// add records a span with its id and start given, open until end or done already.
func (r *recording) add(parent, id, kind, name string, attrs attrMap, start int64, open bool) *spanDoc {
	if parent == "" {
		parent = r.root.ID
	}
	span := &spanDoc{ID: id, ParentID: &parent, Kind: kind, Name: name, Start: start, Status: "ok", Attrs: attrs, open: open}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.finished {
		return span
	}
	// Spans are added as they start: past the limit, every later one goes, its children with it.
	if len(r.spans) < maxSpans {
		r.spans = append(r.spans, span)
	} else {
		r.dropped("tracepanel.dropped_spans", 1)
	}
	return span
}

// done records a span that already happened, from `start` until now.
func (r *recording) done(parent, id, kind, name string, attrs attrMap, start time.Time, failed bool) *spanDoc {
	span := r.add(parent, id, kind, name, attrs, micros(start), true)
	r.end(span, failed)
	return span
}

func (r *recording) end(span *spanDoc, failed bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.finished || !span.open {
		return
	}
	span.open = false
	span.Duration = micros(time.Now()) - span.Start
	if failed {
		span.Status = "error"
	}
	if span.Kind == "sql" {
		r.queries++
		r.dbTime += span.Duration
	}
}

// isOpen tells whether a span of the trace still runs.
func (r *recording) isOpen(span *spanDoc) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return !r.finished && span.open
}

// setSpan sets a span's attribute after it started: the rows a query returned, read later.
func (r *recording) setSpan(span *spanDoc, key string, value any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.finished {
		span.Attrs[key] = value
	}
}

func (r *recording) set(key string, value any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.finished {
		r.doc.Attrs[key] = value
	}
}

func (r *recording) attr(key string) any {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.doc.Attrs[key]
}

func (r *recording) rename(name string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.finished {
		r.doc.Name = name
		r.root.Name = name
	}
}

func (r *recording) fail() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.finished {
		r.doc.Status = "error"
		r.root.Status = "error"
	}
}

func (r *recording) log(record logDoc) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.finished {
		return
	}
	if len(r.doc.Logs) >= maxLogs {
		r.dropped("tracepanel.dropped_logs", 1)
		return
	}
	r.doc.Logs = append(r.doc.Logs, record)
}

func (r *recording) addError(record errorDoc) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.finished {
		r.doc.Errors = append(r.doc.Errors, record)
	}
}

func (r *recording) link(kind, traceID, spanID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.finished {
		r.doc.Links = append(r.doc.Links, linkDoc{Kind: kind, TraceID: traceID, SpanID: spanID})
	}
}

func (r *recording) setView(span string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.view = span
}

// current is the span a panic or an error of the trace happened in: the handler's, else the root.
func (r *recording) current() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.view != "" {
		return r.view
	}
	return r.root.ID
}

// timing is what the badge shows before the trace ends: its queries so far, their time, and the time so far (ms).
func (r *recording) timing() (queries int, dbMs, totalMs float64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.queries, float64(r.dbTime) / 1000, float64(micros(time.Now())-r.doc.Start) / 1000
}

// dropped adds n to the trace's tally of what went unrecorded (the caller holds the lock).
func (r *recording) dropped(key string, n int) {
	before, _ := r.doc.Attrs[key].(int)
	r.doc.Attrs[key] = before + n
}

// finish ends the trace: its document, as the agent takes it. A span still open lasts until now.
func (r *recording) finish() *traceDoc {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := micros(time.Now())
	r.finished = true
	r.doc.Duration = now - r.doc.Start
	spans := make([]spanDoc, 0, len(r.spans))
	for _, span := range r.spans {
		if span.open {
			span.Duration = now - span.Start
		}
		spans = append(spans, *span)
	}
	doc := r.doc
	doc.Spans = spans
	return &doc
}
