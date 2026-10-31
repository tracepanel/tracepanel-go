package pgxtrace

import (
	"context"
	"net"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgproto3"

	tracepanel "github.com/tracepanel/tracepanel-go"
	"github.com/tracepanel/tracepanel-go/internal/tracetest"
)

// fakeServer answers a pgx connection over a pipe as PostgreSQL would, for the simple protocol: a SELECT returns
// two rows, BEGIN, COMMIT and ROLLBACK keep the transaction's state, a statement on "missing" fails.
func fakeServer(t *testing.T, server net.Conn) {
	backend := pgproto3.NewBackend(server, server)
	if _, err := backend.ReceiveStartupMessage(); err != nil {
		t.Error(err)
		return
	}
	backend.Send(&pgproto3.AuthenticationOk{})
	for _, param := range [][2]string{{"server_version", "17.0"}, {"standard_conforming_strings", "on"}, {"client_encoding", "UTF8"}} {
		backend.Send(&pgproto3.ParameterStatus{Name: param[0], Value: param[1]})
	}
	status := byte('I')
	backend.Send(&pgproto3.ReadyForQuery{TxStatus: status})
	_ = backend.Flush()
	for {
		message, err := backend.Receive()
		if err != nil {
			return
		}
		query, ok := message.(*pgproto3.Query)
		if !ok {
			return
		}
		for _, statement := range strings.Split(query.String, ";") {
			statement = strings.ToLower(strings.TrimSpace(statement))
			switch {
			case statement == "":
			case strings.Contains(statement, "missing"):
				backend.Send(&pgproto3.ErrorResponse{Severity: "ERROR", Code: "42P01", Message: "relation does not exist"})
			case strings.HasPrefix(statement, "select"):
				backend.Send(&pgproto3.RowDescription{Fields: []pgproto3.FieldDescription{
					{Name: []byte("n"), DataTypeOID: 23, DataTypeSize: 4, TypeModifier: -1},
				}})
				backend.Send(&pgproto3.DataRow{Values: [][]byte{[]byte("1")}})
				backend.Send(&pgproto3.DataRow{Values: [][]byte{[]byte("2")}})
				backend.Send(&pgproto3.CommandComplete{CommandTag: []byte("SELECT 2")})
			case strings.HasPrefix(statement, "begin"):
				status = 'T'
				backend.Send(&pgproto3.CommandComplete{CommandTag: []byte("BEGIN")})
			case statement == "commit":
				status = 'I'
				backend.Send(&pgproto3.CommandComplete{CommandTag: []byte("COMMIT")})
			case statement == "rollback":
				status = 'I'
				backend.Send(&pgproto3.CommandComplete{CommandTag: []byte("ROLLBACK")})
			default:
				backend.Send(&pgproto3.CommandComplete{CommandTag: []byte("INSERT 0 1")})
			}
		}
		backend.Send(&pgproto3.ReadyForQuery{TxStatus: status})
		_ = backend.Flush()
	}
}

func connect(t *testing.T, options ...tracepanel.DBOption) *pgx.Conn {
	t.Helper()
	config, err := pgx.ParseConfig("postgres://shop@127.0.0.1/shop?sslmode=disable&default_query_exec_mode=simple_protocol")
	if err != nil {
		t.Fatal(err)
	}
	config.Tracer = Tracer(options...)
	config.DialFunc = func(context.Context, string, string) (net.Conn, error) {
		client, server := net.Pipe()
		go fakeServer(t, server)
		return client, nil
	}
	conn, err := pgx.ConnectConfig(context.Background(), config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close(context.Background()) })
	return conn
}

// command runs fn in a trace of its own, and is that trace once it ended.
func command(t *testing.T, recorder *tracetest.Recorder, fn func(ctx context.Context) error) tracetest.Doc {
	t.Helper()
	_ = tracepanel.Command(context.Background(), "job", fn)
	return recorder.Wait(t, 1)[0]
}

func TestStatementsAreSQLSpans(t *testing.T) {
	recorder := tracetest.Record(t)
	conn := connect(t)

	trace := command(t, recorder, func(ctx context.Context) error {
		rows, err := conn.Query(ctx, "SELECT id\n  FROM books WHERE author_id = $1", 7)
		if err != nil {
			return err
		}
		rows.Close()
		_, err = conn.Exec(ctx, "INSERT INTO books (title) VALUES (@title)", pgx.NamedArgs{"title": "Dune"})
		return err
	})

	spans := tracetest.Spans(trace, "sql")
	if len(spans) != 2 {
		t.Fatalf("sql spans: %v", spans)
	}
	attrs := tracetest.Attrs(spans[0])
	origin, _ := attrs["code.origin"].(string)
	if !strings.HasPrefix(origin, "pgxtrace_test.go:") {
		t.Fatalf("code.origin: %v", attrs["code.origin"])
	}
	delete(attrs, "code.origin")
	want := tracetest.Doc{
		"db.system": "postgresql", "db.vendor": "postgresql", "db.client": "pgx", "db.alias": "default",
		"db.namespace": "default", "db.statement": "SELECT id FROM books WHERE author_id = $1",
		"db.fingerprint": "SELECT id FROM books WHERE author_id = $1", "db.params": []any{float64(7)},
		"db.params_hash": attrs["db.params_hash"], "db.many": false, "code.template": nil,
		"db.response.returned_rows": float64(2),
	}
	if !equal(attrs, want) {
		t.Fatalf("attrs:\n got %v\nwant %v", attrs, want)
	}
	if spans[0]["name"] != "SELECT books" {
		t.Fatalf("name: %v", spans[0]["name"])
	}

	insert := tracetest.Attrs(spans[1])
	if spans[1]["name"] != "INSERT books" || insert["db.statement"] != "INSERT INTO books (title) VALUES ($1)" ||
		!equal(insert["db.params"], []any{"Dune"}) {
		t.Fatalf("named args: %v %v", spans[1]["name"], insert)
	}
}

func TestAFailedStatementSaysWhy(t *testing.T) {
	recorder := tracetest.Record(t)
	conn := connect(t)

	trace := command(t, recorder, func(ctx context.Context) error {
		_, _ = conn.Exec(ctx, "DELETE FROM missing")
		return nil
	})

	span := tracetest.Spans(trace, "sql")[0]
	if span["status"] != "error" || tracetest.Attrs(span)["error.type"] != "*pgconn.PgError" {
		t.Fatalf("span: %v", span)
	}
}

func TestATransactionHoldsItsStatements(t *testing.T) {
	recorder := tracetest.Record(t)
	conn := connect(t, tracepanel.DBAlias("orders"))

	var inTx context.Context
	trace := command(t, recorder, func(ctx context.Context) error {
		tx, err := conn.Begin(ctx)
		if err != nil {
			return err
		}
		inTx = TxContext(ctx, tx)
		if _, err := tx.Exec(ctx, "UPDATE stock SET n = n - 1"); err != nil {
			return err
		}
		_, end := tracepanel.Span(inTx, "reserve")
		end()
		return tx.Commit(context.Background())
	})

	transactions := tracetest.Spans(trace, "transaction")
	if len(transactions) != 1 {
		t.Fatalf("transactions: %v", trace["spans"])
	}
	transaction := transactions[0]
	attrs := tracetest.Attrs(transaction)
	if transaction["name"] != "transaction orders" || attrs["db.transaction.outcome"] != "commit" || attrs["db.alias"] != "orders" {
		t.Fatalf("transaction: %v", transaction)
	}
	statements := tracetest.Spans(trace, "sql")
	if len(statements) != 1 || statements[0]["parent_id"] != transaction["id"] {
		t.Fatalf("statements: %v", statements)
	}
	work := tracetest.Spans(trace, "function")
	if len(work) != 1 || work[0]["parent_id"] != transaction["id"] {
		t.Fatalf("work in the transaction: %v", work)
	}
}

func TestARolledBackTransaction(t *testing.T) {
	recorder := tracetest.Record(t)
	conn := connect(t)

	trace := command(t, recorder, func(ctx context.Context) error {
		tx, err := conn.Begin(ctx)
		if err != nil {
			return err
		}
		return tx.Rollback(ctx)
	})

	transaction := tracetest.Spans(trace, "transaction")[0]
	if tracetest.Attrs(transaction)["db.transaction.outcome"] != "rollback" {
		t.Fatalf("transaction: %v", transaction)
	}
}

func TestABatchIsManyStatements(t *testing.T) {
	recorder := tracetest.Record(t)
	conn := connect(t)

	trace := command(t, recorder, func(ctx context.Context) error {
		batch := &pgx.Batch{}
		batch.Queue("SELECT id FROM books WHERE id = $1", 1)
		batch.Queue("SELECT id FROM books WHERE id = $1", 2)
		return conn.SendBatch(ctx, batch).Close()
	})

	spans := tracetest.Spans(trace, "sql")
	if len(spans) != 2 {
		t.Fatalf("sql spans: %v", trace["spans"])
	}
	for i, span := range spans {
		attrs := tracetest.Attrs(span)
		if attrs["db.many"] != true || !equal(attrs["db.params"], []any{float64(i + 1)}) {
			t.Fatalf("batch query %d: %v", i, attrs)
		}
	}
}

func TestACopy(t *testing.T) {
	recorder := tracetest.Record(t)
	conn := connect(t)
	traced := conn.Config().Tracer.(*tracer)

	trace := command(t, recorder, func(ctx context.Context) error {
		ctx = traced.TraceCopyFromStart(ctx, conn, pgx.TraceCopyFromStartData{
			TableName: pgx.Identifier{"books"}, ColumnNames: []string{"id", "title"},
		})
		traced.TraceCopyFromEnd(ctx, conn, pgx.TraceCopyFromEndData{CommandTag: pgconn.NewCommandTag("COPY 3")})
		return nil
	})

	span := tracetest.Spans(trace, "sql")[0]
	if span["name"] != "COPY books" || tracetest.Attrs(span)["db.statement"] != `COPY "books" ("id", "title") FROM STDIN` {
		t.Fatalf("copy: %v", span)
	}
}

func TestOutsideATraceNothingIsRecorded(t *testing.T) {
	recorder := tracetest.Record(t)
	conn := connect(t)

	if _, err := conn.Exec(context.Background(), "INSERT INTO books (title) VALUES ($1)", "x"); err != nil {
		t.Fatal(err)
	}
	trace := command(t, recorder, func(context.Context) error { return nil })

	if spans := tracetest.Spans(trace, "sql"); len(spans) != 0 {
		t.Fatalf("recorded: %v", spans)
	}
}

func TestTransactionControl(t *testing.T) {
	for query, want := range map[string]string{
		"begin": "begin", "BEGIN ISOLATION LEVEL SERIALIZABLE": "begin", "start transaction": "begin",
		"commit": "commit", "END": "commit", "rollback": "rollback", "rollback to savepoint sp_1": "",
		"savepoint sp_1": "", "SELECT 1": "", "": "",
	} {
		if got := transactionControl(query); got != want {
			t.Errorf("transactionControl(%q) = %q, want %q", query, got, want)
		}
	}
}

func equal(got, want any) bool {
	return tracetest.Equal(got, want)
}
