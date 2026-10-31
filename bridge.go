package tracepanel

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"github.com/tracepanel/tracepanel-go/internal/hook"
)

// What the integrations (each a module of its own: pgxtrace, gintrace, grpctrace...) record with: the same spans,
// traces, SQL rules and log records as the core's own, through the internal hook package.

func init() {
	hook.Enabled = func() bool { return active.Load() != nil }
	hook.Traced = func(ctx context.Context) bool { return scopeOf(ctx) != nil }
	hook.Begin = func(ctx context.Context, kind, name string, attrs map[string]any) hook.Span {
		s := scopeOf(ctx)
		if s == nil {
			return nil
		}
		return &bridgeSpan{rec: s.rec, span: s.rec.begin(s.parent(), kind, name, attrs)}
	}
	hook.Start = func(kind, rootKind, name string, attrs map[string]any) hook.Trace {
		t := active.Load()
		if t == nil {
			return nil
		}
		return &bridgeTrace{t: t, rec: t.begin(kind, rootKind, name, attrs)}
	}
	hook.SetTrace = func(ctx context.Context, key string, value any) {
		if s := scopeOf(ctx); s != nil {
			s.rec.set(key, value)
		}
	}
	hook.View = bridgeView
	hook.TxContext = func(ctx context.Context, tx hook.Span) context.Context {
		s := scopeOf(ctx)
		span, ok := tx.(*bridgeSpan)
		if s == nil || !ok || span.rec != s.rec {
			return ctx
		}
		return context.WithValue(ctx, contextKey{}, &scope{rec: s.rec, span: s.parent(), txn: &openTx{rec: span.rec, span: span.span}})
	}
	hook.Incoming = func(header string) map[string]any { return incoming(header) }
	hook.Origin = origin
	hook.Database = func(options []any) hook.DB {
		var typed []DBOption
		for _, option := range options {
			if option, ok := option.(DBOption); ok {
				typed = append(typed, option)
			}
		}
		db := newDatabase(typed)
		return hook.DB{Alias: db.alias, System: db.system, Namespace: db.namespace}
	}
	hook.Statement = func(db hook.DB, client, query string, params []any) (string, map[string]any) {
		attrs := statementAttrs(&database{alias: db.Alias, system: db.System, namespace: db.Namespace}, query, params)
		attrs["db.client"] = client
		return spanName(query), attrs
	}
	hook.Explain = func(db hook.DB, sqlDB *sql.DB) {
		remember(&database{alias: db.Alias, system: db.System, namespace: db.Namespace, sql: sqlDB})
	}
	hook.Log = bridgeLog
	hook.Capture = capture
}

type bridgeSpan struct {
	rec  *recording
	span *spanDoc
}

func (b *bridgeSpan) Set(key string, value any) { b.rec.setSpan(b.span, key, value) }
func (b *bridgeSpan) End(failed bool)           { b.rec.end(b.span, failed) }
func (b *bridgeSpan) W3CID() string             { return w3cSpan(b.span.ID) }
func (b *bridgeSpan) Traceparent() string       { return outgoing(b.rec, b.span.ID) }
func (b *bridgeSpan) SentBy() string            { return b.rec.doc.ID + ":" + b.span.ID }

func (b *bridgeSpan) Context(ctx context.Context) context.Context {
	return within(ctx, b.rec, b.span.ID)
}

func (b *bridgeSpan) Child(kind, name string, attrs map[string]any) hook.Span {
	return &bridgeSpan{rec: b.rec, span: b.rec.begin(b.span.ID, kind, name, attrs)}
}

func (b *bridgeSpan) SameTrace(ctx context.Context) bool {
	s := scopeOf(ctx)
	return s != nil && s.rec == b.rec
}

type bridgeTrace struct {
	t   *tracepanel
	rec *recording
}

func (b *bridgeTrace) Context(ctx context.Context) context.Context {
	return within(ctx, b.rec, b.rec.root.ID)
}

func (b *bridgeTrace) Rename(name string)                { b.rec.rename(name) }
func (b *bridgeTrace) Set(key string, value any)         { b.rec.set(key, value) }
func (b *bridgeTrace) Link(kind, traceID, spanID string) { b.rec.link(kind, traceID, spanID) }
func (b *bridgeTrace) Fail()                             { b.rec.fail() }
func (b *bridgeTrace) Finish()                           { b.t.finish(b.rec) }

func (b *bridgeTrace) Error(err error) {
	b.rec.addError(errorDoc{
		SpanID: ptr(b.rec.current()), Time: micros(time.Now()), Type: fmt.Sprintf("%T", err), Message: err.Error(),
		Frames: []frameDoc{}, Chain: []any{},
	})
}

func (b *bridgeTrace) Panic(recovered any) { recordPanic(b.rec, recovered, callers(1)) }

func bridgeView(ctx context.Context, function string) (context.Context, func(bool)) {
	s := scopeOf(ctx)
	if s == nil || function == "" {
		return ctx, func(bool) {}
	}
	span := s.rec.begin(s.parent(), "view", shortFunction(function), attrMap{"code.function": function})
	s.rec.setView(span.ID)
	s.rec.set("code.view", function)
	return within(ctx, s.rec, span.ID), func(failed bool) { s.rec.end(span, failed) }
}

// bridgeLog adds a logging library's record to the trace in ctx, its fields as extra.* (masked as slog's are).
func bridgeLog(ctx context.Context, record hook.Record) {
	s := scopeOf(ctx)
	if s == nil {
		return
	}
	attrs := attrMap{}
	flattenFields(attrs, "extra.", record.Fields)
	if record.File != "" {
		attrs["code.filepath"] = relative(record.File)
		attrs["code.lineno"] = record.Line
		if record.Function != "" {
			attrs["code.function"] = shortFunction(record.Function)
		}
	}
	at := record.Time
	if at.IsZero() {
		at = time.Now()
	}
	s.rec.log(logDoc{
		SpanID: ptr(s.parent()), Time: micros(at), Level: record.Level, Logger: record.Logger,
		Message: record.Message, Attrs: attrs,
	})
}

func flattenFields(attrs attrMap, prefix string, fields map[string]any) {
	keys := make([]string, 0, len(fields))
	for key := range fields {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		value := fields[key]
		if group, ok := value.(map[string]any); ok {
			flattenFields(attrs, prefix+key+".", group)
			continue
		}
		if key == "" {
			continue
		}
		if secret.MatchString(key) {
			attrs[prefix+key] = mask
			continue
		}
		attrs[prefix+key] = fieldValue(value)
	}
}

func fieldValue(value any) any {
	switch v := value.(type) {
	case nil, bool, string, int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64, float32, float64:
		return v
	case json.Number:
		return number(v)
	case error:
		return v.Error()
	case time.Time:
		return v.Format(time.RFC3339Nano)
	case time.Duration:
		return v.String()
	case fmt.Stringer:
		return v.String()
	default:
		return fmt.Sprint(v)
	}
}

// capture turns Tracepanel on with each ended trace handed to deliver, for an integration's tests.
func capture(service string, deliver func([]byte)) func() {
	module := modulePath + "/app"
	appModule.Store(&module)
	t := &tracepanel{
		config:  Config{Service: service, ConsoleURL: "http://127.0.0.1:9/"},
		console: &console{configured: "http://127.0.0.1:9/"},
		starter: &starter{storeDir: ""},
	}
	t.deliver = func(doc *traceDoc) {
		encoded, err := json.Marshal(doc)
		if err == nil {
			deliver(encoded)
		}
	}
	previous := active.Swap(t)
	return func() { active.Store(previous) }
}
