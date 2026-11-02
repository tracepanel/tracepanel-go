package tracepanel

import (
	"database/sql"
	"encoding/json"
	"os"
	"slices"
	"strings"
	"testing"
)

// The cases of the rule, as testdata/explainable.json has them (it mirrors the agent's schema/explainable.json).
func TestWhatEXPLAINRunsIsTheSharedRule(t *testing.T) {
	data, err := os.ReadFile("testdata/explainable.json")
	if err != nil {
		t.Fatal(err)
	}
	var cases []struct {
		SQL     string  `json:"sql"`
		Explain *string `json:"explain"`
		Analyze *string `json:"analyze"`
	}
	if err := json.Unmarshal(data, &cases); err != nil {
		t.Fatal(err)
	}
	text := func(message *string) string {
		if message == nil {
			return ""
		}
		return *message
	}

	for _, c := range cases {
		got := [2]string{explainRefusal(c.SQL, false), explainRefusal(c.SQL, true)}

		if want := [2]string{text(c.Explain), text(c.Analyze)}; got != want {
			t.Errorf("%q: got %q, want %q", c.SQL, got, want)
		}
	}
}

// A refused statement never reaches the database; one that reads runs, in a transaction rolled back.
func TestARefusedEXPLAINNeverReachesTheDatabase(t *testing.T) {
	fake.reset()
	db, err := sql.Open("fakedb", "shop")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	remember(&database{alias: "refused", system: "postgresql", sql: db})
	t.Cleanup(func() {
		databasesMu.Lock()
		defer databasesMu.Unlock()
		delete(databases, "refused")
	})

	for _, statement := range []string{
		"EXPLAIN WITH gone AS (DELETE FROM books RETURNING id) SELECT * FROM gone",
		"EXPLAIN (ANALYZE, BUFFERS) SELECT * FROM books FOR UPDATE",
		"EXPLAIN SELECT 1; DROP TABLE books",
		"DELETE FROM books",
	} {
		_, err := explain(explainRequest{SQL: statement, Alias: "refused"})

		if err == nil || !strings.Contains(err.Error(), "explained") {
			t.Errorf("%q: %v", statement, err)
		}
	}
	_, err = explain(explainRequest{
		SQL: "EXPLAIN (ANALYZE, BUFFERS) WITH recent AS (SELECT * FROM books) SELECT * FROM recent", Analyze: true,
		Alias: "refused",
	})

	fake.mu.Lock()
	defer fake.mu.Unlock()
	if err != nil || !slices.Equal(fake.ran, []string{
		"EXPLAIN (ANALYZE, BUFFERS) WITH recent AS (SELECT * FROM books) SELECT * FROM recent",
	}) || fake.rolledBack != 1 || fake.committed != 0 {
		t.Fatalf("err %v, ran %q, rolled back %d, committed %d", err, fake.ran, fake.rolledBack, fake.committed)
	}
}
