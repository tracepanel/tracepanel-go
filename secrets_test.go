package tracepanel

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The Python SDK's cases (tests/core/test_masking.py) and the Java SDK's: one rule in every SDK.

// Names a request, a log record's fields and a dump mask.
var secretNames = []string{
	// Passwords.
	"password", "passwd", "db_pass", "passphrase", "passcode", "passkey", "new_password", "password1",
	"password_confirm", "passwordConfirm", "passwords", "password_hash",
	// Secrets.
	"secret", "secrets", "client_secret", "client_secrets", "clientSecret", "clientsecret", "webhook_secret",
	"secret_key", "secretKey",
	// Tokens, and in the plural: credentials after a credential's word, and alone (unless counted).
	"token", "access_token", "accessToken", "accesstoken", "refresh_token", "id_token", "tokens", "refresh_tokens",
	"access_tokens", "accessTokens", "auth_tokens", "api_tokens", "bearer_tokens", "id_tokens", "session_tokens",
	"csrftoken", "csrf_token", "X-CSRFToken", "csrfmiddlewaretoken", "X-XSRF-TOKEN", "authenticity_token",
	"x-amz-security-token", "authToken",
	// API keys.
	"api_key", "apiKey", "APIKey", "apikey", "x-api-key", "X-Api-Key", "api_keys", "apiKeys",
	// Auth: the header, its schemes, OAuth's.
	"auth", "x-auth", "X-Auth-Token", "basic_auth", "HTTPBasicAuth", "Authorization", "Proxy-Authorization",
	"authorization_code", "authentication", "oauth", "oauth2", "oauth_token", "oauth_secret", "OAuthToken",
	"_auth_user_hash", "AUTH_HEADER",
	// Cookies and sessions.
	"cookie", "cookies", "Set-Cookie", "cookiejar", "session", "sessions", "sessionid", "sessionId", "session_id",
	"session_key", "PHPSESSID", "JSESSIONID",
	// CSRF, signatures, private keys.
	"csrf", "xsrf", "signature", "X-Hub-Signature-256", "Stripe-Signature", "private", "private_key", "privateKey",
	// Payment cards.
	"card_number", "cardNumber", "cardnumber", "card_no", "card_num", "card-number", "card[number]",
	"credit_card_number", "card_number_last4", "pan", "card_pan", "cvc", "cvv", "cvv2", "cvn", "card_cvc",
	"card[cvc]", "cvc_token",
	// A Go struct's fields and expressions.
	"Password", "APIToken", "user.Password", "cfg.APIKey", "SessionID",
}

// Names that only look like one: a secret's word inside another word, a name about a secret, a yes or no.
var plainNames = []string{
	// Not auth.
	"author", "Author", "authors", "author_id", "authorId", "AuthorID", "authored_by", "authorship", "authority",
	"authorities", "authenticated", "is_authenticated", "authenticator", "authorized", "WWW-Authenticate",
	// Not a token: a model's tokens are counted.
	"tokenizer", "max_tokens", "prompt_tokens", "total_tokens", "completion_tokens", "input_tokens",
	// Not a password, a secret or a card.
	"bypass", "compass", "passenger", "passport", "secretary", "company", "span", "expand", "pane", "card_brand",
	"card_last4", "discount", "number", "account", "key",
	// About a secret.
	"passwords_count", "password_length", "token_type", "token_expires_at", "session_timeout", "cookie_name",
	"tokenCount", "tokens.size",
	// Yes or no.
	"has_password", "is_private", "hasPassword", "IsPrivate",
}

func TestANameWithASecretsWordIsMasked(t *testing.T) {
	for _, name := range secretNames {
		if !sensitive(name) {
			t.Errorf("%s: shown", name)
		}
	}
}

func TestANameWithoutOneIsShown(t *testing.T) {
	for _, name := range plainNames {
		if sensitive(name) {
			t.Errorf("%s: masked", name)
		}
	}
}

func TestANameIsReadWordByWord(t *testing.T) {
	cases := map[string][]string{
		"X-CSRFToken":      {"x", "csrf", "token"},
		"card[number]":     {"card", "number"},
		"creditCardNumber": {"credit", "card", "number"},
		"APIKey":           {"api", "key"},
		"source.exp_month": {"source", "exp", "month"},
		"oauth2":           {"oauth2"},
		"__Host-Lang":      {"host", "lang"},
		"AUTH_HEADER":      {"auth", "header"},
		"userID2FA":        {"user", "id2", "fa"},
		"":                 nil,
	}
	for name, want := range cases {
		if got := nameWords(name); strings.Join(got, ",") != strings.Join(want, ",") {
			t.Errorf("%q: %q, want %q", name, got, want)
		}
	}
}

func TestTokensAloneHoldingCountsAreAModels(t *testing.T) {
	count := 7
	cases := []struct {
		name  string
		value any
		want  bool
	}{
		// A model's counts.
		{"tokens", 1024, true},
		{"tokens", []int{12, 7}, true},
		{"tokens", map[string]any{"prompt": 12, "completion": 7.0}, true},
		{"tokens", 1.5, true},
		{"tokens", []any{}, true},
		{"Tokens", &count, true},
		// Credentials, or not only counts.
		{"tokens", []string{"abc", "def"}, false},
		{"tokens", "abc", false},
		{"tokens", []any{1, "abc"}, false},
		{"tokens", map[string]string{"github": "abc"}, false},
		{"tokens", true, false},
		{"tokens", nil, false},
		// Not `tokens` alone: the name decides.
		{"refresh_tokens", []int{1}, false},
		{"token", 1, false},
		{"password", 1234, false},
	}
	for _, c := range cases {
		if got := counted(c.name, c.value); got != c.want {
			t.Errorf("%s %#v: %v, want %v", c.name, c.value, got, c.want)
		}
	}
}

// One rule wherever a name masks a value: a request's headers, query, cookies and body, a log record's fields, a dump.
func TestEveryCaptureMasksByTheSameRule(t *testing.T) {
	recorded := record(t)
	recorded.t.captureRequest = true
	out := printed(t)
	type login struct {
		Author    string
		AuthToken string
		Tokens    []int
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /login", func(w http.ResponseWriter, r *http.Request) {
		buf := make([]byte, 512)
		_, _ = r.Body.Read(buf)
		slog.New(WrapLogHandler(slog.NewTextHandler(io.Discard, nil))).InfoContext(r.Context(), "in", "author", "Le Guin", "authToken", "t-1",
			"tokens", 1024)
		value := login{Author: "Le Guin", AuthToken: "t-2", Tokens: []int{12, 7}}
		Dump(r.Context(), value)
	})
	request := httptest.NewRequest(http.MethodPost, "/login?author=leguin&auth_token=t-3", strings.NewReader(
		`{"author":"Le Guin","AUTH_HEADER":"t-4","tokens":[12,7],"usage":{"tokens":"t-5"}}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Api-Key", "k-1")
	request.Header.Set("X-Author", "leguin")
	request.AddCookie(&http.Cookie{Name: "author_pref", Value: "dark"})
	request.AddCookie(&http.Cookie{Name: "sessionId", Value: "s-1"})

	recorded.t.middleware(mux).ServeHTTP(httptest.NewRecorder(), request)

	doc := recorded.wait(t, 1)[0]
	for key, want := range map[string]any{
		"http.request.header.x_api_key":   mask,
		"http.request.header.x_author":    "leguin",
		"http.request.cookie.author_pref": "dark",
		"http.request.cookie.sessionid":   mask,
		"url.query":                       "author=leguin&auth_token=********",
		"http.request.body":               `{"author":"Le Guin","AUTH_HEADER":"********","tokens":[12,7],"usage":{"tokens":"********"}}`,
	} {
		if doc.Attrs[key] != want {
			t.Errorf("%s: %v, want %v", key, doc.Attrs[key], want)
		}
	}
	var logged, dumped attrMap
	for _, record := range doc.Logs {
		if record.Attrs["log.kind"] == "dump" {
			dumped = record.Attrs
		} else {
			logged = record.Attrs
		}
	}
	if logged["extra.author"] != "Le Guin" || logged["extra.authToken"] != mask || logged["extra.tokens"] != int64(1024) {
		t.Errorf("log fields %v", logged)
	}
	repr := dumped["dump.args"].([]any)[0].(dumpEntry).Repr
	want := "tracepanel.login{\n    Author:    \"Le Guin\",\n    AuthToken: ********,\n    Tokens:    []int{12, 7},\n}"
	if repr != want || strings.Contains(out.String(), "t-2") {
		t.Errorf("dump %q, printed %q", repr, out.String())
	}
}
