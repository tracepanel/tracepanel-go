package tracepanel

import (
	"net/http"
	"strconv"
	"time"
)

// Outgoing HTTP calls: each is an `http.client` span of the trace in its request's context, and carries a
// traceparent, so the app it reaches (with Tracepanel of its own) links its trace to the call.

// Transport records the calls made through `base` (http.DefaultTransport when nil):
//
//	client := &http.Client{Transport: tracepanel.Transport(nil)}
//
// Setup gives it to http.DefaultClient, so its calls are recorded already. http.DefaultTransport stays as it is:
// code asserts it is an *http.Transport. A call is recorded when its request has the context of a traced request:
// use http.NewRequestWithContext(ctx, ...).
func Transport(base http.RoundTripper) http.RoundTripper {
	if base == nil {
		base = http.DefaultTransport
	}
	if _, ok := base.(*transport); ok {
		return base
	}
	return &transport{base: base}
}

type transport struct {
	base http.RoundTripper
}

func installTransport() {
	if http.DefaultClient.Transport == nil {
		http.DefaultClient.Transport = Transport(nil)
	}
}

func (t *transport) RoundTrip(req *http.Request) (*http.Response, error) {
	s := scopeOf(req.Context())
	if s == nil || req.Header.Get(traceparentHeader) != "" {
		return t.base.RoundTrip(req)
	}
	start := time.Now()
	id := newID()
	where := origin()
	// RoundTrip mustn't change the caller's request: a copy carries the header.
	out := req.Clone(req.Context())
	out.Header.Set(traceparentHeader, outgoing(s.rec, id))
	response, err := t.base.RoundTrip(out)
	url := req.URL
	scheme := url.Scheme
	port := url.Port()
	attrs := Attrs{
		"http.request.method": req.Method,
		"url.full":            url.Redacted(),
		"url.scheme":          scheme,
		"server.address":      url.Hostname(),
		"w3c.span_id":         w3cSpan(id),
		"code.origin":         nilIfEmpty(where),
	}
	if number, convErr := strconv.Atoi(port); convErr == nil {
		attrs["server.port"] = number
	} else if scheme == "https" {
		attrs["server.port"] = 443
	} else {
		attrs["server.port"] = 80
	}
	failed := err != nil
	if err != nil {
		attrs["error.type"] = errorType(err)
	} else {
		attrs["http.response.status_code"] = response.StatusCode
		failed = response.StatusCode >= 500
	}
	s.rec.done(s.span, id, "http.client", req.Method+" "+url.Host+url.Path, attrs, start, failed)
	return response, err
}

// errorType names what failed a call: a timeout, a refused connection, or the error's type.
func errorType(err error) string {
	type timeout interface{ Timeout() bool }
	if t, ok := err.(timeout); ok && t.Timeout() {
		return "timeout"
	}
	return shortType(err)
}
