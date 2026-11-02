package tracepanel

import (
	"fmt"
	"slices"
	"strings"
	"unicode"
)

// Which statements EXPLAIN may run, the rule the agent, the console and every SDK share: a SELECT, or a WITH whose
// CTEs and final statement only read. Its words are read outside quoted strings and names and comments, so a keyword
// in a string changes nothing.
//
//   - It starts (after its comments) with SELECT, or with WITH and ends in a SELECT.
//   - It is one statement: nothing follows a `;` but comments.
//   - No INSERT, UPDATE, DELETE or MERGE anywhere (a data-modifying CTE) and no INTO (`SELECT … INTO` makes a table).
//   - ANALYZE runs it, so a SELECT that locks rows (`FOR UPDATE`, `FOR SHARE`, `LOCK IN SHARE MODE`) is explained
//     but not analyzed. ANALYZE runs in a transaction that is rolled back.

const (
	notSelect    = "Only SELECT statements can be explained."
	oneStatement = "Only one statement can be explained at a time."
)

var (
	changing = []string{"INSERT", "UPDATE", "DELETE", "MERGE"}
	// What can follow a WITH's CTEs: the statement they are for.
	mainStatements = []string{"SELECT", "INSERT", "UPDATE", "DELETE", "MERGE", "VALUES", "TABLE"}
)

// sqlToken is a word (upper-cased) or any other token (a quoted string or name, a number, a sign), with its depth in
// parentheses.
type sqlToken struct {
	text  string
	word  bool
	depth int
}

// explainRefusal says why a statement isn't explained (or with `analyze`, analyzed); "" when it is.
func explainRefusal(statement string, analyze bool) string {
	tokens := sqlTokens(statement)
	words := make([]string, len(tokens))
	for i, token := range tokens {
		if token.word {
			words[i] = token.text
		}
	}
	if len(words) == 0 || words[0] != "SELECT" && words[0] != "WITH" {
		return notSelect
	}
	with := words[0] == "WITH"
	for i, token := range tokens {
		if token.depth == 0 && !token.word && token.text == ";" {
			if i+1 < len(tokens) {
				return oneStatement
			}
			break
		}
	}
	main := 0
	if with {
		main = -1
		for i, token := range tokens {
			if token.word && token.depth == 0 && slices.Contains(mainStatements, token.text) {
				main = i
				break
			}
		}
	}
	previous := func(at int) string {
		if at == 0 {
			return ""
		}
		return words[at-1]
	}
	lock := func(at int) bool { return previous(at) == "FOR" || previous(at) == "KEY" }
	for at, word := range words {
		if slices.Contains(changing, word) && (word != "UPDATE" || !lock(at)) {
			if with && (main < 0 || at < main) {
				word += " inside WITH"
			}
			return changes(word)
		}
	}
	if slices.Contains(words, "INTO") {
		return changes("SELECT INTO")
	}
	if main < 0 || words[main] != "SELECT" {
		return notSelect
	}
	if !analyze {
		return ""
	}
	for at, word := range words {
		locks := ""
		switch {
		case word == "UPDATE" && lock(at):
			locks = "FOR UPDATE"
		case word == "SHARE" && lock(at):
			locks = "FOR SHARE"
		case word == "SHARE" && previous(at) == "IN" && at >= 2 && words[at-2] == "LOCK":
			locks = "LOCK IN SHARE MODE"
		default:
			continue
		}
		return fmt.Sprintf("This query locks rows (%s): it can be explained, not analyzed.", locks)
	}
	return ""
}

func changes(what string) string {
	return fmt.Sprintf("This query changes data (%s): it isn't explained.", what)
}

// sqlTokens are the statement's tokens, without its comments and spaces: a quoted string or name (single quotes with
// a doubled quote inside, E'…' with backslashes too, "…", `…`, […], PostgreSQL's $tag$…$tag$) is one token whose
// text is never read. The inside of MySQL's /*! … */, which MySQL runs, is read as the statement.
func sqlTokens(statement string) []sqlToken {
	chars := []rune(statement)
	at, depth := 0, 0
	var tokens []sqlToken
	get := func(i int) rune {
		if i < len(chars) {
			return chars[i]
		}
		return 0
	}
	for at < len(chars) {
		char, next := chars[at], get(at+1)
		var text string
		word := false
		switch {
		case unicode.IsSpace(char):
			at++
			continue
		case char == '-' && next == '-':
			at = pastRunes(chars, at+2, []rune{'\n'})
			continue
		case char == '/' && next == '*':
			if get(at+2) == '!' {
				at += 3
			} else {
				at = pastRunes(chars, at+2, []rune{'*', '/'})
			}
			continue
		case char == '\'':
			// E'…' (PostgreSQL's escape string): a backslash escapes the next character.
			escapes := at > 0 && (chars[at-1] == 'E' || chars[at-1] == 'e') &&
				len(tokens) > 0 && tokens[len(tokens)-1].word && tokens[len(tokens)-1].text == "E"
			at = quotedEnd(chars, at, '\'', escapes)
			text = "'"
		case char == '"' || char == '`':
			at = quotedEnd(chars, at, char, false)
			text = string(char)
		case char == '[' && (unicode.IsLetter(next) || next == '_'):
			at = pastRunes(chars, at+1, []rune{']'})
			text = "["
		case char == '$' && dollarTag(chars, at) != nil:
			tag := dollarTag(chars, at)
			at = pastRunes(chars, at+len(tag), tag)
			text = "$"
		case unicode.IsLetter(char) || char == '_':
			start := at
			for at < len(chars) && (unicode.IsLetter(chars[at]) || unicode.IsDigit(chars[at]) || chars[at] == '_' || chars[at] == '$') {
				at++
			}
			text, word = strings.ToUpper(string(chars[start:at])), true
		case char >= '0' && char <= '9':
			for at < len(chars) && (unicode.IsLetter(chars[at]) || unicode.IsDigit(chars[at]) || chars[at] == '_' || chars[at] == '.') {
				at++
			}
			text = "0"
		default:
			at++
			text = string(char)
		}
		if !word && text == ")" && depth > 0 {
			depth--
		}
		tokens = append(tokens, sqlToken{text: text, word: word, depth: depth})
		if !word && text == "(" {
			depth++
		}
	}
	return tokens
}

// quotedEnd is where a quote opened at `at` ends: past its closing `quote` (doubled, it is one inside), or the
// statement's end.
func quotedEnd(chars []rune, at int, quote rune, escapes bool) int {
	at++
	for at < len(chars) {
		switch {
		case escapes && chars[at] == '\\':
			at += 2
		case chars[at] == quote && at+1 < len(chars) && chars[at+1] == quote:
			at += 2
		case chars[at] == quote:
			return at + 1
		default:
			at++
		}
	}
	return len(chars)
}

// pastRunes is past the first `end` from `from` on, or the statement's end.
func pastRunes(chars []rune, from int, end []rune) int {
	for at := from; at+len(end) <= len(chars); at++ {
		if slices.Equal(chars[at:at+len(end)], end) {
			return at + len(end)
		}
	}
	return len(chars)
}

// dollarTag is PostgreSQL's dollar quote opening at `at`: $$ or $tag$ (not a $1 parameter); nil when there is none.
func dollarTag(chars []rune, at int) []rune {
	for end := at + 1; end < len(chars); end++ {
		char := chars[end]
		if char == '$' {
			return chars[at : end+1]
		}
		first := end == at+1
		if !(unicode.IsLetter(char) || char == '_' || !first && char >= '0' && char <= '9') {
			return nil
		}
	}
	return nil
}
