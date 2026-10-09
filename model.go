package tracepanel

import (
	"crypto/rand"
	"encoding/binary"
	"strings"
	"time"
)

// The trace as every tracepanel SDK sends it to the agent.

type Attrs map[string]any

type Trace struct {
	ID       string  `json:"id"`
	Kind     string  `json:"kind"`
	Name     string  `json:"name"`
	Start    int64   `json:"start"`
	Duration int64   `json:"duration"`
	Status   string  `json:"status"`
	Attrs    Attrs   `json:"attrs"`
	Spans    []Span  `json:"spans"`
	Logs     []Log   `json:"logs"`
	Errors   []Error `json:"errors"`
	Links    []any   `json:"links"`
	Issues   []any   `json:"issues"`
}

type Span struct {
	ID       string  `json:"id"`
	ParentID *string `json:"parent_id"`
	Kind     string  `json:"kind"`
	Name     string  `json:"name"`
	Start    int64   `json:"start"`
	Duration int64   `json:"duration"`
	Status   string  `json:"status"`
	Attrs    Attrs   `json:"attrs"`
}

type Log struct {
	SpanID    *string `json:"span_id"`
	Time      int64   `json:"time"`
	Level     string  `json:"level"`
	Logger    string  `json:"logger"`
	Message   string  `json:"message"`
	Exception *string `json:"exception"`
	Attrs     Attrs   `json:"attrs"`
}

type Error struct {
	SpanID  *string `json:"span_id"`
	Time    int64   `json:"time"`
	Type    string  `json:"type"`
	Message string  `json:"message"`
	Handled bool    `json:"handled"`
	Frames  []Frame `json:"frames"`
	Chain   []any   `json:"chain"`
}

type Frame struct {
	Filename     string            `json:"filename"`
	Module       string            `json:"module"`
	Function     string            `json:"function"`
	Lineno       int               `json:"lineno"`
	InApp        bool              `json:"in_app"`
	ContextStart *int              `json:"context_start"`
	Context      []string          `json:"context"`
	Locals       map[string]string `json:"locals"`
}

func micros(t time.Time) int64 { return t.UnixMicro() }

const crockford = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

// newID is a ULID: 48 bits of milliseconds, 80 random bits, 26 characters of Crockford base32.
func newID() string {
	var raw [16]byte
	binary.BigEndian.PutUint64(raw[0:8], uint64(time.Now().UnixMilli())<<16)
	_, _ = rand.Read(raw[6:])
	hi, lo := binary.BigEndian.Uint64(raw[0:8]), binary.BigEndian.Uint64(raw[8:16])
	var out strings.Builder
	for i := 25; i >= 0; i-- {
		shift := uint(i * 5)
		var digit uint64
		switch {
		case shift >= 64:
			digit = hi >> (shift - 64)
		case shift > 59:
			digit = lo>>shift | hi<<(64-shift)
		default:
			digit = lo >> shift
		}
		out.WriteByte(crockford[digit&31])
	}
	return out.String()
}
