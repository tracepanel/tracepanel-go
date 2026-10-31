// Package pgxtrace records the SQL of pgx v5 (a pgx.Conn or a pgxpool.Pool) as tracepanel.Driver does for
// database/sql: each statement a `sql` span with its parameters, the rows it returned and the line of the app's
// code that ran it, and each transaction a span around its statements.
//
//	config, err := pgxpool.ParseConfig(dsn)
//	config.ConnConfig.Tracer = pgxtrace.Tracer()
//	pool, err := pgxpool.NewWithConfig(ctx, config)
//
// A statement is recorded when it runs with the context of a traced request: pool.Query(ctx, ...).
package pgxtrace

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"sync"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/stdlib"

	tracepanel "github.com/tracepanel/tracepanel-go"
	"github.com/tracepanel/tracepanel-go/internal/hook"
)

// tracer is a pgx tracer recording statements, batches and copies.
type tracer struct {
	db hook.DB
	// The transaction open on each connection, which its statements go under.
	txns sync.Map // *pgx.Conn → *openTx
	// The pool EXPLAIN runs on, opened once from the first connection's config.
	explain sync.Once
}

var (
	_ pgx.QueryTracer    = (*tracer)(nil)
	_ pgx.BatchTracer    = (*tracer)(nil)
	_ pgx.CopyFromTracer = (*tracer)(nil)
)

// Tracer is the tracer of a ConnConfig (a pgxpool.Config's ConnConfig): it records statements, batches and copies
// (a pgx.QueryTracer, BatchTracer and CopyFromTracer). It takes the database's options as tracepanel.Driver takes
// them: tracepanel.DBAlias names a second database, tracepanel.DBName the database shown with its queries.
func Tracer(options ...tracepanel.DBOption) pgx.QueryTracer {
	opts := make([]any, len(options))
	for i, option := range options {
		opts[i] = option
	}
	db := hook.Database(opts)
	if db.System == "" {
		db.System = "postgresql"
	}
	return &tracer{db: db}
}

type openTx struct {
	span hook.Span
}

type queryKey struct{}

// running is a statement under way, from its start to its end.
type running struct {
	span hook.Span
	// A transaction's own statement (begin, commit, rollback): not a statement of the app's.
	control string
}

func (t *tracer) TraceQueryStart(ctx context.Context, conn *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	if !hook.Enabled() {
		return ctx
	}
	t.remember(conn)
	switch control := transactionControl(data.SQL); control {
	case "begin":
		if span := t.begin(ctx, conn, "transaction", "transaction "+t.db.Namespace, t.attrs()); span != nil {
			t.txns.Store(conn, &openTx{span: span})
		}
		return context.WithValue(ctx, queryKey{}, &running{control: control})
	case "commit", "rollback":
		return context.WithValue(ctx, queryKey{}, &running{control: control})
	}
	query, args := rewrite(ctx, conn, data.SQL, data.Args)
	name, attrs := hook.Statement(t.db, "pgx", query, args)
	span := t.begin(ctx, conn, "sql", name, attrs)
	if span == nil {
		return ctx
	}
	return context.WithValue(ctx, queryKey{}, &running{span: span})
}

func (t *tracer) TraceQueryEnd(ctx context.Context, conn *pgx.Conn, data pgx.TraceQueryEndData) {
	run, _ := ctx.Value(queryKey{}).(*running)
	if run == nil {
		return
	}
	switch run.control {
	case "begin":
		if data.Err != nil {
			t.endTx(conn, "rollback", true)
		}
		return
	case "commit":
		outcome := "commit"
		// PostgreSQL answers COMMIT of a failed transaction with ROLLBACK.
		if data.Err != nil || data.CommandTag.String() == "ROLLBACK" {
			outcome = "rollback"
		}
		t.endTx(conn, outcome, false)
		return
	case "rollback":
		t.endTx(conn, "rollback", false)
		return
	}
	end(run.span, data.CommandTag, data.Err)
}

// end ends a statement's span with its outcome: the rows a SELECT returned, the error's type.
func end(span hook.Span, tag pgconn.CommandTag, err error) {
	if err != nil {
		span.Set("error.type", errorType(err))
	} else if tag.Select() {
		span.Set("db.response.returned_rows", int(tag.RowsAffected()))
	}
	span.End(err != nil)
}

// begin opens a span of the trace in ctx: under the connection's transaction when it is that trace's.
func (t *tracer) begin(ctx context.Context, conn *pgx.Conn, kind, name string, attrs map[string]any) hook.Span {
	if open, ok := t.txns.Load(conn); ok && open.(*openTx).span.SameTrace(ctx) {
		return open.(*openTx).span.Child(kind, name, attrs)
	}
	return hook.Begin(ctx, kind, name, attrs)
}

func (t *tracer) endTx(conn *pgx.Conn, outcome string, failed bool) {
	open, ok := t.txns.LoadAndDelete(conn)
	if !ok {
		return
	}
	span := open.(*openTx).span
	span.Set("db.transaction.outcome", outcome)
	span.End(failed)
}

func (t *tracer) attrs() map[string]any {
	return map[string]any{
		"db.system": t.db.System, "db.vendor": t.db.System, "db.client": "pgx", "db.alias": t.db.Alias,
		"db.namespace": t.db.Namespace, "code.origin": nilIfEmpty(hook.Origin()),
	}
}

// remember lets the console run EXPLAIN on the database, through a database/sql pool of the first connection's
// config: it connects only when the console asks.
func (t *tracer) remember(conn *pgx.Conn) {
	t.explain.Do(func() {
		hook.Explain(t.db, sql.OpenDB(stdlib.GetConnector(*conn.Config())))
	})
}

// ---------- batches and copies ----------

type batchKey struct{}

func (t *tracer) TraceBatchStart(ctx context.Context, conn *pgx.Conn, _ pgx.TraceBatchStartData) context.Context {
	if !hook.Enabled() {
		return ctx
	}
	t.remember(conn)
	return context.WithValue(ctx, batchKey{}, true)
}

// TraceBatchQuery records each query of a batch, as it is read: the batch goes to the server at once, so they are
// db.many, one round trip.
func (t *tracer) TraceBatchQuery(ctx context.Context, conn *pgx.Conn, data pgx.TraceBatchQueryData) {
	if ctx.Value(batchKey{}) == nil {
		return
	}
	name, attrs := hook.Statement(t.db, "pgx", data.SQL, data.Args)
	attrs["db.many"] = true
	span := t.begin(ctx, conn, "sql", name, attrs)
	if span != nil {
		end(span, data.CommandTag, data.Err)
	}
}

func (t *tracer) TraceBatchEnd(context.Context, *pgx.Conn, pgx.TraceBatchEndData) {}

func (t *tracer) TraceCopyFromStart(ctx context.Context, conn *pgx.Conn, data pgx.TraceCopyFromStartData) context.Context {
	if !hook.Enabled() {
		return ctx
	}
	t.remember(conn)
	columns := make([]string, len(data.ColumnNames))
	for i, column := range data.ColumnNames {
		columns[i] = pgx.Identifier{column}.Sanitize()
	}
	query := "COPY " + data.TableName.Sanitize() + " (" + strings.Join(columns, ", ") + ") FROM STDIN"
	_, attrs := hook.Statement(t.db, "pgx", query, nil)
	attrs["db.many"] = true
	name := "COPY " + strings.Join(data.TableName, ".")
	span := t.begin(ctx, conn, "sql", name, attrs)
	if span == nil {
		return ctx
	}
	return context.WithValue(ctx, queryKey{}, &running{span: span})
}

func (t *tracer) TraceCopyFromEnd(ctx context.Context, _ *pgx.Conn, data pgx.TraceCopyFromEndData) {
	run, _ := ctx.Value(queryKey{}).(*running)
	if run == nil || run.span == nil {
		return
	}
	end(run.span, data.CommandTag, data.Err)
}

// ---------- transactions ----------

// TxContext is ctx for the work of the transaction `tx`, begun with ctx: what runs with it while `tx` is open (an
// outgoing HTTP call, a log record, a span) goes under the transaction's span, as tracepanel.TxContext for
// database/sql:
//
//	tx, err := pool.Begin(ctx)
//	ctx = pgxtrace.TxContext(ctx, tx)
//
// Its statements go under the transaction already, with ctx or without.
func TxContext(ctx context.Context, tx pgx.Tx) context.Context {
	if tx == nil || tx.Conn() == nil {
		return ctx
	}
	traced, ok := tx.Conn().Config().Tracer.(*tracer)
	if !ok {
		return ctx
	}
	open, ok := traced.txns.Load(tx.Conn())
	if !ok {
		return ctx
	}
	return hook.TxContext(ctx, open.(*openTx).span)
}

// transactionControl tells a transaction's own statement: "begin" (BEGIN, START TRANSACTION, with their options),
// "commit" (COMMIT, END), "rollback" (ROLLBACK, ABORT, not to a savepoint), "" for any other.
func transactionControl(query string) string {
	fields := strings.Fields(strings.ToLower(strings.TrimSuffix(strings.TrimSpace(query), ";")))
	if len(fields) == 0 {
		return ""
	}
	switch fields[0] {
	case "begin":
		return "begin"
	case "start":
		if len(fields) > 1 && fields[1] == "transaction" {
			return "begin"
		}
	case "commit", "end":
		if len(fields) == 1 || fields[1] == "work" || fields[1] == "transaction" {
			return "commit"
		}
	case "rollback", "abort":
		if len(fields) == 1 || fields[1] == "work" || fields[1] == "transaction" {
			return "rollback"
		}
	}
	return ""
}

// rewrite is the statement as the server runs it: pgx's options (an exec mode, result formats) left out of its
// arguments, and named arguments (pgx.NamedArgs) as their $1 placeholders, so EXPLAIN can run it again.
func rewrite(ctx context.Context, conn *pgx.Conn, query string, args []any) (string, []any) {
	for len(args) > 0 {
		switch rewriter := args[0].(type) {
		case pgx.QueryExecMode, pgx.QueryResultFormats, pgx.QueryResultFormatsByOID:
			args = args[1:]
			continue
		case pgx.NamedArgs, pgx.StrictNamedArgs:
			rewritten, rest, err := rewriter.(pgx.QueryRewriter).RewriteQuery(ctx, conn, query, args[1:])
			if err != nil {
				return query, args[1:]
			}
			return rewritten, rest
		}
		break
	}
	return query, args
}

// errorType names a failed statement's error as tracepanel.Driver does: its type.
func errorType(err error) string { return fmt.Sprintf("%T", err) }

func nilIfEmpty(text string) any {
	if text == "" {
		return nil
	}
	return text
}
