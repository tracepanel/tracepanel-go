package tracepanel

import (
	"regexp"
	"strings"
)

// A statement's span name, its one-line form, and its fingerprint, which the agent groups queries (and finds N+1
// queries) by: the same rules in every tracepanel SDK.

var (
	whitespace = regexp.MustCompile(`\s+`)
	inList     = regexp.MustCompile(`(?i)\bIN \((?:%s|\?|\$\d+)(?:, ?(?:%s|\?|\$\d+))*\)`)
	table      = regexp.MustCompile("(?i)\\b(?:FROM|INTO|UPDATE|JOIN)\\s+[`\"\\[]?([\\w.]+)")
	count      = regexp.MustCompile(`(?i)^\s*SELECT\s+COUNT\(`)
)

func statement(sql string) string { return strings.TrimSpace(whitespace.ReplaceAllString(sql, " ")) }

func fingerprint(sql string) string { return inList.ReplaceAllString(statement(sql), "IN (...)") }

func spanName(sql string) string {
	fields := strings.Fields(sql)
	verb := "SQL"
	if len(fields) > 0 {
		verb = strings.ToUpper(fields[0])
	}
	if count.MatchString(sql) {
		verb = "SELECT COUNT"
	}
	if match := table.FindStringSubmatch(sql); match != nil {
		return verb + " " + match[1]
	}
	return verb
}
