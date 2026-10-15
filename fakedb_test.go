package tracepanel

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
)

// fakedb is a database/sql driver for the tests: a query returns the rows of its table, `books` three of them, an
// EXPLAIN its plan as one text column. It remembers what ran, and whether a transaction was rolled back.

type fakeDriver struct {
	mu          sync.Mutex
	ran         []string
	args        [][]driver.NamedValue
	rolledBack  int
	committed   int
	skipQueries bool // its connections decline queries with arguments, as MySQL's does without interpolation
}

var fake = &fakeDriver{}

func init() { sql.Register("fakedb", fake) }

func (d *fakeDriver) Open(string) (driver.Conn, error) { return &fakeConn{driver: d}, nil }

func (d *fakeDriver) reset() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.ran, d.args, d.rolledBack, d.committed, d.skipQueries = nil, nil, 0, 0, false
}

func (d *fakeDriver) record(query string, args []driver.NamedValue) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.ran = append(d.ran, query)
	d.args = append(d.args, args)
}

type fakeConn struct{ driver *fakeDriver }

func (c *fakeConn) Prepare(query string) (driver.Stmt, error) {
	return &fakeStmt{conn: c, query: query}, nil
}
func (c *fakeConn) Close() error              { return nil }
func (c *fakeConn) Begin() (driver.Tx, error) { return &fakeTx{driver: c.driver}, nil }

func (c *fakeConn) BeginTx(context.Context, driver.TxOptions) (driver.Tx, error) {
	return &fakeTx{driver: c.driver}, nil
}

func (c *fakeConn) QueryContext(_ context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	c.driver.mu.Lock()
	skip := c.driver.skipQueries && len(args) > 0
	c.driver.mu.Unlock()
	if skip {
		return nil, driver.ErrSkip
	}
	c.driver.record(query, args)
	return rowsFor(query)
}

func (c *fakeConn) ExecContext(_ context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	c.driver.record(query, args)
	if strings.Contains(query, "missing") {
		return nil, errors.New("no such table: missing")
	}
	return driver.RowsAffected(1), nil
}

type fakeStmt struct {
	conn  *fakeConn
	query string
}

func (s *fakeStmt) Close() error  { return nil }
func (s *fakeStmt) NumInput() int { return -1 }

func (s *fakeStmt) Exec(args []driver.Value) (driver.Result, error) {
	s.conn.driver.record(s.query, named(args))
	return driver.RowsAffected(1), nil
}

func (s *fakeStmt) Query(args []driver.Value) (driver.Rows, error) {
	s.conn.driver.record(s.query, named(args))
	return rowsFor(s.query)
}

type fakeTx struct{ driver *fakeDriver }

func (t *fakeTx) Commit() error {
	t.driver.mu.Lock()
	defer t.driver.mu.Unlock()
	t.driver.committed++
	return nil
}

func (t *fakeTx) Rollback() error {
	t.driver.mu.Lock()
	defer t.driver.mu.Unlock()
	t.driver.rolledBack++
	return nil
}

func rowsFor(query string) (driver.Rows, error) {
	if strings.HasPrefix(query, "EXPLAIN") {
		return &fakeRows{columns: []string{"QUERY PLAN"}, rows: [][]driver.Value{
			{[]byte("Seq Scan on books  (cost=0.00..1.03 rows=3 width=36)")},
			{[]byte("  Filter: (author_id = 7)")},
		}}, nil
	}
	if strings.Contains(query, "books") {
		rows := [][]driver.Value{}
		for i := 1; i <= 3; i++ {
			rows = append(rows, []driver.Value{int64(i), fmt.Sprintf("Book %d", i)})
		}
		return &fakeRows{columns: []string{"id", "title"}, rows: rows}, nil
	}
	return &fakeRows{columns: []string{"n"}, rows: [][]driver.Value{{int64(1)}}}, nil
}

type fakeRows struct {
	columns []string
	rows    [][]driver.Value
	at      int
}

func (r *fakeRows) Columns() []string { return r.columns }
func (r *fakeRows) Close() error      { return nil }

func (r *fakeRows) Next(dest []driver.Value) error {
	if r.at >= len(r.rows) {
		return io.EOF
	}
	copy(dest, r.rows[r.at])
	r.at++
	return nil
}
