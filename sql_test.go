package tracepanel

import (
	"reflect"
	"testing"
)

// The same cases as the other SDKs' and the agent's own tests: one rule everywhere.

func TestTheFingerprintFoldsWhitespaceAndINLists(t *testing.T) {
	cases := map[string]string{
		"SELECT *\n  FROM \"t\"  WHERE id IN (%s, %s, %s)":  `SELECT * FROM "t" WHERE id IN (...)`,
		"SELECT 1 WHERE id IN (%s,%s)":                      "SELECT 1 WHERE id IN (...)",
		"SELECT 1 WHERE id IN (?, ?)":                       "SELECT 1 WHERE id IN (...)",
		"SELECT 1 WHERE id IN (%(id_1_1)s, %(id_1_2)s)":     "SELECT 1 WHERE id IN (...)",
		"SELECT * FROM books WHERE id IN ($1, $2, $3)":      "SELECT * FROM books WHERE id IN (...)",
		"select  count(*)\n FROM books WHERE author_id = ?": "select count(*) FROM books WHERE author_id = ?",
	}
	for sql, want := range cases {
		if got := fingerprint(sql); got != want {
			t.Errorf("fingerprint(%q) = %q, want %q", sql, got, want)
		}
	}
	if got := statementText("SELECT *\n  FROM books\n WHERE id = $1"); got != "SELECT * FROM books WHERE id = $1" {
		t.Errorf("one line: %q", got)
	}
}

func TestSpanNamesTellCountsFromReads(t *testing.T) {
	cases := map[string]string{
		`SELECT "t"."id" FROM "t" WHERE "t"."id" = %s`: "SELECT t",
		`SELECT COUNT(*) AS "__count" FROM "t"`:        "SELECT COUNT t",
		`UPDATE "t" SET "likes" = ("t"."likes" + %s)`:  "UPDATE t",
		"SELECT name FROM authors WHERE id = $1":       "SELECT authors",
		"select count(*) from books":                   "SELECT COUNT books",
		`INSERT INTO "books" (title) VALUES ($1)`:      "INSERT books",
		"BEGIN": "BEGIN",
		"":      "SQL",
	}
	for sql, want := range cases {
		if got := spanName(sql); got != want {
			t.Errorf("spanName(%q) = %q, want %q", sql, got, want)
		}
	}
}

func TestASchemaQualifiedTableIsNamedByTheTable(t *testing.T) {
	cases := []struct{ sql, name string }{
		{`SELECT "public"."category"."id", "public"."category"."name" FROM "public"."category" WHERE 1=1`, "SELECT category"},
		{`SELECT COUNT(*) AS "_count$_all" FROM (SELECT "public"."order"."id" FROM "public"."order" OFFSET $1) AS "sub"`, "SELECT COUNT order"},
		{`SELECT "t1"."id" FROM "public"."order_item" AS "t1" LEFT JOIN "public"."product" AS "t2" ON "t2"."id" = 1`, "SELECT order_item"},
		{`INSERT INTO "public"."order" ("number") VALUES ($1) RETURNING "public"."order"."id"`, "INSERT order"},
		{`UPDATE "public"."product" SET "stock" = $1 WHERE "public"."product"."id" = $2`, "UPDATE product"},
		{`DELETE FROM "public"."cart_item" WHERE "public"."cart_item"."customer_id" = $1`, "DELETE cart_item"},
		{"SELECT `book`.`id` FROM `shop`.`book` JOIN `shop`.`author` ON `author`.`id` = `book`.`author_id`", "SELECT book"},
		{"UPDATE `shop` . `book` SET `price` = ?", "UPDATE book"},
		{"SELECT [id] FROM [dbo].[orders]", "SELECT orders"},
		{"SELECT * FROM shop.books", "SELECT books"},
		{`SELECT id FROM "Order Items"`, "SELECT Order Items"},
	}
	for _, c := range cases {
		if got := spanName(c.sql); got != c.name {
			t.Errorf("spanName(%q) = %q, want %q", c.sql, got, c.name)
		}
	}
}

const users = "[auth_user].[id] AS [id], [auth_user].[username] AS [username] FROM [auth_user]"

func TestNamesAndFingerprintsInEachDialect(t *testing.T) {
	cases := []struct{ sql, name, fingerprint string }{
		// SQL Server: [names], TOP, OFFSET ... FETCH, @P1 and @name placeholders.
		{"SELECT TOP 21 " + users + " WHERE [auth_user].[id] = %s", "SELECT auth_user", "SELECT TOP 21 " + users + " WHERE [auth_user].[id] = %s"},
		{"SELECT " + users + " ORDER BY 1 ASC  OFFSET 20 ROWS FETCH FIRST 10 ROWS ONLY", "SELECT auth_user", "SELECT " + users + " ORDER BY 1 ASC OFFSET 20 ROWS FETCH FIRST 10 ROWS ONLY"},
		{"SELECT " + users + " WHERE [auth_user].[id] IN (%s, %s, %s)", "SELECT auth_user", "SELECT " + users + " WHERE [auth_user].[id] IN (...)"},
		{"UPDATE [auth_user] SET [username] = %s WHERE [auth_user].[id] = %s", "UPDATE auth_user", "UPDATE [auth_user] SET [username] = %s WHERE [auth_user].[id] = %s"},
		{"SELECT TOP 10 [o].[id] FROM [dbo].[orders] AS [o] WHERE [o].[customer_id] IN (@P1, @P2, @P3)", "SELECT orders", "SELECT TOP 10 [o].[id] FROM [dbo].[orders] AS [o] WHERE [o].[customer_id] IN (...)"},
		{"SELECT [id] FROM [dbo].[orders] WHERE [customer_id] IN (@id0, @id1)", "SELECT orders", "SELECT [id] FROM [dbo].[orders] WHERE [customer_id] IN (...)"},
		{"SELECT id FROM dbo.orders WHERE customer_id IN (?, ?)", "SELECT orders", "SELECT id FROM dbo.orders WHERE customer_id IN (...)"},
		// Oracle, PostgreSQL, MySQL and SQLite quoting and placeholders.
		{`SELECT "ID" FROM "SHOP"."ORDERS" WHERE "ID" IN (:1, :2)`, "SELECT ORDERS", `SELECT "ID" FROM "SHOP"."ORDERS" WHERE "ID" IN (...)`},
		{"SELECT id FROM orders WHERE id IN (:id_1, :id_2)", "SELECT orders", "SELECT id FROM orders WHERE id IN (...)"},
		{`SELECT "shop_order"."id" FROM "shop_order" WHERE "shop_order"."id" IN ($1, $2)`, "SELECT shop_order", `SELECT "shop_order"."id" FROM "shop_order" WHERE "shop_order"."id" IN (...)`},
		{`SELECT "t"."id" FROM "public"."t" WHERE "t"."id" IN (%s, %s)`, "SELECT t", `SELECT "t"."id" FROM "public"."t" WHERE "t"."id" IN (...)`},
		{"SELECT `shop_order`.`id` FROM `shop_order` WHERE `shop_order`.`id` IN (%s, %s) LIMIT 21", "SELECT shop_order", "SELECT `shop_order`.`id` FROM `shop_order` WHERE `shop_order`.`id` IN (...) LIMIT 21"},
		{"SELECT id FROM t WHERE id IN (?1, ?2)", "SELECT t", "SELECT id FROM t WHERE id IN (...)"},
	}
	for _, c := range cases {
		if got := spanName(c.sql); got != c.name {
			t.Errorf("spanName(%q) = %q, want %q", c.sql, got, c.name)
		}
		if got := fingerprint(c.sql); got != c.fingerprint {
			t.Errorf("fingerprint(%q) = %q, want %q", c.sql, got, c.fingerprint)
		}
	}
}

func TestCommentsNameAndGroupNothing(t *testing.T) {
	cases := []struct{ sql, text, name, fingerprint string }{
		// The agent's case: a sqlcommenter block with the traceparent, a line comment, a string that looks like both.
		{
			"/*controller='orders',traceparent='00-4bf9-01'*/ SELECT id\n-- the page's rows\nFROM orders " +
				"WHERE note = '-- not /* a */ comment' AND id IN ($1, $2)",
			"/*controller='orders',traceparent='00-4bf9-01'*/ SELECT id /* the page's rows */ FROM orders " +
				"WHERE note = '-- not /* a */ comment' AND id IN ($1, $2)",
			"SELECT orders",
			"SELECT id FROM orders WHERE note = '-- not /* a */ comment' AND id IN (...)",
		},
		{
			"/*traceparent='00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01'*/ SELECT * FROM orders",
			"/*traceparent='00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01'*/ SELECT * FROM orders",
			"SELECT orders",
			"SELECT * FROM orders",
		},
		// A line comment mid-statement: a */ in it doesn't end the closed one, an empty one is closed too.
		{
			"SELECT count(*)\n  -- the */ count\n  FROM books --\nWHERE id = %s",
			"SELECT count(*) /* the * / count */ FROM books /* */ WHERE id = %s",
			"SELECT COUNT books",
			"SELECT count(*) FROM books WHERE id = %s",
		},
		// -- and /* in a string or a quoted name are its text.
		{
			"SELECT \"a--b\", `c/*d` FROM notes WHERE body = 'it''s -- not /* a comment' AND x = \"--\"",
			"SELECT \"a--b\", `c/*d` FROM notes WHERE body = 'it''s -- not /* a comment' AND x = \"--\"",
			"SELECT notes",
			"SELECT \"a--b\", `c/*d` FROM notes WHERE body = 'it''s -- not /* a comment' AND x = \"--\"",
		},
	}
	for _, c := range cases {
		got := parse(c.sql)
		if got != (parsedSQL{name: c.name, text: c.text, fingerprint: c.fingerprint}) {
			t.Errorf("%q:\n got %+v\nwant %+v", c.sql, got, parsedSQL{name: c.name, text: c.text, fingerprint: c.fingerprint})
		}
	}
}

func TestADatabaseKindIsKnownByItsDriver(t *testing.T) {
	got := map[string]string{}
	for _, name := range []string{
		"pgx", "postgres", "github.com/lib/pq", "github.com/jackc/pgx/v5/stdlib", "mysql", "sqlite", "sqlite3",
		"modernc.org/sqlite", "sqlserver", "mssql", "godror", "oracle", "clickhouse", "duckdb", "snowflake",
		"github.com/snowflakedb/gosnowflake", "bigquery", "databricks", "github.com/databricks/databricks-sql-go",
		"redshift", "trino", "acme",
	} {
		got[name] = systemOf(name)
	}

	want := map[string]string{
		"pgx": "postgresql", "postgres": "postgresql", "github.com/lib/pq": "postgresql",
		"github.com/jackc/pgx/v5/stdlib": "postgresql", "mysql": "mysql", "sqlite": "sqlite", "sqlite3": "sqlite",
		"modernc.org/sqlite": "sqlite", "sqlserver": "mssql", "mssql": "mssql", "godror": "oracle", "oracle": "oracle",
		"clickhouse": "clickhouse", "duckdb": "duckdb", "snowflake": "snowflake",
		"github.com/snowflakedb/gosnowflake": "snowflake", "bigquery": "bigquery", "databricks": "databricks",
		"github.com/databricks/databricks-sql-go": "databricks", "redshift": "redshift", "trino": "trino", "acme": "",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("systems: %v", got)
	}
}
