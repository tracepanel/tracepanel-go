package tracepanel

import (
	"regexp"
	"strings"
	"testing"
	"time"
)

func TestULIDTimeAndFormat(t *testing.T) {
	before := time.Now().UnixMilli()
	id := newID()
	after := time.Now().UnixMilli()
	if !regexp.MustCompile(`^[0-9A-HJKMNP-TV-Z]{26}$`).MatchString(id) {
		t.Fatalf("not a ULID: %s", id)
	}
	var ms int64
	for _, char := range id[:10] {
		ms = ms*32 + int64(strings.IndexRune(crockford, char))
	}
	if ms < before || ms > after {
		t.Fatalf("time %d not in [%d, %d]", ms, before, after)
	}
	if newID() == newID() {
		t.Fatal("ids repeat")
	}
}

func TestSQLRules(t *testing.T) {
	cases := []struct{ sql, name, fingerprint string }{
		{"SELECT name FROM authors WHERE id = ?", "SELECT authors", "SELECT name FROM authors WHERE id = ?"},
		{"select  count(*)\n FROM books WHERE author_id = ?", "SELECT COUNT books", "select count(*) FROM books WHERE author_id = ?"},
		{"SELECT * FROM t WHERE id IN (?, ?, ?)", "SELECT t", "SELECT * FROM t WHERE id IN (...)"},
		{"INSERT INTO books (title) VALUES (?)", "INSERT books", "INSERT INTO books (title) VALUES (?)"},
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

func TestInjectFirstInHead(t *testing.T) {
	got := string(inject([]byte("<html><head><title>x</title></head></html>"), "<script></script>"))
	if got != "<html><head><script></script><title>x</title></head></html>" {
		t.Fatal(got)
	}
}
