package tracepanel

import (
	"bytes"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// captured runs one request through a handler with the request's capture on: its trace's attributes of what the
// request carried (http.request.*, url.query) and what the response said (http.response.header.*).
func captured(t *testing.T, request *http.Request, handler http.HandlerFunc) attrMap {
	t.Helper()
	recorded := record(t)
	recorded.t.captureRequest = true
	recorded.t.middleware(handler).ServeHTTP(httptest.NewRecorder(), request)
	attrs := attrMap{}
	for key, value := range recorded.wait(t, 1)[0].Attrs {
		if strings.HasPrefix(key, "http.request.") || strings.HasPrefix(key, "http.response.header.") || key == "url.query" {
			attrs[key] = value
		}
	}
	return attrs
}

func readAll(w http.ResponseWriter, r *http.Request) {
	_, _ = io.ReadAll(r.Body)
}

func TestARequestsHeadersQueryCookiesAndJSONAreKeptMasked(t *testing.T) {
	request := httptest.NewRequest(http.MethodPost, "/orders?page=2&token=abc&q=red+shoes", strings.NewReader(
		`{"item": "book", "count": 2, "price": 12.50, "card": {"number": "4242", "password": "hunter2"}, "tags": ["a", {"api_key": "k"}]}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer secret")
	request.Header.Set("X-Api-Key", "k")
	request.Header.Set("User-Agent", "curl/8")
	request.Header.Add("Accept", "text/html")
	request.Header.Add("Accept", "application/json")
	request.Header.Set("Cookie", "theme=dark; sessionid=s3cr3t")

	attrs := captured(t, request, func(w http.ResponseWriter, r *http.Request) {
		readAll(w, r)
		http.SetCookie(w, &http.Cookie{Name: "theme", Value: "light"})
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	})

	if !equalJSON(attrs, attrMap{
		"http.request.header.content_type":  "application/json",
		"http.request.header.authorization": "********",
		"http.request.header.x_api_key":     "********",
		"http.request.header.user_agent":    "curl/8",
		"http.request.header.accept":        "text/html, application/json",
		"http.request.header.host":          "example.com",
		"url.query":                         "page=2&token=********&q=red+shoes",
		"http.request.cookie.theme":         "dark",
		"http.request.cookie.sessionid":     "********",
		"http.request.body": `{"item":"book","count":2,"price":12.50,"card":{"number":"4242","password":"********"},` +
			`"tags":["a",{"api_key":"********"}]}`,
		"http.response.header.set_cookie":   "********",
		"http.response.header.content_type": "application/json",
	}) {
		t.Fatalf("attrs %v", attrs)
	}
}

func TestABodyTheAppDidntReadIsntKept(t *testing.T) {
	request := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"a": 1}`))
	request.Header.Set("Content-Type", "application/json")

	attrs := captured(t, request, func(w http.ResponseWriter, r *http.Request) {})

	if _, ok := attrs["http.request.body"]; ok {
		t.Fatalf("attrs %v", attrs)
	}
}

func TestAFormIsKeptAsItWasSentMasked(t *testing.T) {
	request := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader("user=ann+lee&password=hunter2&next=%2Fhome"))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	attrs := captured(t, request, func(w http.ResponseWriter, r *http.Request) { _ = r.ParseForm() })

	if attrs["http.request.body"] != "user=ann lee&password=********&next=/home" {
		t.Fatalf("body %q", attrs["http.request.body"])
	}
}

func TestAMultipartFormKeepsItsFieldsAndItsFilesBySize(t *testing.T) {
	var body bytes.Buffer
	form := multipart.NewWriter(&body)
	_ = form.WriteField("title", "Dune")
	_ = form.WriteField("csrf_token", "t0k3n")
	file, _ := form.CreateFormFile("cover", "dune.png")
	_, _ = file.Write(make([]byte, 300))
	_ = form.Close()
	request := httptest.NewRequest(http.MethodPost, "/books", &body)
	request.Header.Set("Content-Type", form.FormDataContentType())

	attrs := captured(t, request, func(w http.ResponseWriter, r *http.Request) { _ = r.ParseMultipartForm(1 << 20) })

	if attrs["http.request.body"] != "title=Dune&csrf_token=********&cover=<file dune.png, 300 bytes>" {
		t.Fatalf("body %q", attrs["http.request.body"])
	}
}

func TestABodyPastTheLimit(t *testing.T) {
	cases := map[string]struct {
		contentType string
		body        string
		want        attrMap
	}{
		"text is cut": {"text/plain", strings.Repeat("a", maxBody+10),
			attrMap{"http.request.body": strings.Repeat("a", maxBody), "http.request.body.truncated": true}},
		"JSON isn't kept":   {"application/json", `{"a": "` + strings.Repeat("a", maxBody) + `"}`, attrMap{}},
		"binary isn't kept": {"application/octet-stream", "\x00\x01\x02", attrMap{}},
		"broken JSON isn't": {"application/json", `{"a": `, attrMap{}},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(c.body))
			request.Header.Set("Content-Type", c.contentType)

			attrs := captured(t, request, readAll)

			got := attrMap{}
			for key, value := range attrs {
				if strings.HasPrefix(key, "http.request.body") {
					got[key] = value
				}
			}
			if !equalJSON(got, c.want) {
				t.Fatalf("got %v", got)
			}
		})
	}
}

func TestNothingIsCapturedWhenItIsOff(t *testing.T) {
	recorded := record(t)
	request := httptest.NewRequest(http.MethodGet, "/?token=1", nil)
	request.Header.Set("User-Agent", "curl/8")

	recorded.t.middleware(http.NotFoundHandler()).ServeHTTP(httptest.NewRecorder(), request)

	for key := range recorded.wait(t, 1)[0].Attrs {
		if strings.HasPrefix(key, "http.request.header.") || key == "url.query" {
			t.Fatalf("captured %s", key)
		}
	}
}

func TestJSONADecoderReadIsKept(t *testing.T) {
	request := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"title": "Dune", "token": "t"}`))
	request.Header.Set("Content-Type", "application/json")

	// A decoder stops at the end of the value, without reading to the stream's end.
	attrs := captured(t, request, func(w http.ResponseWriter, r *http.Request) {
		var book map[string]string
		_ = json.NewDecoder(r.Body).Decode(&book)
	})

	if attrs["http.request.body"] != `{"title":"Dune","token":"********"}` {
		t.Fatalf("body %q", attrs["http.request.body"])
	}
}
