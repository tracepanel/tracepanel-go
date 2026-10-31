// Package zerologtrace adds zerolog's records to Tracepanel's traces, as slog's are: each record logged with a
// request's context (Ctx(ctx)) joins its trace, with its level, its message, its fields (secret-looking ones masked)
// and the line that logged it (with Caller()).
//
//	logger := zerolog.New(zerologtrace.Writer(os.Stdout)).Hook(zerologtrace.Hook{})
//	logger.Info().Ctx(ctx).Int("order", id).Msg("order placed")
//
// The hook sees the record's context, the writer its fields: both are needed. The writer writes each record on as
// the app's output had it.
package zerologtrace

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/rs/zerolog"

	"github.com/tracepanel/tracepanel-go/internal/hook"
)

// The field the hook marks a traced record with, for the writer, which takes it out again.
const refKey = "tracepanel.ref"

// Hook marks the records logged with a traced context, for Writer to add to their trace.
type Hook struct{}

var _ zerolog.Hook = Hook{}

func (Hook) Run(event *zerolog.Event, level zerolog.Level, message string) {
	if !hook.Enabled() {
		return
	}
	ctx := event.GetCtx()
	if ctx == nil || !hook.Traced(ctx) {
		return
	}
	event.Str(refKey, records.add(&marked{ctx: ctx, level: level, message: message, time: time.Now()}))
}

// marked is a record the hook marked: its context, level and message, until the writer gets its fields.
type marked struct {
	ctx     context.Context
	level   zerolog.Level
	message string
	time    time.Time
}

// pending are the marked records on their way to the writer, by their mark.
type pending struct {
	mu   sync.Mutex
	next uint64
	byID map[string]*marked
}

// The most marked records kept for the writer: a record a filtering writer drops never reaches it.
const maxPending = 10000

var records = &pending{byID: map[string]*marked{}}

func (p *pending) add(record *marked) string {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.byID) >= maxPending {
		p.byID = map[string]*marked{}
	}
	p.next++
	id := strconv.FormatUint(p.next, 10)
	p.byID[id] = record
	return id
}

func (p *pending) take(id string) *marked {
	p.mu.Lock()
	defer p.mu.Unlock()
	record := p.byID[id]
	delete(p.byID, id)
	return record
}

// Writer writes each record to `next`, and adds the ones the hook marked to their trace.
func Writer(next io.Writer) zerolog.LevelWriter {
	return &writer{next: next}
}

type writer struct{ next io.Writer }

func (w *writer) Write(line []byte) (int, error) {
	return w.WriteLevel(zerolog.NoLevel, line)
}

func (w *writer) WriteLevel(level zerolog.Level, line []byte) (int, error) {
	written := len(line)
	if bytes.Contains(line, []byte(`"`+refKey+`":"`)) {
		line = w.record(line)
	}
	var err error
	if leveled, ok := w.next.(zerolog.LevelWriter); ok {
		_, err = leveled.WriteLevel(level, line)
	} else {
		_, err = w.next.Write(line)
	}
	return written, err
}

// record adds a marked record to its trace, and is its line without the mark.
func (w *writer) record(line []byte) []byte {
	decoder := json.NewDecoder(bytes.NewReader(line))
	decoder.UseNumber()
	var fields map[string]any
	if err := decoder.Decode(&fields); err != nil {
		return line
	}
	id, _ := fields[refKey].(string)
	record := records.take(id)
	line = unmark(line, id)
	if record == nil {
		return line
	}
	out := hook.Record{Time: record.time, Level: levelName(record.level), Logger: "zerolog", Message: record.message}
	if caller, ok := fields[zerolog.CallerFieldName].(string); ok {
		if at := strings.LastIndexByte(caller, ':'); at > 0 {
			if number, err := strconv.Atoi(caller[at+1:]); err == nil {
				out.File, out.Line = caller[:at], number
			}
		}
	}
	for _, key := range []string{refKey, zerolog.LevelFieldName, zerolog.MessageFieldName, zerolog.TimestampFieldName, zerolog.CallerFieldName} {
		delete(fields, key)
	}
	out.Fields = fields
	hook.Log(record.ctx, out)
	return line
}

// unmark is a line without the hook's mark.
func unmark(line []byte, id string) []byte {
	field := `"` + refKey + `":"` + id + `"`
	for _, form := range []string{"," + field, field + ",", field} {
		if at := bytes.Index(line, []byte(form)); at >= 0 {
			return append(append([]byte{}, line[:at]...), line[at+len(form):]...)
		}
	}
	return line
}

func levelName(level zerolog.Level) string {
	switch level {
	case zerolog.TraceLevel, zerolog.DebugLevel:
		return "debug"
	case zerolog.WarnLevel:
		return "warning"
	case zerolog.ErrorLevel, zerolog.FatalLevel, zerolog.PanicLevel:
		return "error"
	default:
		return "info"
	}
}
