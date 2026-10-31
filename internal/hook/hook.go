// Package hook is what the integrations (pgxtrace, gintrace, grpctrace and the others, each a module of its own)
// record traces with: the core package fills these in when it loads, so the rules of a trace stay in one place.
// It is internal: an app uses the integrations, never this.
//
// The integrations of one release work with the core of that release and later ones: what is here only grows.
package hook

import (
	"context"
	"database/sql"
	"time"
)

// Span is a span an integration recorded, open until End.
type Span interface {
	// Set sets one of its attributes.
	Set(key string, value any)
	// End ends it, failed or not.
	End(failed bool)
	// Context is ctx with new work going under the span.
	Context(ctx context.Context) context.Context
	// Child opens a span under it.
	Child(kind, name string, attrs map[string]any) Span
	// SameTrace tells whether ctx's trace is the span's.
	SameTrace(ctx context.Context) bool
	// W3CID is its W3C span id: 16 hex digits.
	W3CID() string
	// Traceparent is what a call made in the span carries, for the app it reaches.
	Traceparent() string
	// SentBy is "(trace id):(span id)", which a message carries for the trace that receives it (sent_by).
	SentBy() string
}

// Trace is a trace an integration started: a gRPC call served, a message received.
type Trace interface {
	// Context is ctx with the trace's work going under its root.
	Context(ctx context.Context) context.Context
	Rename(name string)
	Set(key string, value any)
	// Link ties it to another trace's span: "sent_by" for a message, at the publish that sent it.
	Link(kind, traceID, spanID string)
	// Error adds an error it ended with.
	Error(err error)
	// Panic adds a panic with its stack: called in the deferred function that recovered it.
	Panic(recovered any)
	Fail()
	// Finish ends it and sends it to the agent.
	Finish()
}

// DB is a database as its options (tracepanel.DBAlias, DBSystem, DBName) name it.
type DB struct {
	Alias, System, Namespace string
}

// Record is a log record of a logging library, for the trace of the context it was logged with.
type Record struct {
	Time time.Time
	// "debug", "info", "warning", "error".
	Level   string
	Logger  string
	Message string
	// The record's fields: a nested map's are named by their path (extra.user.id); secret-looking ones are masked.
	Fields map[string]any
	// The line that logged it, when the library knows: File empty otherwise.
	File     string
	Line     int
	Function string
}

var (
	// Enabled tells whether Tracepanel is on.
	Enabled func() bool
	// Records tells whether what an app wraps is to be recorded: Tracepanel is on, or will be once set up (what
	// tracepanel.Driver asks).
	Records func() bool
	// Traced tells whether ctx has a trace being recorded.
	Traced func(ctx context.Context) bool
	// Begin opens a span in the trace in ctx, under the span new work of ctx goes under: nil outside a trace.
	Begin func(ctx context.Context, kind, name string, attrs map[string]any) Span
	// Done records a span that began at start and ends now, in the trace in ctx: under `under` when it is set and of
	// that trace, else under the span new work of ctx goes under. Nothing outside a trace.
	Done func(ctx context.Context, under Span, kind, name string, attrs map[string]any, start time.Time, failed bool)
	// Start begins a trace of its own: nil when Tracepanel is off.
	Start func(kind, rootKind, name string, attrs map[string]any) Trace
	// SetTrace sets an attribute of the trace in ctx.
	SetTrace func(ctx context.Context, key string, value any)
	// View is the handler's span in a request's trace, named after its function, as WrapHandler's on a ServeMux; and
	// ctx under it.
	View func(ctx context.Context, function string) (context.Context, func(failed bool))
	// TxContext is ctx for the work of the transaction whose span is tx, as tracepanel.TxContext.
	TxContext func(ctx context.Context, tx Span) context.Context
	// Incoming is a received traceparent as a trace's attributes (w3c.trace_id, w3c.parent_span_id); nil if invalid.
	Incoming func(traceparent string) map[string]any
	// Origin is "file:line in function" of the app's innermost frame on the stack.
	Origin func() string
	// Database is the database tracepanel.DBOption values describe.
	Database func(options []any) DB
	// Statement is a SQL statement's span name and attributes, as tracepanel.Driver records them; `client` names the library.
	Statement func(db DB, client, query string, params []any) (string, map[string]any)
	// System is a database's kind from its driver's or dialect's name, as OpenTelemetry names it ("postgres" is
	// "postgresql"): "" when the name doesn't tell.
	System func(name string) string
	// Recorded tells whether sqlDB's statements are recorded already: it was opened through tracepanel.Driver or
	// WrapConnector.
	Recorded func(sqlDB *sql.DB) bool
	// Explain lets the console run EXPLAIN on db's queries through sqlDB.
	Explain func(db DB, sqlDB *sql.DB)
	// Log adds a record to the trace in ctx.
	Log func(ctx context.Context, record Record)
	// Capture turns Tracepanel on for a test, each ended trace handed to deliver as the agent would get it (JSON),
	// until restore.
	Capture func(service string, deliver func(doc []byte)) (restore func())
)
