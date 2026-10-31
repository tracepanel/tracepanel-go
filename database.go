package tracepanel

import (
	"context"
	"crypto/sha1"
	"database/sql"
	"database/sql/driver"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// SQL through database/sql, whatever runs it (the app's own queries, sqlx, GORM, sqlc, Bun on a *sql.DB): each
// statement a `sql` span with its parameters, the rows it returned and the line of the app's code that ran it, and
// each transaction a span around its statements. A statement is recorded when it runs with the context of a traced
// request: db.QueryContext(ctx, ...), not db.Query(...).

// Driver is the name of the driver `name` with its statements recorded, for sql.Open (and sqlx.Open, GORM's
// DriverName, anything that opens by a driver's name):
//
//	db, err := sql.Open(tracepanel.Driver("pgx"), dsn)
//
// The first call registers it with database/sql, as "tracepanel:pgx". When Tracepanel is off (outside development),
// it is `name`.
func Driver(name string, options ...DBOption) string {
	if !records() {
		return name
	}
	db := newDatabase(options)
	key := fmt.Sprintf("%s\x00%s\x00%s\x00%s", name, db.alias, db.system, db.namespace)
	driversMu.Lock()
	defer driversMu.Unlock()
	if registered, ok := drivers[key]; ok {
		return registered
	}
	registered := "tracepanel:" + name
	if db.alias != "default" {
		registered += ":" + db.alias
	}
	for n := 2; taken(registered); n++ {
		registered = fmt.Sprintf("tracepanel:%s:%s#%d", name, db.alias, n)
	}
	sql.Register(registered, &tracedDriver{name: name, db: db})
	drivers[key] = registered
	return registered
}

var (
	driversMu sync.Mutex
	drivers   = map[string]string{}
)

func taken(name string) bool {
	for _, known := range sql.Drivers() {
		if known == name {
			return true
		}
	}
	return false
}

// WrapConnector is a driver's connector with its statements recorded, for sql.OpenDB:
//
//	db := sql.OpenDB(tracepanel.WrapConnector(stdlib.GetConnector(*config)))
//
// When Tracepanel is off (outside development), it is `connector`.
func WrapConnector(connector driver.Connector, options ...DBOption) driver.Connector {
	if !records() {
		return connector
	}
	return traced(connector, newDatabase(options))
}

// newDatabase is a database as its options tell it: its system comes from its driver when they don't.
func newDatabase(options []DBOption) *database {
	db := &database{alias: "default"}
	for _, option := range options {
		option(db)
	}
	if db.namespace == "" {
		db.namespace = db.alias
	}
	return db
}

// traced records the statements of `connector`'s connections, and keeps an untraced pool on it for EXPLAIN.
func traced(connector driver.Connector, template *database) driver.Connector {
	db := *template
	if db.system == "" {
		db.system = systemOf(driverPackage(connector.Driver()))
	}
	if db.system == "" {
		db.system = "other_sql"
	}
	db.sql = sql.OpenDB(connector)
	remember(&db)
	return &tracedConnector{inner: connector, db: &db}
}

// tracedDriver is a registered driver's stand-in: it opens through that driver, and records its statements.
type tracedDriver struct {
	name string
	db   *database
}

func (d *tracedDriver) Open(dsn string) (driver.Conn, error) {
	connector, err := d.OpenConnector(dsn)
	if err != nil {
		return nil, err
	}
	return connector.Connect(context.Background())
}

func (d *tracedDriver) OpenConnector(dsn string) (driver.Connector, error) {
	// database/sql hands out a registered driver only through a DB of it.
	probe, err := sql.Open(d.name, dsn)
	if err != nil {
		return nil, err
	}
	inner := probe.Driver()
	_ = probe.Close()
	var connector driver.Connector = dsnConnector{dsn: dsn, driver: inner}
	if opener, ok := inner.(driver.DriverContext); ok {
		if connector, err = opener.OpenConnector(dsn); err != nil {
			return nil, err
		}
	}
	db := *d.db
	if db.system == "" {
		db.system = systemOf(d.name)
	}
	return traced(connector, &db), nil
}

// DBOption tells Driver or WrapConnector more about the database.
type DBOption func(*database)

// DBAlias names the database among the app's ("default"): a second one, such as a replica, gets a name of its own.
func DBAlias(name string) DBOption { return func(db *database) { db.alias = name } }

// DBSystem is the database's kind, as OpenTelemetry names it ("postgresql", "mysql", "sqlite"), when the driver's
// name doesn't tell.
func DBSystem(name string) DBOption {
	return func(db *database) {
		if name != "" {
			db.system = name
		}
	}
}

// DBName is the database's name, shown with its queries (the alias otherwise).
func DBName(name string) DBOption { return func(db *database) { db.namespace = name } }

// systemOf is a database's kind from its driver's name or package, as OpenTelemetry names it: "pgx" and lib/pq are
// "postgresql", gosnowflake's "snowflake" is "snowflake".
func systemOf(name string) string {
	name = strings.ToLower(name)
	for _, known := range []struct{ part, system string }{
		{"pgx", "postgresql"}, {"lib/pq", "postgresql"}, {"postgres", "postgresql"}, {"pq", "postgresql"},
		{"mysql", "mysql"}, {"mariadb", "mariadb"}, {"sqlite", "sqlite"},
		{"sqlserver", "mssql"}, {"mssql", "mssql"}, {"godror", "oracle"}, {"go-ora", "oracle"}, {"oracle", "oracle"},
		{"clickhouse", "clickhouse"}, {"duckdb", "duckdb"},
		// The analytical warehouses, which the agent holds to limits of their own.
		{"snowflake", "snowflake"}, {"bigquery", "bigquery"}, {"databricks", "databricks"}, {"redshift", "redshift"},
		{"trino", "trino"},
	} {
		if name == known.part || strings.Contains(name, known.part) && len(known.part) > 2 {
			return known.system
		}
	}
	return ""
}

func driverPackage(d driver.Driver) string {
	kind := reflect.TypeOf(d)
	for kind != nil && kind.Kind() == reflect.Pointer {
		kind = kind.Elem()
	}
	if kind == nil {
		return ""
	}
	return kind.PkgPath()
}

type database struct {
	alias, system, namespace string
	sql                      *sql.DB
}

func (db *database) attrs() attrMap {
	return attrMap{
		"db.system": db.system, "db.vendor": db.system, "db.client": "database/sql", "db.alias": db.alias,
		"db.namespace": db.namespace,
	}
}

var (
	databasesMu sync.Mutex
	databases   = map[string]*database{}
)

// remember keeps the app's databases by alias: EXPLAIN runs on the one a query came from.
func remember(db *database) {
	databasesMu.Lock()
	defer databasesMu.Unlock()
	databases[db.alias] = db
}

// hasDatabases: the app opened a database through Driver or WrapConnector, which EXPLAIN can run on.
func hasDatabases() bool {
	databasesMu.Lock()
	defer databasesMu.Unlock()
	return len(databases) > 0
}

func databaseNamed(alias string) *database {
	databasesMu.Lock()
	defer databasesMu.Unlock()
	return databases[alias]
}

type dsnConnector struct {
	dsn    string
	driver driver.Driver
}

func (c dsnConnector) Connect(context.Context) (driver.Conn, error) { return c.driver.Open(c.dsn) }
func (c dsnConnector) Driver() driver.Driver                        { return c.driver }

type tracedConnector struct {
	inner driver.Connector
	db    *database
}

func (c *tracedConnector) Connect(ctx context.Context) (driver.Conn, error) {
	inner, err := c.inner.Connect(ctx)
	if err != nil {
		return nil, err
	}
	return &tracedConn{inner: inner, db: c.db}, nil
}

func (c *tracedConnector) Driver() driver.Driver { return c.inner.Driver() }

// ---------- a connection ----------

type tracedConn struct {
	inner driver.Conn
	db    *database
	// The transaction open on this connection, which its statements go under.
	mu  sync.Mutex
	txn *openTx
}

type openTx struct {
	rec  *recording
	span *spanDoc
}

var (
	_ driver.Conn               = (*tracedConn)(nil)
	_ driver.ConnPrepareContext = (*tracedConn)(nil)
	_ driver.ConnBeginTx        = (*tracedConn)(nil)
	_ driver.ExecerContext      = (*tracedConn)(nil)
	_ driver.QueryerContext     = (*tracedConn)(nil)
	_ driver.Pinger             = (*tracedConn)(nil)
	_ driver.SessionResetter    = (*tracedConn)(nil)
	_ driver.Validator          = (*tracedConn)(nil)
	_ driver.NamedValueChecker  = (*tracedConn)(nil)
)

func (c *tracedConn) Prepare(query string) (driver.Stmt, error) {
	return c.PrepareContext(context.Background(), query)
}

func (c *tracedConn) PrepareContext(ctx context.Context, query string) (driver.Stmt, error) {
	if probe, ok := ctx.Value(txProbe{}).(*openTx); ok {
		// TxContext asks which transaction is open on this connection: nothing is prepared.
		c.mu.Lock()
		if c.txn != nil {
			*probe = *c.txn
		}
		c.mu.Unlock()
		return nil, errTxProbe
	}
	var inner driver.Stmt
	var err error
	if preparer, ok := c.inner.(driver.ConnPrepareContext); ok {
		inner, err = preparer.PrepareContext(ctx, query)
	} else {
		inner, err = c.inner.Prepare(query)
	}
	if err != nil {
		return nil, err
	}
	return &tracedStmt{inner: inner, conn: c, query: query}, nil
}

func (c *tracedConn) Close() error {
	c.endTx("rollback")
	return c.inner.Close()
}

func (c *tracedConn) Begin() (driver.Tx, error) {
	return c.BeginTx(context.Background(), driver.TxOptions{})
}

func (c *tracedConn) BeginTx(ctx context.Context, options driver.TxOptions) (driver.Tx, error) {
	var inner driver.Tx
	var err error
	if beginner, ok := c.inner.(driver.ConnBeginTx); ok {
		inner, err = beginner.BeginTx(ctx, options)
	} else {
		// The driver has no BeginTx: its Begin is all there is.
		inner, err = c.inner.Begin()
	}
	if err != nil {
		return nil, err
	}
	if s := scopeOf(ctx); s != nil {
		attrs := c.db.attrs()
		attrs["code.origin"] = nilIfEmpty(origin())
		span := s.rec.begin(s.parent(), "transaction", "transaction "+c.db.namespace, attrs)
		c.mu.Lock()
		c.txn = &openTx{rec: s.rec, span: span}
		c.mu.Unlock()
	}
	return &tracedTx{inner: inner, conn: c}, nil
}

// endTx ends the connection's transaction with its outcome.
func (c *tracedConn) endTx(outcome string) {
	c.mu.Lock()
	txn := c.txn
	c.txn = nil
	c.mu.Unlock()
	if txn != nil {
		txn.rec.setSpan(txn.span, "db.transaction.outcome", outcome)
		txn.rec.end(txn.span, false)
	}
}

// parent is where a statement of `s`'s trace goes: under the connection's transaction, else the context's span.
func (c *tracedConn) parent(s *scope) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.txn != nil && c.txn.rec == s.rec {
		return c.txn.span.ID
	}
	return s.parent()
}

func (c *tracedConn) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	execer, ok := c.inner.(driver.ExecerContext)
	if !ok {
		// database/sql prepares the statement then: the statement records it.
		return nil, driver.ErrSkip
	}
	statement := c.begin(ctx, query, args)
	result, err := execer.ExecContext(ctx, query, args)
	statement.end(err, nil)
	return result, err
}

func (c *tracedConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	queryer, ok := c.inner.(driver.QueryerContext)
	if !ok {
		return nil, driver.ErrSkip
	}
	statement := c.begin(ctx, query, args)
	rows, err := queryer.QueryContext(ctx, query, args)
	return statement.end(err, rows), err
}

func (c *tracedConn) Ping(ctx context.Context) error {
	if pinger, ok := c.inner.(driver.Pinger); ok {
		return pinger.Ping(ctx)
	}
	return nil
}

func (c *tracedConn) ResetSession(ctx context.Context) error {
	if resetter, ok := c.inner.(driver.SessionResetter); ok {
		return resetter.ResetSession(ctx)
	}
	return nil
}

func (c *tracedConn) IsValid() bool {
	if validator, ok := c.inner.(driver.Validator); ok {
		return validator.IsValid()
	}
	return true
}

func (c *tracedConn) CheckNamedValue(value *driver.NamedValue) error {
	if checker, ok := c.inner.(driver.NamedValueChecker); ok {
		return checker.CheckNamedValue(value)
	}
	return driver.ErrSkip
}

// ---------- a statement ----------

type tracedStmt struct {
	inner driver.Stmt
	conn  *tracedConn
	query string
}

var (
	_ driver.StmtExecContext   = (*tracedStmt)(nil)
	_ driver.StmtQueryContext  = (*tracedStmt)(nil)
	_ driver.NamedValueChecker = (*tracedStmt)(nil)
)

func (s *tracedStmt) Close() error  { return s.inner.Close() }
func (s *tracedStmt) NumInput() int { return s.inner.NumInput() }

func (s *tracedStmt) Exec(args []driver.Value) (driver.Result, error) {
	return s.ExecContext(context.Background(), named(args))
}

func (s *tracedStmt) Query(args []driver.Value) (driver.Rows, error) {
	return s.QueryContext(context.Background(), named(args))
}

func (s *tracedStmt) ExecContext(ctx context.Context, args []driver.NamedValue) (driver.Result, error) {
	statement := s.conn.begin(ctx, s.query, args)
	var result driver.Result
	var err error
	if execer, ok := s.inner.(driver.StmtExecContext); ok {
		result, err = execer.ExecContext(ctx, args)
	} else {
		result, err = s.inner.Exec(values(args))
	}
	statement.end(err, nil)
	return result, err
}

func (s *tracedStmt) QueryContext(ctx context.Context, args []driver.NamedValue) (driver.Rows, error) {
	statement := s.conn.begin(ctx, s.query, args)
	var rows driver.Rows
	var err error
	if queryer, ok := s.inner.(driver.StmtQueryContext); ok {
		rows, err = queryer.QueryContext(ctx, args)
	} else {
		rows, err = s.inner.Query(values(args))
	}
	return statement.end(err, rows), err
}

func (s *tracedStmt) CheckNamedValue(value *driver.NamedValue) error {
	if checker, ok := s.inner.(driver.NamedValueChecker); ok {
		return checker.CheckNamedValue(value)
	}
	return s.conn.CheckNamedValue(value)
}

func named(args []driver.Value) []driver.NamedValue {
	out := make([]driver.NamedValue, len(args))
	for i, value := range args {
		out[i] = driver.NamedValue{Ordinal: i + 1, Value: value}
	}
	return out
}

func values(args []driver.NamedValue) []driver.Value {
	out := make([]driver.Value, len(args))
	for i, arg := range args {
		out[i] = arg.Value
	}
	return out
}

// ---------- a transaction ----------

// TxContext is ctx for the work of the transaction `tx`, begun with ctx: what runs with it while `tx` is open (an
// outgoing HTTP call, a log record, a span, a query) goes under the transaction's span, and after the commit or the
// rollback under ctx's span again. database/sql keeps the context a transaction began with to itself, so the app
// asks for this one:
//
//	tx, err := db.BeginTx(ctx, nil)
//	ctx = tracepanel.TxContext(ctx, tx)
//
// Outside a trace, or for a transaction that isn't recorded, it is ctx.
func TxContext(ctx context.Context, tx *sql.Tx) context.Context {
	s := scopeOf(ctx)
	if s == nil || tx == nil {
		return ctx
	}
	// The transaction's connection answers a prepare carrying the probe with its open transaction.
	found := &openTx{}
	_, _ = tx.PrepareContext(context.WithValue(ctx, txProbe{}, found), "")
	if found.span == nil || found.rec != s.rec {
		return ctx
	}
	return context.WithValue(ctx, contextKey{}, &scope{rec: s.rec, span: s.parent(), txn: found})
}

type txProbe struct{}

var errTxProbe = errors.New("tracepanel: a probe for the open transaction")

type tracedTx struct {
	inner driver.Tx
	conn  *tracedConn
}

func (t *tracedTx) Commit() error {
	err := t.inner.Commit()
	outcome := "commit"
	if err != nil {
		outcome = "rollback"
	}
	t.conn.endTx(outcome)
	return err
}

func (t *tracedTx) Rollback() error {
	err := t.inner.Rollback()
	t.conn.endTx("rollback")
	return err
}

// ---------- recording a statement ----------

// statement is one statement running, when its context is a traced request's: recorded once it ran, as a driver
// may decline it (driver.ErrSkip) for database/sql to prepare it, and then the prepared one is what ran.
type statement struct {
	rec    *recording
	parent string
	name   string
	attrs  attrMap
	start  time.Time
}

// The most parameters a statement keeps, and the most characters of each.
const (
	maxParams     = 200
	maxParamChars = 2000
)

func (c *tracedConn) begin(ctx context.Context, query string, args []driver.NamedValue) statement {
	s := scopeOf(ctx)
	if s == nil {
		return statement{}
	}
	raw := make([]any, len(args))
	for i, arg := range args {
		raw[i] = arg.Value
	}
	name, attrs := statementAttrs(c.db, query, raw)
	return statement{rec: s.rec, parent: c.parent(s), name: name, attrs: attrs, start: time.Now()}
}

// statementAttrs are a statement's span name and attributes, whatever library ran it: its one-line text, its
// fingerprint, its parameters, and the line of the app's code that ran it.
func statementAttrs(db *database, query string, args []any) (string, attrMap) {
	params := make([]any, 0, min(len(args), maxParams))
	for _, arg := range args[:min(len(args), maxParams)] {
		params = append(params, param(arg))
	}
	encoded, _ := json.Marshal(params)
	digest := sha1.Sum(encoded)
	attrs := db.attrs()
	stmt := parse(query)
	attrs["db.statement"] = stmt.text
	attrs["db.fingerprint"] = stmt.fingerprint
	attrs["db.params"] = params
	attrs["db.params_hash"] = nil
	if len(params) > 0 {
		attrs["db.params_hash"] = hex.EncodeToString(digest[:])[:12]
	}
	attrs["db.many"] = false
	attrs["code.origin"] = nilIfEmpty(origin())
	attrs["code.template"] = nil
	return stmt.name, attrs
}

// end records the statement as it ran; a query's rows are counted as they are read.
func (st statement) end(err error, rows driver.Rows) driver.Rows {
	if st.rec == nil || errors.Is(err, driver.ErrSkip) {
		return rows
	}
	if err != nil {
		st.attrs["error.type"] = shortType(err)
	}
	if rows != nil {
		st.attrs["db.response.returned_rows"] = 0
	}
	span := st.rec.done(st.parent, newID(), "sql", st.name, st.attrs, st.start, err != nil)
	if rows == nil {
		return rows
	}
	return &countedRows{inner: rows, rec: st.rec, span: span}
}

// param is a recorded parameter as JSON keeps it, and as EXPLAIN binds it again.
func param(value any) any {
	switch v := value.(type) {
	case nil, bool, int64, float64:
		return v
	case string:
		return truncate(v)
	case []byte:
		if utf8.Valid(v) {
			return truncate(string(v))
		}
		return fmt.Sprintf("<%d bytes>", len(v))
	case time.Time:
		return v.Format(time.RFC3339Nano)
	case driver.Valuer:
		inner, err := v.Value()
		if err != nil {
			return truncate(fmt.Sprint(value))
		}
		return param(inner)
	default:
		// An integer or a float of any size, as a library other than database/sql passes it.
		switch number := reflect.ValueOf(v); number.Kind() {
		case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
			return number.Int()
		case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
			return number.Uint()
		case reflect.Float32, reflect.Float64:
			return number.Float()
		}
		return truncate(fmt.Sprint(v))
	}
}

func truncate(text string) string {
	if len(text) <= maxParamChars {
		return text
	}
	return text[:maxParamChars-1] + "…"
}

// countedRows counts the rows a query returned as the app reads them.
type countedRows struct {
	inner driver.Rows
	rec   *recording
	span  *spanDoc
	count int
}

var (
	_ driver.RowsNextResultSet              = (*countedRows)(nil)
	_ driver.RowsColumnTypeScanType         = (*countedRows)(nil)
	_ driver.RowsColumnTypeDatabaseTypeName = (*countedRows)(nil)
	_ driver.RowsColumnTypeLength           = (*countedRows)(nil)
	_ driver.RowsColumnTypeNullable         = (*countedRows)(nil)
	_ driver.RowsColumnTypePrecisionScale   = (*countedRows)(nil)
)

func (r *countedRows) Columns() []string { return r.inner.Columns() }

func (r *countedRows) Close() error {
	r.rec.setSpan(r.span, "db.response.returned_rows", r.count)
	return r.inner.Close()
}

func (r *countedRows) Next(dest []driver.Value) error {
	err := r.inner.Next(dest)
	if err == nil {
		r.count++
	}
	return err
}

func (r *countedRows) HasNextResultSet() bool {
	if sets, ok := r.inner.(driver.RowsNextResultSet); ok {
		return sets.HasNextResultSet()
	}
	return false
}

func (r *countedRows) NextResultSet() error {
	if sets, ok := r.inner.(driver.RowsNextResultSet); ok {
		return sets.NextResultSet()
	}
	return io.EOF
}

func (r *countedRows) ColumnTypeScanType(index int) reflect.Type {
	if typed, ok := r.inner.(driver.RowsColumnTypeScanType); ok {
		return typed.ColumnTypeScanType(index)
	}
	return reflect.TypeFor[any]()
}

func (r *countedRows) ColumnTypeDatabaseTypeName(index int) string {
	if typed, ok := r.inner.(driver.RowsColumnTypeDatabaseTypeName); ok {
		return typed.ColumnTypeDatabaseTypeName(index)
	}
	return ""
}

func (r *countedRows) ColumnTypeLength(index int) (int64, bool) {
	if typed, ok := r.inner.(driver.RowsColumnTypeLength); ok {
		return typed.ColumnTypeLength(index)
	}
	return 0, false
}

func (r *countedRows) ColumnTypeNullable(index int) (bool, bool) {
	if typed, ok := r.inner.(driver.RowsColumnTypeNullable); ok {
		return typed.ColumnTypeNullable(index)
	}
	return false, false
}

func (r *countedRows) ColumnTypePrecisionScale(index int) (int64, int64, bool) {
	if typed, ok := r.inner.(driver.RowsColumnTypePrecisionScale); ok {
		return typed.ColumnTypePrecisionScale(index)
	}
	return 0, 0, false
}
