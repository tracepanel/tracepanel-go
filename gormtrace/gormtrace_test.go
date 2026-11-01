package gormtrace

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	tracepanel "github.com/tracepanel/tracepanel-go"
	"github.com/tracepanel/tracepanel-go/internal/tracetest"
)

type Author struct {
	ID   uint
	Name string
}

type Book struct {
	ID       uint
	Title    string
	AuthorID uint
	Author   Author
}

// open is a GORM database on SQLite (pure Go, no cgo) with three books by two authors, and the plugin.
func open(t *testing.T, config *gorm.Config, options ...tracepanel.DBOption) *gorm.DB {
	t.Helper()
	db := openPlain(t, sqlite.Open(filepath.Join(t.TempDir(), "shop.db")), config)
	if err := db.Use(Plugin(options...)); err != nil {
		t.Fatal(err)
	}
	return db
}

func openPlain(t *testing.T, dialector gorm.Dialector, config *gorm.Config) *gorm.DB {
	t.Helper()
	if config == nil {
		config = &gorm.Config{}
	}
	config.Logger = logger.Discard
	db, err := gorm.Open(dialector, config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if sqlDB, err := db.DB(); err == nil {
			_ = sqlDB.Close()
		}
	})
	if err := db.AutoMigrate(&Author{}, &Book{}); err != nil {
		t.Fatal(err)
	}
	authors := []Author{{Name: "Herbert"}, {Name: "Le Guin"}}
	if err := db.Create(&authors).Error; err != nil {
		t.Fatal(err)
	}
	books := []Book{
		{Title: "Dune", AuthorID: authors[0].ID}, {Title: "Children of Dune", AuthorID: authors[0].ID},
		{Title: "The Dispossessed", AuthorID: authors[1].ID},
	}
	if err := db.Create(&books).Error; err != nil {
		t.Fatal(err)
	}
	return db
}

// command runs fn in a trace of its own, and is that trace once it ended.
func command(t *testing.T, recorder *tracetest.Recorder, fn func(ctx context.Context) error) tracetest.Doc {
	t.Helper()
	if err := tracepanel.Command(context.Background(), "job", fn); err != nil {
		t.Fatal(err)
	}
	return recorder.Wait(t, 1)[0]
}

func TestAQueryIsASQLSpan(t *testing.T) {
	recorder := tracetest.Record(t)
	db := open(t, nil)

	trace := command(t, recorder, func(ctx context.Context) error {
		var books []Book
		return db.WithContext(ctx).Where("author_id = ?", 1).Order("id").Find(&books).Error
	})

	spans := tracetest.Spans(trace, "sql")
	if len(spans) != 1 {
		t.Fatalf("sql spans: %v", trace["spans"])
	}
	attrs := tracetest.Attrs(spans[0])
	origin, _ := attrs["code.origin"].(string)
	if !strings.HasPrefix(origin, "gormtrace_test.go:") {
		t.Fatalf("code.origin: %v", attrs["code.origin"])
	}
	delete(attrs, "code.origin")
	want := tracetest.Doc{
		"db.system": "sqlite", "db.vendor": "sqlite", "db.client": "gorm", "db.alias": "default",
		"db.namespace": "default", "db.statement": "SELECT * FROM `books` WHERE author_id = ? ORDER BY id",
		"db.fingerprint": "SELECT * FROM `books` WHERE author_id = ? ORDER BY id", "db.params": []any{1},
		"db.params_hash": attrs["db.params_hash"], "db.many": false, "code.template": nil,
		"db.response.returned_rows": 2,
	}
	if !tracetest.Equal(attrs, want) {
		t.Fatalf("attrs:\n got %v\nwant %v", attrs, want)
	}

	if spans[0]["name"] != "SELECT books" || spans[0]["status"] != "ok" || attrs["db.params_hash"] == nil {
		t.Fatalf("span: %v", spans[0])
	}
}

func TestAQueryPerRowIsAnNPlusOne(t *testing.T) {
	recorder := tracetest.Record(t)
	db := open(t, nil)

	trace := command(t, recorder, func(ctx context.Context) error {
		var books []Book
		if err := db.WithContext(ctx).Find(&books).Error; err != nil {
			return err
		}
		for _, book := range books {
			var author Author
			if err := db.WithContext(ctx).First(&author, book.AuthorID).Error; err != nil {
				return err
			}
		}
		return nil
	})

	spans := tracetest.Spans(trace, "sql")
	if len(spans) != 4 {
		t.Fatalf("sql spans: %v", spans)
	}
	fingerprints := map[any]int{}
	for _, span := range spans[1:] {
		fingerprints[tracetest.Attrs(span)["db.fingerprint"]]++
	}

	want := map[any]int{"SELECT * FROM `authors` WHERE `authors`.`id` = ? ORDER BY `authors`.`id` LIMIT 1": 3}
	if !tracetest.Equal(fingerprints, want) {
		t.Fatalf("fingerprints: %v", fingerprints)
	}

	params := []any{}
	for _, span := range spans[1:] {
		params = append(params, tracetest.Attrs(span)["db.params"])
	}
	if !tracetest.Equal(params, []any{[]any{1}, []any{1}, []any{2}}) {
		t.Fatalf("params: %v", params)
	}
}

func TestAPreloadIsAStatementAfterItsQuery(t *testing.T) {
	recorder := tracetest.Record(t)
	db := open(t, nil)

	trace := command(t, recorder, func(ctx context.Context) error {
		var books []Book
		return db.WithContext(ctx).Preload("Author").Find(&books).Error
	})

	spans := tracetest.Spans(trace, "sql")
	if len(spans) != 2 || spans[0]["name"] != "SELECT books" || spans[1]["name"] != "SELECT authors" {
		t.Fatalf("sql spans: %v", spans)
	}
	booksEnd := spans[0]["start"].(float64) + spans[0]["duration"].(float64)
	if booksEnd > spans[1]["start"].(float64) {
		t.Fatalf("the books' query ends after the preload starts: %v", spans)
	}
}

func TestAWriteSaysTheRowsItChangedInItsTransaction(t *testing.T) {
	recorder := tracetest.Record(t)
	db := open(t, nil)

	trace := command(t, recorder, func(ctx context.Context) error {
		return db.WithContext(ctx).Model(&Book{}).Where("author_id = ?", 1).Update("title", "Dune Messiah").Error
	})

	statements := tracetest.Spans(trace, "sql")
	transactions := tracetest.Spans(trace, "transaction")
	if len(statements) != 1 || len(transactions) != 1 {
		t.Fatalf("spans: %v", trace["spans"])
	}
	attrs := tracetest.Attrs(statements[0])

	if statements[0]["name"] != "UPDATE books" || attrs["db.response.affected_rows"] != float64(2) ||
		!tracetest.Equal(attrs["db.params"], []any{"Dune Messiah", 1}) {
		t.Fatalf("update: %v", statements[0])
	}

	if statements[0]["parent_id"] != transactions[0]["id"] ||
		tracetest.Attrs(transactions[0])["db.transaction.outcome"] != "commit" {
		t.Fatalf("GORM's own transaction: %v", transactions[0])
	}
}

func TestATransactionHoldsItsStatements(t *testing.T) {
	recorder := tracetest.Record(t)
	db := open(t, &gorm.Config{SkipDefaultTransaction: true}, tracepanel.DBAlias("orders"))

	trace := command(t, recorder, func(ctx context.Context) error {
		return db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
			if err := tx.Create(&Book{Title: "Tehanu", AuthorID: 2}).Error; err != nil {
				return err
			}
			_, end := tracepanel.Span(TxContext(ctx, tx), "reserve")
			end()
			return tx.Model(&Author{}).Where("id = ?", 2).Update("name", "Ursula K. Le Guin").Error
		})
	})

	transactions := tracetest.Spans(trace, "transaction")
	if len(transactions) != 1 {
		t.Fatalf("transactions: %v", trace["spans"])
	}
	transaction := transactions[0]
	attrs := tracetest.Attrs(transaction)
	origin, _ := attrs["code.origin"].(string)

	if transaction["name"] != "transaction orders" || attrs["db.transaction.outcome"] != "commit" ||
		attrs["db.alias"] != "orders" || attrs["db.client"] != "gorm" || !strings.HasPrefix(origin, "gormtrace_test.go:") {
		t.Fatalf("transaction: %v", transaction)
	}

	statements := tracetest.Spans(trace, "sql")
	if len(statements) != 2 || statements[0]["name"] != "INSERT books" || statements[1]["name"] != "UPDATE authors" {
		t.Fatalf("statements: %v", statements)
	}
	for _, statement := range statements {
		if statement["parent_id"] != transaction["id"] {
			t.Fatalf("a statement outside its transaction: %v", statement)
		}
	}

	work := tracetest.Spans(trace, "function")
	if len(work) != 1 || work[0]["parent_id"] != transaction["id"] {
		t.Fatalf("work in the transaction: %v", work)
	}
}

func TestARolledBackTransaction(t *testing.T) {
	recorder := tracetest.Record(t)
	db := open(t, &gorm.Config{SkipDefaultTransaction: true})
	failure := errors.New("out of stock")

	trace := command(t, recorder, func(ctx context.Context) error {
		err := db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
			if err := tx.Create(&Book{Title: "Tehanu", AuthorID: 2}).Error; err != nil {
				return err
			}
			return failure
		})
		if !errors.Is(err, failure) {
			return err
		}
		return nil
	})

	transaction := tracetest.Spans(trace, "transaction")[0]
	if tracetest.Attrs(transaction)["db.transaction.outcome"] != "rollback" || transaction["status"] != "ok" {
		t.Fatalf("transaction: %v", transaction)
	}
}

func TestATransactionUnderPreparedStatements(t *testing.T) {
	recorder := tracetest.Record(t)
	db := open(t, &gorm.Config{PrepareStmt: true})

	trace := command(t, recorder, func(ctx context.Context) error {
		tx := db.WithContext(ctx).Begin()
		if err := tx.Create(&Book{Title: "Tehanu", AuthorID: 2}).Error; err != nil {
			return err
		}
		return tx.Commit().Error
	})

	transactions := tracetest.Spans(trace, "transaction")
	statements := tracetest.Spans(trace, "sql")
	if len(transactions) != 1 || len(statements) != 1 || statements[0]["parent_id"] != transactions[0]["id"] {
		t.Fatalf("spans: %v", trace["spans"])
	}
	if sqlDB, err := db.DB(); err != nil || sqlDB == nil {
		t.Fatalf("db.DB(): %v", err)
	}
}

func TestAFailedStatementSaysWhy(t *testing.T) {
	recorder := tracetest.Record(t)
	db := open(t, nil)

	trace := command(t, recorder, func(ctx context.Context) error {
		var none Author
		_ = db.WithContext(ctx).First(&none, 99).Error
		_ = db.WithContext(ctx).Exec("DELETE FROM missing").Error
		return nil
	})

	spans := tracetest.Spans(trace, "sql")
	if len(spans) != 2 {
		t.Fatalf("sql spans: %v", spans)
	}

	if spans[0]["status"] != "ok" || tracetest.Attrs(spans[0])["db.response.returned_rows"] != float64(0) {
		t.Fatalf("a record not found isn't a failure: %v", spans[0])
	}

	if spans[1]["status"] != "error" || tracetest.Attrs(spans[1])["error.type"] == nil {
		t.Fatalf("failed: %v", spans[1])
	}
}

// traced is a GORM logger that keeps what GORM tells it: the app's, which the plugin's wraps.
type traced struct {
	logger.Interface
	mu   sync.Mutex
	rows []int64
}

func (l *traced) LogMode(logger.LogLevel) logger.Interface { return l }

func (l *traced) Trace(_ context.Context, _ time.Time, fc func() (string, int64), _ error) {
	_, rows := fc()
	l.mu.Lock()
	l.rows = append(l.rows, rows)
	l.mu.Unlock()
}

func TestRawSQLAndRows(t *testing.T) {
	recorder := tracetest.Record(t)
	db := open(t, nil)
	app := &traced{Interface: logger.Discard}
	db.Config.Logger.(*rowsLogger).Interface = app

	trace := command(t, recorder, func(ctx context.Context) error {
		var titles []string
		if err := db.WithContext(ctx).Raw("SELECT title FROM books WHERE id > ?", 1).Scan(&titles).Error; err != nil {
			return err
		}
		var none []Book
		if err := db.WithContext(ctx).Raw("SELECT * FROM books WHERE id > ?", 100).Scan(&none).Error; err != nil {
			return err
		}
		// Debug is a Session with the logger at another level: still the plugin's.
		var books []struct{ Title string }
		if err := db.Debug().WithContext(ctx).Model(&Book{}).Select("title").Where("author_id = ?", 1).Scan(&books).Error; err != nil {
			return err
		}
		rows, err := db.WithContext(ctx).Model(&Book{}).Select("title").Rows()
		if err != nil {
			return err
		}
		for rows.Next() {
		}
		return rows.Close()
	})

	got := []any{}
	for _, span := range tracetest.Spans(trace, "sql") {
		attrs := tracetest.Attrs(span)
		returned, counted := attrs["db.response.returned_rows"]
		got = append(got, []any{attrs["db.statement"], returned, counted})
	}
	want := []any{
		[]any{"SELECT title FROM books WHERE id > ?", 2, true},
		[]any{"SELECT * FROM books WHERE id > ?", 0, true},
		[]any{"SELECT `title` FROM `books` WHERE author_id = ?", 2, true},
		// Rows() handed to the app: what it reads of them can't be seen.
		[]any{"SELECT `title` FROM `books`", nil, false},
	}
	if !tracetest.Equal(got, want) {
		t.Fatalf("sql spans: %v", got)
	}
	raw := tracetest.Attrs(tracetest.Spans(trace, "sql")[0])
	if !tracetest.Equal(raw["db.params"], []any{1}) {
		t.Fatalf("raw: %v", raw)
	}
	// The app's logger still gets what GORM tells it: Scan's rows, and -1 of the Rows() it ran.
	app.mu.Lock()
	defer app.mu.Unlock()
	if !tracetest.Equal(app.rows, []int64{2, 0, 2, -1}) {
		t.Fatalf("the app's logger: %v", app.rows)
	}
}

// Scan's rows, into whatever it scans to, with the app's logger logger.Discard or none at all.
func TestScanCountsItsRowsWithAnyLogger(t *testing.T) {
	loggers := map[string]logger.Interface{"discard": logger.Discard, "none": nil}
	for name, appLogger := range loggers {
		t.Run(name, func(t *testing.T) {
			recorder := tracetest.Record(t)
			db := openPlain(t, sqlite.Open(filepath.Join(t.TempDir(), "shop.db")), nil)
			db.Config.Logger = appLogger
			if err := db.Use(Plugin()); err != nil {
				t.Fatal(err)
			}

			trace := command(t, recorder, func(ctx context.Context) error {
				db := db.WithContext(ctx)
				var average *float64
				var count float64
				var book struct{ Title string }
				var titles []string
				var missing *string
				var none string
				type title string
				var named title
				for _, scan := range []*gorm.DB{
					db.Model(&Book{}).Select("AVG(id)").Where("author_id = ?", 1).Scan(&average),
					db.Model(&Book{}).Select("COUNT(*)").Scan(&count),
					db.Model(&Book{}).Select("title").Where("id = ?", 1).Scan(&book),
					db.Model(&Book{}).Select("title").Scan(&titles),
					db.Model(&Book{}).Select("title").Where("id > ?", 100).Scan(&missing),
					db.Model(&Book{}).Select("title").Where("id > ?", 100).Scan(&none),
					db.Model(&Book{}).Select("title").Where("id = ?", 2).Scan(&named),
				} {
					if scan.Error != nil {
						return scan.Error
					}
				}
				if average == nil || *average != 1.5 || count != 3 || book.Title != "Dune" || len(titles) != 3 ||
					missing != nil || named != "Children of Dune" {
					t.Errorf("scanned: %v %v %v %v %v %v", average, count, book, titles, missing, named)
				}
				return nil
			})

			got := []any{}
			for _, span := range tracetest.Spans(trace, "sql") {
				attrs := tracetest.Attrs(span)
				got = append(got, []any{attrs["db.statement"], attrs["db.response.returned_rows"]})
			}
			want := []any{
				[]any{"SELECT AVG(id) FROM `books` WHERE author_id = ?", 1},
				[]any{"SELECT COUNT(*) FROM `books`", 1},
				[]any{"SELECT `title` FROM `books` WHERE id = ?", 1},
				[]any{"SELECT `title` FROM `books`", 3},
				[]any{"SELECT `title` FROM `books` WHERE id > ?", 0},
				[]any{"SELECT `title` FROM `books` WHERE id > ?", 0},
				[]any{"SELECT `title` FROM `books` WHERE id = ?", 1},
			}
			if !tracetest.Equal(got, want) {
				t.Fatalf("sql spans:\n got %v\nwant %v", got, want)
			}
		})
	}
}

func TestOutsideATraceNothingIsRecorded(t *testing.T) {
	recorder := tracetest.Record(t)
	db := open(t, nil)

	if err := db.Create(&Book{Title: "Tehanu", AuthorID: 2}).Error; err != nil {
		t.Fatal(err)
	}
	trace := command(t, recorder, func(context.Context) error { return nil })

	if spans := tracetest.List(trace, "spans"); len(spans) != 1 {
		t.Fatalf("recorded: %v", spans)
	}
}

func TestADatabaseOpenedThroughDriverIsLeftToIt(t *testing.T) {
	recorder := tracetest.Record(t)
	dialector := &sqlite.Dialector{DriverName: tracepanel.Driver("sqlite"), DSN: filepath.Join(t.TempDir(), "shop.db")}
	db := openPlain(t, dialector, nil)
	if err := db.Use(Plugin()); err != nil {
		t.Fatal(err)
	}

	trace := command(t, recorder, func(ctx context.Context) error {
		var books []Book
		return db.WithContext(ctx).Find(&books).Error
	})

	spans := tracetest.Spans(trace, "sql")
	if len(spans) != 1 || tracetest.Attrs(spans[0])["db.client"] != "database/sql" {
		t.Fatalf("sql spans: %v", spans)
	}
}

func TestOffThePluginRegistersNothing(t *testing.T) {
	t.Setenv("TRACEPANEL_ENABLED", "0")
	db := openPlain(t, sqlite.Open(filepath.Join(t.TempDir(), "shop.db")), nil)
	pool := db.ConnPool

	if err := db.Use(Plugin()); err != nil {
		t.Fatal(err)
	}

	if db.ConnPool != pool || db.Callback().Query().Get("tracepanel:after_query") != nil {
		t.Fatal("the plugin registered with Tracepanel off")
	}
}
