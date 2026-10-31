package tracepanel

import (
	"regexp"
	"strings"
	"sync"
)

// A statement's span name, its one-line form, and its fingerprint, which the agent groups queries (and finds N+1
// queries) by: the same rules in every tracepanel SDK, and in the agent for a statement it maps from OpenTelemetry.

const placeholder = `(?:%s|%\([A-Za-z0-9_]+\)s|\?[0-9]*|\$[0-9]+|@[A-Za-z0-9_]+|:[A-Za-z0-9_]+)`

// One part of a table's name, bare or quoted: "standard SQL", `MySQL`, [SQL Server].
const part = "(?:\"[^\"]+\"|`[^`]+`|\\[[^\\]]+\\]|[\\w$]+)"

var (
	// A comment (`-- a note` to the line's end, `/* sqlcommenter's tags */`), or a string or quoted name, whose `--`
	// and `/*` are its text: matched so a comment is told from a string that looks like one.
	comment = regexp.MustCompile(`(--[^\n]*)|(/\*(?s:.*?)\*/)|'[^']*(?:''[^']*)*'|"[^"]*(?:""[^"]*)*"|` + "`[^`]*`")
	// An IN list of placeholders, as each driver writes them: %s and %(name)s, ? and ?1, $1, @p1 and @name, :1 and
	// :name.
	inList = regexp.MustCompile(`(?i)\bIN \(` + placeholder + `(?:, ?` + placeholder + `)*\)`)
	// The table a statement reads or writes, with its schema (Prisma's "public"."category", MySQL's `shop`.`book`):
	// the first part captured inside its quotes, the rest apart.
	table = regexp.MustCompile("(?i)\\b(?:FROM|INTO|UPDATE|JOIN)\\s+" +
		"(?:\"([^\"]+)\"|`([^`]+)`|\\[([^\\]]+)\\]|([\\w$]+))((?:\\s*\\.\\s*" + part + ")*)")
	parts = regexp.MustCompile(part)
	count = regexp.MustCompile(`(?i)^\s*SELECT\s+COUNT\(`)
)

// hasComment: the statement may have a comment, which takes the slower parse.
func hasComment(sql string) bool {
	return strings.Contains(sql, "--") || strings.Contains(sql, "/*")
}

// oneLine is the text on one line, its runs of whitespace one space each.
func oneLine(sql string) string { return strings.Join(strings.Fields(sql), " ") }

// replaceComments rewrites each comment of sql (line: a `--` one) with `with`; strings and quoted names stay.
func replaceComments(sql string, with func(text string, line bool) string) string {
	var out strings.Builder
	last := 0
	for _, match := range comment.FindAllStringSubmatchIndex(sql, -1) {
		line, block := match[2] >= 0, match[4] >= 0
		if !line && !block {
			continue
		}
		out.WriteString(sql[last:match[0]])
		out.WriteString(with(sql[match[0]:match[1]], line))
		last = match[1]
	}
	out.WriteString(sql[last:])
	return out.String()
}

// statementText is the statement on one line, a line comment (`-- a note`) closed as `/* a note */` so the line it
// ended still reads as SQL.
func statementText(sql string) string {
	if strings.Contains(sql, "--") {
		sql = replaceComments(sql, func(text string, line bool) string {
			if !line {
				return text
			}
			return "/* " + strings.ReplaceAll(strings.TrimSpace(text[2:]), "*/", "* /") + " */"
		})
	}
	return oneLine(sql)
}

// uncommented is the statement on one line without its comments (an ORM's or sqlcommenter's tags, a note, the
// traceparent): what names and groups it.
func uncommented(sql string) string {
	if !hasComment(sql) {
		return oneLine(sql)
	}
	return oneLine(replaceComments(sql, func(string, bool) string { return " " }))
}

// fingerprint is what the agent groups queries by, and finds N+1 queries and duplicates by: the statement without
// its comments (one that carries the request's traceparent differs in every query), its IN lists of placeholders
// one `IN (...)`.
func fingerprint(sql string) string { return inList.ReplaceAllString(uncommented(sql), "IN (...)") }

// spanName is the statement's verb and its table, its schema, quotes and comments left out: `SELECT orders`, and
// `SELECT COUNT orders` for a count.
func spanName(sql string) string {
	sql = uncommented(sql)
	verb := "SQL"
	if word, _, _ := strings.Cut(sql, " "); word != "" {
		verb = strings.ToUpper(word)
	}
	if verb == "SELECT" && count.MatchString(sql) {
		verb = "SELECT COUNT"
	}
	match := table.FindStringSubmatch(sql)
	if match == nil {
		return verb
	}
	if match[5] == "" {
		return verb + " " + match[1] + match[2] + match[3] + match[4]
	}
	found := parts.FindAllString(match[5], -1)
	name := found[len(found)-1]
	name = strings.TrimLeft(name[:1], "\"`[") + name[1:]
	name = name[:len(name)-1] + strings.TrimRight(name[len(name)-1:], "\"`]")
	return verb + " " + name
}

// parsedSQL is a statement's span name, its one-line form and its fingerprint.
type parsedSQL struct{ name, text, fingerprint string }

const parsedCap = 2048

var (
	parsedMu sync.Mutex
	parsed   = map[string]parsedSQL{}
)

// parse is the statement's span name, one-line form and fingerprint: an app runs the same statements over and over,
// so each is parsed once (until parsedCap of them were, then afresh).
func parse(sql string) parsedSQL {
	parsedMu.Lock()
	known, ok := parsed[sql]
	parsedMu.Unlock()
	if ok {
		return known
	}
	known = parsedSQL{name: spanName(sql), text: statementText(sql), fingerprint: fingerprint(sql)}
	parsedMu.Lock()
	if len(parsed) >= parsedCap {
		clear(parsed)
	}
	parsed[sql] = known
	parsedMu.Unlock()
	return known
}
