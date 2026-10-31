package tracepanel

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"mime"
	"mime/multipart"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"unicode/utf8"
)

// What a request carried, as attributes of its trace (the console's Request tab): its headers, query, cookies, the
// body as far as the app read it, and its response's headers. They stay on this machine (the agent keeps them out of
// what it sends anywhere), and the values under a sensitive name are masked before they are recorded, as a log
// record's fields are. A body is only what the app read itself, up to maxBody bytes: Tracepanel never reads a
// request's body for the app. JSON and forms are kept with their secrets masked, text as it is, binary content not;
// JSON or a form the app read only part of (or past maxBody) isn't kept, as its secrets can't be masked from a part.
// TRACEPANEL_CAPTURE_REQUEST=0 keeps none of it.

// How much of a body is kept, in bytes.
const maxBody = 16 * 1024

// A response's cookies are a session's keys, whatever their name.
var alwaysMasked = map[string]bool{"set-cookie": true}

// attrKey is a name as one segment of an attribute's key (Content-Type -> content_type), as every SDK writes them.
func attrKey(name string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(name) {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '_' {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
	}
	if b.Len() == 0 {
		return "_"
	}
	return b.String()
}

func headerAttrs(prefix string, header http.Header, skip string) attrMap {
	attrs := attrMap{}
	for name, values := range header {
		lowered := strings.ToLower(name)
		if lowered == skip || len(values) == 0 {
			continue
		}
		key := prefix + attrKey(lowered)
		if alwaysMasked[lowered] || secret.MatchString(strings.ReplaceAll(lowered, "-", "_")) {
			attrs[key] = mask
			continue
		}
		value := strings.Join(values, ", ")
		if before, ok := attrs[key].(string); ok {
			value = before + ", " + value
		}
		if value != "" {
			attrs[key] = value
		}
	}
	return attrs
}

// requestAttrs are what the request carried before the app read it: http.request.header.<name> (Cookie read as
// cookies), url.query and http.request.cookie.<name>.
func requestAttrs(r *http.Request) attrMap {
	attrs := headerAttrs("http.request.header.", r.Header, "cookie")
	if r.Host != "" {
		attrs["http.request.header.host"] = r.Host
	}
	if query := maskedQuery(r.URL.RawQuery); query != "" {
		attrs["url.query"] = query
	}
	for _, cookie := range r.Cookies() {
		value := cookie.Value
		if secret.MatchString(cookie.Name) {
			value = mask
		}
		attrs["http.request.cookie."+attrKey(cookie.Name)] = value
	}
	return attrs
}

// responseHeaders are http.response.header.<name>: several values joined, Set-Cookie masked; those Tracepanel added
// for the badge left out.
func responseHeaders(header http.Header) attrMap {
	own := header.Clone()
	own.Del("Tracepanel-Id")
	own.Del("Tracepanel-Url")
	for _, name := range []string{"Server-Timing", "Access-Control-Expose-Headers"} {
		values := own.Values(name)
		own.Del(name)
		for _, value := range values {
			if !strings.Contains(value, "tracepanel-issues;") && value != exposedHeaders {
				own.Add(name, value)
			}
		}
	}
	return headerAttrs("http.response.header.", own, "")
}

// maskedQuery is the query string in its order, the values of sensitive parameters masked.
func maskedQuery(raw string) string {
	if raw == "" {
		return ""
	}
	var pairs []string
	for _, part := range strings.Split(raw, "&") {
		if part == "" {
			continue
		}
		name, value, _ := strings.Cut(part, "=")
		name, value = unescape(name), unescape(value)
		if secret.MatchString(name) {
			value = mask
		}
		pairs = append(pairs, quote(name, "_.~*/:-", true)+"="+quote(value, "_.~*/:-", true))
	}
	return strings.Join(pairs, "&")
}

func unescape(text string) string {
	if decoded, err := url.QueryUnescape(text); err == nil {
		return decoded
	}
	return text
}

// quote percent-encodes all but letters, digits and `safe`; a space as `+` when plus.
func quote(text, safe string, plus bool) string {
	var b strings.Builder
	for _, c := range []byte(text) {
		switch {
		case plus && c == ' ':
			b.WriteByte('+')
		case c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c < 0x80 && strings.IndexByte(safe, c) >= 0:
			b.WriteByte(c)
		default:
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}

// bodyTap is the request's body as the app reads it: each read passes through, and the start of what it read is
// kept, up to maxBody (one byte more, to tell a body past it).
type bodyTap struct {
	inner io.ReadCloser
	mu    sync.Mutex
	kept  bytes.Buffer
	read  int64
	eof   bool
}

// tapBody puts a tap on the request's body: nil when it has none.
func tapBody(r *http.Request) *bodyTap {
	if r.Body == nil || r.Body == http.NoBody {
		return nil
	}
	tap := &bodyTap{inner: r.Body}
	r.Body = tap
	return tap
}

func (b *bodyTap) Read(p []byte) (int, error) {
	n, err := b.inner.Read(p)
	b.mu.Lock()
	b.read += int64(n)
	if room := maxBody + 1 - b.kept.Len(); room > 0 && n > 0 {
		b.kept.Write(p[:min(n, room)])
	}
	if errors.Is(err, io.EOF) {
		b.eof = true
	}
	b.mu.Unlock()
	return n, err
}

func (b *bodyTap) Close() error { return b.inner.Close() }

// attrs are http.request.body (and http.request.body.truncated) of what the app read; a form the app parsed past
// what was kept is read from the request.
func (b *bodyTap) attrs(contentType string, r *http.Request) attrMap {
	if b == nil {
		return nil
	}
	kind, params, _ := mime.ParseMediaType(contentType)
	b.mu.Lock()
	data := bytes.Clone(b.kept.Bytes())
	// All of it is at hand: the app read to its end, or as much as it said it was (a decoder stops at the end of
	// its value, before the end of the stream).
	complete := b.read <= maxBody && (b.eof || r.ContentLength >= 0 && b.read >= r.ContentLength)
	b.mu.Unlock()
	if len(data) == 0 {
		return nil
	}
	var text string
	switch {
	case kind == "application/json" || strings.HasSuffix(kind, "+json"):
		if !complete {
			return nil
		}
		masked, ok := maskJSON(data)
		if !ok {
			return nil
		}
		text = masked
	case kind == "application/x-www-form-urlencoded":
		if !complete {
			if r.PostForm == nil {
				return nil
			}
			return cutBody(formText(orderedForm(r.PostForm), nil))
		}
		text = maskedForm(string(data))
	case kind == "multipart/form-data":
		// A multipart reader stops at the closing boundary.
		closed := params["boundary"] != "" && bytes.Contains(data, []byte("--"+params["boundary"]+"--"))
		if complete || closed && b.read <= maxBody {
			text = multipartText(data, params["boundary"])
		} else if r.MultipartForm != nil {
			text = parsedMultipart(r.MultipartForm)
		}
	case strings.HasPrefix(kind, "text/") || kind == "application/xml" || kind == "application/graphql" ||
		strings.HasSuffix(kind, "+xml"):
		if !utf8.Valid(data) {
			// Cut inside a character: up to it.
			for len(data) > 0 && !utf8.Valid(data) {
				data = data[:len(data)-1]
			}
		}
		text = string(data)
		if !complete {
			return attrMap{"http.request.body": string(data[:min(len(data), maxBody)]), "http.request.body.truncated": true}
		}
	}
	if text == "" {
		return nil
	}
	return cutBody(text)
}

func cutBody(text string) attrMap {
	if len(text) <= maxBody {
		return attrMap{"http.request.body": text}
	}
	cut := text[:maxBody]
	for len(cut) > 0 && !utf8.ValidString(cut) {
		cut = cut[:len(cut)-1]
	}
	return attrMap{"http.request.body": cut, "http.request.body.truncated": true}
}

// maskJSON is the JSON document as it came (its keys in their order, its numbers as written), on one line, the
// values under sensitive keys masked at any depth.
func maskJSON(data []byte) (string, bool) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	var out bytes.Buffer
	if !maskValue(decoder, &out) {
		return "", false
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return "", false
	}
	return out.String(), true
}

// maskValue writes the next JSON value of the decoder.
func maskValue(decoder *json.Decoder, out *bytes.Buffer) bool {
	token, err := decoder.Token()
	if err != nil {
		return false
	}
	switch token {
	case json.Delim('{'):
		out.WriteByte('{')
		for first := true; decoder.More(); first = false {
			key, err := decoder.Token()
			name, ok := key.(string)
			if err != nil || !ok {
				return false
			}
			if !first {
				out.WriteByte(',')
			}
			encoded, _ := json.Marshal(name)
			out.Write(encoded)
			out.WriteByte(':')
			if secret.MatchString(name) {
				var skipped json.RawMessage
				if decoder.Decode(&skipped) != nil {
					return false
				}
				out.WriteString(`"` + mask + `"`)
				continue
			}
			if !maskValue(decoder, out) {
				return false
			}
		}
		if _, err := decoder.Token(); err != nil {
			return false
		}
		out.WriteByte('}')
	case json.Delim('['):
		out.WriteByte('[')
		for first := true; decoder.More(); first = false {
			if !first {
				out.WriteByte(',')
			}
			if !maskValue(decoder, out) {
				return false
			}
		}
		if _, err := decoder.Token(); err != nil {
			return false
		}
		out.WriteByte(']')
	default:
		encoded, err := json.Marshal(token)
		if err != nil {
			return false
		}
		out.Write(encoded)
	}
	return true
}

// The characters a form keeps as they are: spaces, masks and file notes read as people read them.
const readable = "_.~*<>,. /:@-"

type field struct{ name, value string }

// maskedForm is a URL-encoded form in its order, sensitive values masked.
func maskedForm(raw string) string {
	var fields []field
	for _, part := range strings.Split(raw, "&") {
		if part == "" {
			continue
		}
		name, value, _ := strings.Cut(part, "=")
		fields = append(fields, field{unescape(name), unescape(value)})
	}
	return formText(fields, nil)
}

// formText is a form's fields as they would have been sent, sensitive values masked, then its files by name and
// size.
func formText(fields []field, files []field) string {
	pairs := make([]string, 0, len(fields)+len(files))
	for _, f := range fields {
		value := f.value
		if secret.MatchString(f.name) {
			value = mask
		}
		pairs = append(pairs, quote(f.name, readable, false)+"="+quote(value, readable, false))
	}
	for _, f := range files {
		pairs = append(pairs, quote(f.name, readable, false)+"="+quote(f.value, readable, false))
	}
	return strings.Join(pairs, "&")
}

func orderedForm(values url.Values) []field {
	var fields []field
	for _, name := range sortedKeys(values) {
		for _, value := range values[name] {
			fields = append(fields, field{name, value})
		}
	}
	return fields
}

func sortedKeys[V any](values map[string]V) []string { return slices.Sorted(maps.Keys(values)) }

// multipartText is a multipart form read whole: its fields, then its files by name and size.
func multipartText(data []byte, boundary string) string {
	if boundary == "" {
		return ""
	}
	reader := multipart.NewReader(bytes.NewReader(data), boundary)
	var fields, files []field
	for {
		part, err := reader.NextPart()
		if err != nil {
			break
		}
		content, _ := io.ReadAll(part)
		if part.FileName() != "" {
			files = append(files, field{part.FormName(), fmt.Sprintf("<file %s, %d bytes>", part.FileName(), len(content))})
		} else if part.FormName() != "" {
			fields = append(fields, field{part.FormName(), string(content)})
		}
	}
	return formText(fields, files)
}

// parsedMultipart is a form the app parsed (ParseMultipartForm) past what the tap kept.
func parsedMultipart(form *multipart.Form) string {
	fields := orderedForm(form.Value)
	var files []field
	for _, name := range sortedKeys(form.File) {
		for _, file := range form.File[name] {
			files = append(files, field{name, fmt.Sprintf("<file %s, %d bytes>", file.Filename, file.Size)})
		}
	}
	return formText(fields, files)
}
