package tracepanel

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"testing"
)

func TestADSNNamesItsDatabase(t *testing.T) {
	t.Setenv("PGDATABASE", "")
	t.Setenv("PGUSER", "")
	got := map[string]string{}
	for _, dsn := range [][2]string{
		{"postgresql", "postgres://app:secret@localhost:5432/shop?sslmode=disable"},
		{"postgresql", "postgres://app@localhost"},
		{"postgresql", "host=localhost user=app dbname='shop' sslmode=disable"},
		{"postgresql", "host=localhost user=app"},
		{"mysql", "app:secret@tcp(localhost:3306)/shop?parseTime=true"},
		{"mysql", "app:secret@tcp(localhost:3306)/"},
		{"mssql", "sqlserver://app:secret@localhost:1433/SQLEXPRESS?database=shop"},
		{"mssql", "sqlserver://app:secret@localhost:1433/SQLEXPRESS"},
		{"mssql", "server=localhost;user id=app;Database=shop"},
		{"sqlite", "/srv/app/shop.db"},
		{"sqlite", "file:shop.db?cache=shared"},
		{"sqlite", ":memory:"},
		{"sqlite", "file:memdb1?mode=memory&cache=shared"},
		{"duckdb", "warehouse.duckdb"},
		{"clickhouse", "clickhouse://localhost:9000/events"},
		{"clickhouse", "tcp://localhost:9000?database=events"},
		{"snowflake", "app:secret@account/ANALYTICS/PUBLIC?warehouse=wh"},
		{"other_sql", "shop"},
	} {
		got[dsn[0]+" "+dsn[1]] = dsnName(dsn[0], dsn[1])
	}

	want := map[string]string{
		"postgresql postgres://app:secret@localhost:5432/shop?sslmode=disable": "shop",
		"postgresql postgres://app@localhost":                                  "app",
		"postgresql host=localhost user=app dbname='shop' sslmode=disable":     "shop",
		"postgresql host=localhost user=app":                                   "app",
		"mysql app:secret@tcp(localhost:3306)/shop?parseTime=true":             "shop",
		"mysql app:secret@tcp(localhost:3306)/":                                "",
		"mssql sqlserver://app:secret@localhost:1433/SQLEXPRESS?database=shop": "shop",
		"mssql sqlserver://app:secret@localhost:1433/SQLEXPRESS":               "",
		"mssql server=localhost;user id=app;Database=shop":                     "shop",
		"sqlite /srv/app/shop.db":                                              "shop.db",
		"sqlite file:shop.db?cache=shared":                                     "shop.db",
		"sqlite :memory:":                                                      "",
		"sqlite file:memdb1?mode=memory&cache=shared":                          "",
		"duckdb warehouse.duckdb":                                              "warehouse.duckdb",
		"clickhouse clickhouse://localhost:9000/events":                        "events",
		"clickhouse tcp://localhost:9000?database=events":                      "events",
		"snowflake app:secret@account/ANALYTICS/PUBLIC?warehouse=wh":           "ANALYTICS",
		"other_sql shop": "",
	}
	if !equalJSON(got, want) {
		t.Fatalf("names:\n got %v\nwant %v", got, want)
	}
}

// Connectors and dialectors shaped as the drivers' and ORMs' are: pgx's stdlib connector (its ConnConfig embedding
// pgconn's Config), go-sql-driver/mysql's (an unexported *Config), lib/pq's (its options by keyword), GORM's
// dialectors (a DSN).
type (
	pgconnConfig  struct{ Host, Database string }
	pgxConnConfig struct{ pgconnConfig }
	pgxConnector  struct {
		pgxConnConfig
		BeforeConnect func()
	}
	mysqlConfig    struct{ User, DBName string }
	mysqlConnector struct{ cfg *mysqlConfig }
	pqValues       map[string]string
	pqConnector    struct{ opts pqValues }
	gormConfig     struct{ DSN string }
	gormPostgres   struct{ *gormConfig }
	gormSQLite     struct{ DriverName, DSN string }
)

func TestADriversConfigNamesItsDatabase(t *testing.T) {
	got := map[string]string{
		"pgx":      configName("postgresql", pgxConnector{pgxConnConfig: pgxConnConfig{pgconnConfig{"localhost", "shop"}}}),
		"mysql":    configName("mysql", &mysqlConnector{cfg: &mysqlConfig{User: "app", DBName: "shop"}}),
		"pq":       configName("postgresql", &pqConnector{opts: pqValues{"host": "localhost", "dbname": "shop"}}),
		"gorm":     configName("postgresql", gormPostgres{&gormConfig{DSN: "postgres://app@localhost/shop"}}),
		"sqlite":   configName("sqlite", &gormSQLite{DSN: "/srv/app/shop.db"}),
		"none":     configName("mysql", &mysqlConnector{}),
		"nil":      configName("mysql", nil),
		"no field": configName("mysql", struct{ Host string }{"localhost"}),
	}

	want := map[string]string{
		"pgx": "shop", "mysql": "shop", "pq": "shop", "gorm": "shop", "sqlite": "shop.db", "none": "", "nil": "",
		"no field": "",
	}
	if !equalJSON(got, want) {
		t.Fatalf("names:\n got %v\nwant %v", got, want)
	}
}

// namedConnector is a driver's connector with its database in its config, as pgx's stdlib one has it.
type namedConnector struct {
	pgxConnConfig
}

func (namedConnector) Connect(context.Context) (driver.Conn, error) { return fake.Open("") }
func (namedConnector) Driver() driver.Driver                        { return fake }

// fakepg is fakedb under a name of its own: the other tests' Driver("fakedb") stays the first one registered.
func init() { sql.Register("fakepg", fake) }

func TestAStatementNamesTheDatabaseItsDSNOrConnectorIsFor(t *testing.T) {
	recorded := record(t)
	byDSN, err := sql.Open(Driver("fakepg", DBSystem("postgresql")), "postgres://app@localhost/shop")
	if err != nil {
		t.Fatal(err)
	}
	byConnector := sql.OpenDB(WrapConnector(namedConnector{pgxConnConfig{pgconnConfig{"localhost", "orders"}}},
		DBAlias("orders"), DBSystem("postgresql")))
	named, err := sql.Open(Driver("fakepg", DBAlias("reports"), DBName("warehouse")), "shop")
	if err != nil {
		t.Fatal(err)
	}

	err = Command(context.Background(), "report", func(ctx context.Context) error {
		for _, db := range []*sql.DB{byDSN, byConnector, named} {
			tx, err := db.BeginTx(ctx, nil)
			if err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, "UPDATE authors SET seen = now() WHERE id = $1", 7); err != nil {
				return err
			}
			if err := tx.Commit(); err != nil {
				return err
			}
		}
		return nil
	})
	doc := recorded.wait(t, 1)[0]

	if err != nil {
		t.Fatal(err)
	}
	type where struct{ Name, Alias, Namespace any }
	var got []where
	for _, span := range doc.Spans {
		if span.Kind == "sql" || span.Kind == "transaction" {
			got = append(got, where{span.Name, span.Attrs["db.alias"], span.Attrs["db.namespace"]})
		}
	}
	want := []where{
		{"transaction default", "default", "shop"},
		{"UPDATE authors", "default", "shop"},
		{"transaction orders", "orders", "orders"},
		{"UPDATE authors", "orders", "orders"},
		{"transaction reports", "reports", "warehouse"},
		{"UPDATE authors", "reports", "warehouse"},
	}
	if !equalJSON(got, want) {
		t.Fatalf("spans:\n got %v\nwant %v", got, want)
	}
}
