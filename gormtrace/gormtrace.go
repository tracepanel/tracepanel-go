// Package gormtrace records the SQL of GORM v2 as tracepanel.Driver does for database/sql: each statement a `sql`
// span with its parameters, the rows it returned (or changed) and the line of the app's code that ran it, and each
// transaction a span around its statements, so the agent finds N+1 queries, duplicates and slow SQL.
//
//	db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{})
//	err = db.Use(gormtrace.Plugin())
//
// A statement is recorded when it runs with the context of a traced request: db.WithContext(ctx).Find(&books).
//
// The rows a query returned are recorded for Find, First, Pluck, Count and the rest, and for Scan (Raw(...).Scan,
// Select(...).Scan), whose count GORM tells its logger once it has scanned them: the plugin wraps the logger
// (gorm.Config.Logger, whichever it is: logger.Discard, the app's own, or none) to read it. A Session with a Logger
// of its own (or Debug on it) keeps the wrapped one only when it was set before db.Use, and so does a logger
// assigned to db.Logger after it. Not for Rows() and Row(): the app reads the rows itself, from a *sql.Rows (or
// *sql.Row) that database/sql hands over and nothing can watch; tracepanel.Driver as the dialector's DriverName
// counts them, as the driver hands each row over.
//
// Use the plugin or tracepanel.Driver as the dialector's DriverName, not both: a database GORM opened through
// tracepanel.Driver (or WrapConnector) is recorded already, and the plugin leaves it to it.
package gormtrace

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sync"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	tracepanel "github.com/tracepanel/tracepanel-go"
	"github.com/tracepanel/tracepanel-go/internal/hook"
)

// Plugin is the GORM plugin for db.Use, with the database's options as tracepanel.Driver takes them:
// tracepanel.DBAlias names a second database, tracepanel.DBName the database shown with its queries, and
// tracepanel.DBSystem its kind when the dialector's name ("postgres", "mysql", "sqlite", "sqlserver") doesn't tell.
// When Tracepanel is off (outside development), it registers nothing.
func Plugin(options ...tracepanel.DBOption) gorm.Plugin {
	opts := make([]any, len(options))
	for i, option := range options {
		opts[i] = option
	}
	return &plugin{db: hook.Database(opts)}
}

type plugin struct {
	db hook.DB
}

func (p *plugin) Name() string { return "tracepanel" }

// registrar is a callback GORM is to place before or after one of its own: what Register needs.
type registrar = interface {
	Register(name string, fn func(*gorm.DB)) error
}

// callbacks are the places of the plugin's callbacks, for each of GORM's operations: right before and right after
// GORM's own that runs the statement, inside the transaction GORM begins for a write, and before a query's preloads
// (which are statements of their own).
func callbacks(db *gorm.DB) map[string][2]registrar {
	cb := db.Callback()
	return map[string][2]registrar{
		"create": {
			cb.Create().After("gorm:save_before_associations").Before("gorm:create"),
			cb.Create().After("gorm:create").Before("gorm:save_after_associations"),
		},
		"query": {cb.Query().Before("gorm:query"), cb.Query().After("gorm:query").Before("gorm:preload")},
		"update": {
			cb.Update().After("gorm:save_before_associations").Before("gorm:update"),
			cb.Update().After("gorm:update").Before("gorm:save_after_associations"),
		},
		"delete": {
			cb.Delete().After("gorm:delete_before_associations").Before("gorm:delete"),
			cb.Delete().After("gorm:delete").Before("gorm:after_delete"),
		},
		"row": {cb.Row().Before("gorm:row"), cb.Row().After("gorm:row")},
		// Scan runs its query as Rows(): a row callback.
		"raw": {cb.Raw().Before("gorm:raw"), cb.Raw().After("gorm:raw")},
	}
}

func (p *plugin) Initialize(db *gorm.DB) error {
	if !hook.Records() {
		return nil
	}
	sqlDB, err := db.DB()
	if err == nil && hook.Recorded(sqlDB) {
		// Opened through tracepanel.Driver: its statements and transactions are recorded already.
		return nil
	}
	if p.db.System == "" && db.Dialector != nil {
		p.db.System = hook.System(db.Dialector.Name())
	}
	if p.db.System == "" {
		p.db.System = "other_sql"
	}
	if err == nil {
		hook.Explain(p.db, sqlDB)
	} else {
		sqlDB = nil
	}
	for operation, places := range callbacks(db) {
		before := p.before
		if operation == "row" {
			before = p.beforeRows
		}
		if err := places[0].Register("tracepanel:before_"+operation, before); err != nil {
			return err
		}
		if err := places[1].Register("tracepanel:after_"+operation, p.after(operation)); err != nil {
			return err
		}
	}
	// Transactions are begun on the connection pool: wrapped, it records each. Under GORM's prepared statements, the
	// pool they prepare on.
	if prepared, ok := db.ConnPool.(*gorm.PreparedStmtDB); ok {
		prepared.ConnPool = &pool{ConnPool: prepared.ConnPool, plugin: p, sqlDB: sqlDB}
	} else if db.ConnPool != nil {
		db.ConnPool = &pool{ConnPool: db.ConnPool, plugin: p, sqlDB: sqlDB}
	}
	if db.Statement != nil {
		db.Statement.ConnPool = db.ConnPool
	}
	// Scan tells its logger how many rows it scanned: the logger, wrapped, gives them to the statement's span. Any
	// logger the app chose (logger.Discard, its own), and none: GORM's Scan calls one, so a nil one is Discard.
	if db.Config != nil {
		switch current := db.Config.Logger.(type) {
		case *rowsLogger:
		case nil:
			db.Config.Logger = &rowsLogger{Interface: logger.Discard}
		default:
			db.Config.Logger = &rowsLogger{Interface: current}
		}
	}
	return nil
}

// ---------- statements ----------

const startedKey = "tracepanel:started"

// started is a statement under way: when it began, and the error the statement had before it.
type started struct {
	at  time.Time
	err error
}

func (p *plugin) before(db *gorm.DB) {
	if db.Statement == nil || !hook.Traced(db.Statement.Context) {
		return
	}
	db.InstanceSet(startedKey, started{at: time.Now(), err: db.Error})
}

// beforeRows is before for Row(), Rows() and Scan, which runs as Rows(): the statement's context carries where its
// span will be, for the logger that Scan tells its rows.
func (p *plugin) beforeRows(db *gorm.DB) {
	p.before(db)
	if _, ok := db.InstanceGet(startedKey); ok {
		db.Statement.Context = context.WithValue(db.Statement.Context, scannedKey{}, &scanned{})
	}
}

// scanned is the span of a Rows() query, whose rows Scan counts later, and its statement; scannedKey is where its
// context carries it.
type scanned struct {
	span hook.Span
	stmt *gorm.Statement
}

type scannedKey struct{}

func (p *plugin) after(operation string) func(*gorm.DB) {
	return func(db *gorm.DB) {
		value, ok := db.InstanceGet(startedKey)
		if !ok {
			return
		}
		begun := value.(started)
		stmt := db.Statement
		query := stmt.SQL.String()
		if query == "" || db.DryRun {
			return
		}
		name, attrs := hook.Statement(p.db, "gorm", query, stmt.Vars)
		failed := db.Error != nil && db.Error != begun.err && !errors.Is(db.Error, gorm.ErrRecordNotFound)
		if failed {
			attrs["error.type"] = fmt.Sprintf("%T", db.Error)
		}
		switch {
		case operation == "query":
			attrs["db.response.returned_rows"] = db.RowsAffected
		case operation != "row" && db.RowsAffected >= 0:
			attrs["db.response.affected_rows"] = db.RowsAffected
		}
		var under hook.Span
		if tx := transactionOf(stmt.ConnPool); tx != nil {
			under = tx.span
		}
		span := hook.DoneSpan(stmt.Context, under, "sql", name, attrs, begun.at, failed)
		if pending, ok := stmt.Context.Value(scannedKey{}).(*scanned); ok && span != nil {
			if _, rows := stmt.Dest.(*sql.Rows); rows {
				pending.span, pending.stmt = span, stmt
			}
		}
	}
}

// rowsLogger is the app's GORM logger, which Scan tells how many rows it scanned (as RowsAffected): they go to the
// span of the Rows() query it ran.
type rowsLogger struct {
	logger.Interface
}

var _ gorm.ParamsFilter = (*rowsLogger)(nil)

func (l *rowsLogger) LogMode(level logger.LogLevel) logger.Interface {
	return &rowsLogger{Interface: l.Interface.LogMode(level)}
}

func (l *rowsLogger) Trace(ctx context.Context, begin time.Time, fc func() (string, int64), err error) {
	if pending, ok := ctx.Value(scannedKey{}).(*scanned); ok && pending.span != nil {
		// A Rows() query GORM ran itself tells -1; Scan, once it scanned them, their number.
		if sql, rows := fc(); rows >= 0 && sql != "" {
			pending.span.Set("db.response.returned_rows", scannedRows(pending.stmt, rows))
			pending.span, pending.stmt = nil, nil
		}
	}
	l.Interface.Trace(ctx, begin, fc, err)
}

// scannedRows is the rows Scan scanned, by GORM's count: but for a destination GORM scans as one value without
// counting it (a pointer to a pointer, `var avg *float64; Scan(&avg)`, a named type of a number or a string), which
// it says is 0. Scan leaves the statement's destination the *sql.Rows of its query when there was no row, and puts
// its own there once it scans one: it read one row then.
func scannedRows(stmt *gorm.Statement, rows int64) int64 {
	if rows != 0 || stmt == nil {
		return rows
	}
	if _, none := stmt.Dest.(*sql.Rows); none || stmt.Dest == nil {
		return 0
	}
	return 1
}

// ParamsFilter is the app's logger's, if it has one: GORM asks it for the parameters a logged statement shows.
func (l *rowsLogger) ParamsFilter(ctx context.Context, sql string, params ...any) (string, []any) {
	if filter, ok := l.Interface.(gorm.ParamsFilter); ok {
		return filter.ParamsFilter(ctx, sql, params...)
	}
	return sql, params
}

// ---------- transactions ----------

// pool is GORM's connection pool, its transactions recorded.
type pool struct {
	gorm.ConnPool
	plugin *plugin
	// The database/sql pool under it, for db.DB().
	sqlDB *sql.DB
}

var (
	_ gorm.ConnPoolBeginner = (*pool)(nil)
	_ gorm.GetDBConnector   = (*pool)(nil)
)

// BeginTx begins a transaction on the pool, recorded when ctx is a traced request's.
func (c *pool) BeginTx(ctx context.Context, opts *sql.TxOptions) (gorm.ConnPool, error) {
	var begun gorm.ConnPool
	switch beginner := c.ConnPool.(type) {
	case gorm.TxBeginner:
		tx, err := beginner.BeginTx(ctx, opts)
		if err != nil {
			return nil, err
		}
		begun = tx
	case gorm.ConnPoolBeginner:
		tx, err := beginner.BeginTx(ctx, opts)
		if err != nil {
			return nil, err
		}
		begun = tx
	default:
		return nil, gorm.ErrInvalidTransaction
	}
	inner, ok := begun.(gorm.Tx)
	if !ok {
		return begun, nil
	}
	db := c.plugin.db
	attrs := map[string]any{
		"db.system": db.System, "db.vendor": db.System, "db.client": "gorm", "db.alias": db.Alias,
		"db.namespace": db.Namespace, "code.origin": nilIfEmpty(hook.Origin()),
	}
	span := hook.Begin(ctx, "transaction", "transaction "+db.Namespace, attrs)
	if span == nil {
		return begun, nil
	}
	return &transaction{Tx: inner, span: span, sqlDB: c.sqlDB}, nil
}

func (c *pool) GetDBConn() (*sql.DB, error) {
	if c.sqlDB != nil {
		return c.sqlDB, nil
	}
	if connector, ok := c.ConnPool.(gorm.GetDBConnector); ok {
		return connector.GetDBConn()
	}
	return nil, gorm.ErrInvalidDB
}

// transaction is a transaction GORM began, recorded until its commit or its rollback.
type transaction struct {
	gorm.Tx
	span  hook.Span
	sqlDB *sql.DB
	once  sync.Once
}

var (
	_ gorm.Tx             = (*transaction)(nil)
	_ gorm.GetDBConnector = (*transaction)(nil)
)

func (t *transaction) Commit() error {
	err := t.Tx.Commit()
	outcome := "commit"
	if err != nil {
		outcome = "rollback"
	}
	t.end(outcome)
	return err
}

func (t *transaction) Rollback() error {
	err := t.Tx.Rollback()
	t.end("rollback")
	return err
}

func (t *transaction) end(outcome string) {
	t.once.Do(func() {
		t.span.Set("db.transaction.outcome", outcome)
		t.span.End(false)
	})
}

func (t *transaction) GetDBConn() (*sql.DB, error) {
	if t.sqlDB != nil {
		return t.sqlDB, nil
	}
	return nil, gorm.ErrInvalidDB
}

// transactionOf is the recorded transaction a statement runs in: its connection pool, or the one GORM's prepared
// statements run on.
func transactionOf(conn gorm.ConnPool) *transaction {
	switch tx := conn.(type) {
	case *transaction:
		return tx
	case *gorm.PreparedStmtTX:
		recorded, _ := tx.Tx.(*transaction)
		return recorded
	}
	return nil
}

// TxContext is ctx for the work of the transaction `tx` (the *gorm.DB of db.Transaction's function, or of
// db.Begin()), begun with ctx: what runs with it while `tx` is open (an outgoing HTTP call, a log record, a span) goes
// under the transaction's span, as tracepanel.TxContext for database/sql:
//
//	err := db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
//		ctx := gormtrace.TxContext(ctx, tx)
//		// ...
//	})
//
// Its statements go under the transaction already, with ctx or without.
func TxContext(ctx context.Context, tx *gorm.DB) context.Context {
	if tx == nil || tx.Statement == nil {
		return ctx
	}
	recorded := transactionOf(tx.Statement.ConnPool)
	if recorded == nil {
		return ctx
	}
	return hook.TxContext(ctx, recorded.span)
}

func nilIfEmpty(text string) any {
	if text == "" {
		return nil
	}
	return text
}
