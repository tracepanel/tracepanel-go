package tracepanel

import (
	"reflect"
	"strings"
)

// Which names hold a secret, read word by word as every Tracepanel SDK reads them (the Python SDK's
// tracepanel.core.masking.sensitive, the Java SDK's Secrets.sensitive): a request's headers, query, cookies and
// body, a log record's fields, a dump's expressions and the fields and keys inside its values.
//
// A name is split into words at anything but a letter or a digit (_ - . [ ] and the like) and where camelCase turns:
// x-api-key, card[number], cardNumber, APIKey and X-CSRFToken are (x, api, key), (card, number), (card, number),
// (api, key), (x, csrf, token). A secret's word must be a whole word, so `author`, `authority` and
// `is_authenticated` aren't `auth`, and `tokenizer` isn't `token`. Words written together are read too: csrftoken
// and accesstoken end in a token, sessionid starts with a session, PHPSESSID ends in one.
//
// Two kinds of names hold no secret though a secret's word is in them: one about the secret (password_length,
// token_type, session_timeout, cookie_name, tokenCount) and a yes or no (is_private, hasPassword). `tokens` alone
// is a secret unless what it holds is counts (counted): a model's tokens, {"tokens": 1024}.

const mask = "********"

var (
	cardWords   = wordSet("pan", "cvc", "cvv", "cvn", "cvc2", "cvv2", "cardnumber", "cardnum", "cardno")
	secretWords = wordSet(
		"pass", "passwd", "password", "passwords", "passphrase", "passcode", "passkey", "secret", "secrets",
		"secretkey", "token", "apikey", "apikeys", "auth", "oauth", "oauth2", "authorization", "authentication",
		"cookie", "cookies", "session", "sessions", "csrf", "xsrf", "signature", "signatures", "private",
	)
	// Two words that make a secret together: card number, api key, refresh tokens.
	secretPairs = func() map[[2]string]bool {
		pairs := map[[2]string]bool{
			{"card", "number"}: true, {"card", "num"}: true, {"card", "no"}: true, {"api", "key"}: true, {"api", "keys"}: true,
		}
		for _, word := range []string{"refresh", "access", "auth", "api", "bearer", "id", "session", "oauth", "csrf",
			"xsrf", "jwt", "client"} {
			pairs[[2]string{word, "tokens"}] = true
		}
		return pairs
	}()
	// The last word of a name about a secret, not the secret.
	aboutWords = wordSet(
		"count", "total", "length", "len", "min", "max", "size", "age", "timeout", "ttl", "expiry", "expires",
		"expiration", "at", "type", "name", "field", "label", "required", "enabled",
	)
	// The first word of a yes or no.
	yesOrNo = wordSet("is", "has")
	// Words written together: accesstoken, csrftoken, clientsecret, newpassword, xapikey, jsessionid, PHPSESSID...
	secretEnds = []string{"token", "secret", "password", "passwd", "apikey", "sessionid", "sessid"}
	// ...and csrfmiddlewaretoken, sessionkey, cookiejar. Not `secret` (secretary), `token` (tokenizer) nor `auth`
	// (author).
	secretStarts = []string{"password", "apikey", "csrf", "xsrf", "session", "cookie"}
)

func wordSet(words ...string) map[string]bool {
	set := make(map[string]bool, len(words))
	for _, word := range words {
		set[word] = true
	}
	return set
}

func lower(c byte) bool { return c >= 'a' && c <= 'z' }
func upper(c byte) bool { return c >= 'A' && c <= 'Z' }
func digit(c byte) bool { return c >= '0' && c <= '9' }
func alnum(c byte) bool { return lower(c) || upper(c) || digit(c) }

// nameWords are a name's words, lowercase: X-CSRFToken is (x, csrf, token), card[number] (card, number).
func nameWords(name string) []string {
	var words []string
	start := -1
	flush := func(end int) {
		if start >= 0 && end > start {
			words = append(words, strings.ToLower(name[start:end]))
		}
		start = -1
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		if !alnum(c) {
			flush(i)
			continue
		}
		if start >= 0 && upper(c) {
			prev := name[i-1]
			// camelCase: a capital after a small letter or a digit; and the last capital of a run before a small
			// letter (APIKey: API, Key).
			if lower(prev) || digit(prev) || upper(prev) && i+1 < len(name) && lower(name[i+1]) {
				flush(i)
			}
		}
		if start < 0 {
			start = i
		}
	}
	flush(len(name))
	return words
}

// sensitive tells whether a secret goes under the name: password, x-api-key, Authorization, csrftoken, sessionId,
// authToken, AUTH_HEADER, card[cvc]; not author, tokenizer, password_length nor hasPassword.
func sensitive(name string) bool {
	found := nameWords(name)
	if len(found) == 1 && found[0] == "tokens" {
		return true
	}
	if len(found) > 1 && (yesOrNo[found[0]] || aboutWords[found[len(found)-1]]) {
		return false
	}
	for i, word := range found {
		if secretWords[word] || cardWords[word] {
			return true
		}
		for _, end := range secretEnds {
			if strings.HasSuffix(word, end) {
				return true
			}
		}
		for _, start := range secretStarts {
			if strings.HasPrefix(word, start) {
				return true
			}
		}
		if i > 0 && secretPairs[[2]string{found[i-1], word}] {
			return true
		}
	}
	return false
}

// counted tells whether `tokens` holds a model's counts, not credentials: a number, or numbers only (a slice, an
// array, a map's values).
func counted(name string, value any) bool {
	return countedValue(name, reflect.ValueOf(value))
}

func countedValue(name string, v reflect.Value) bool {
	if found := nameWords(name); len(found) != 1 || found[0] != "tokens" {
		return false
	}
	for v.IsValid() && (v.Kind() == reflect.Interface || v.Kind() == reflect.Pointer) && !v.IsNil() {
		v = v.Elem()
	}
	switch v.Kind() {
	case reflect.Slice, reflect.Array:
		for i := range v.Len() {
			if !isNumber(v.Index(i)) {
				return false
			}
		}
		return true
	case reflect.Map:
		iter := v.MapRange()
		for iter.Next() {
			if !isNumber(iter.Value()) {
				return false
			}
		}
		return true
	}
	return isNumber(v)
}

// isNumber: an integer or a float (not a bool, not a string), through interfaces and pointers.
func isNumber(v reflect.Value) bool {
	for v.IsValid() && (v.Kind() == reflect.Interface || v.Kind() == reflect.Pointer) && !v.IsNil() {
		v = v.Elem()
	}
	switch v.Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64, reflect.Uint, reflect.Uint8,
		reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Float32, reflect.Float64:
		return true
	}
	return false
}

// masked tells whether a value under the name is masked: the name is a secret's, and not `tokens` holding counts.
func masked(name string, value any) bool {
	return sensitive(name) && !counted(name, value)
}
