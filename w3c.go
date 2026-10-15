package tracepanel

import (
	"fmt"
	"math/big"
	"regexp"
	"strings"
)

// W3C trace context (traceparent), as every Tracepanel SDK writes it: a call out carries 00-(trace id)-(span id)-01,
// the trace id being the one the request came in with, else this trace's own ULID's 128 bits in hex, and the span id
// the last 64 bits of the call's span's ULID. The app it reaches links its trace to the call by them.

const traceparentHeader = "traceparent"

var traceparent = regexp.MustCompile(`^([0-9a-f]{2})-([0-9a-f]{32})-([0-9a-f]{16})-([0-9a-f]{2})`)

// incoming is a received traceparent as its trace's attributes, the W3C trace and the caller's span; none if invalid.
func incoming(header string) Attrs {
	match := traceparent.FindStringSubmatch(strings.ToLower(strings.TrimSpace(header)))
	if match == nil || match[1] == "ff" || match[2] == strings.Repeat("0", 32) || match[3] == strings.Repeat("0", 16) {
		return nil
	}
	return Attrs{"w3c.trace_id": match[2], "w3c.parent_span_id": match[3]}
}

// outgoing is the traceparent a call made in `rec` carries, whose span is `spanID`.
func outgoing(rec *recording, spanID string) string {
	return "00-" + w3cTrace(rec) + "-" + w3cSpan(spanID) + "-01"
}

// w3cTrace is the W3C trace this trace is part of: the one it was called in, else its own id, kept on it from now on.
func w3cTrace(rec *recording) string {
	if known, ok := rec.attr("w3c.trace_id").(string); ok && known != "" {
		return known
	}
	own := hexOf(ulidBits(rec.doc.ID), 32)
	rec.set("w3c.trace_id", own)
	return own
}

// w3cSpan is a span's W3C id: the last 64 bits of its ULID, in hex.
func w3cSpan(ulid string) string {
	mask := new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), 64), big.NewInt(1))
	return hexOf(new(big.Int).And(ulidBits(ulid), mask), 16)
}

func ulidBits(ulid string) *big.Int {
	value := new(big.Int)
	for _, c := range ulid {
		value.Lsh(value, 5)
		value.Or(value, big.NewInt(int64(strings.IndexRune(crockford, c))))
	}
	return value
}

func hexOf(value *big.Int, width int) string {
	return fmt.Sprintf("%0*x", width, value)
}
