// Package zaptrace adds zap's records to Tracepanel's traces, as slog's are: each record logged with a request's
// context joins its trace, with its level, its message, its fields (secret-looking ones masked) and the line that
// logged it (with zap.AddCaller).
//
//	logger = logger.WithOptions(zap.WrapCore(zaptrace.WrapCore))
//	logger.Info("order placed", zaptrace.Context(ctx), zap.Int("order", id))
//
// zap's loggers carry no context: zaptrace.Context(ctx) is a field that names it (it writes nothing), on a record or
// on a logger (logger.With(zaptrace.Context(ctx))).
package zaptrace

import (
	"context"
	"strings"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"

	"github.com/tracepanel/tracepanel-go/internal/hook"
)

// carrier is the context a Context field carries.
type carrier struct{ ctx context.Context }

// Context is a field naming the context a record is logged with: the record joins its trace. Encoders skip it.
func Context(ctx context.Context) zap.Field {
	return zap.Field{Type: zapcore.SkipType, Interface: carrier{ctx}}
}

// WrapCore is `next` with its records added to the trace of their context too, for zap.WrapCore or zap.New.
func WrapCore(next zapcore.Core) zapcore.Core {
	if _, ok := next.(*core); ok {
		return next
	}
	return &core{next: next}
}

type core struct {
	next zapcore.Core
	// The context a Context field on the logger named, and the logger's other fields.
	ctx    context.Context
	fields []zapcore.Field
}

func (c *core) Enabled(level zapcore.Level) bool { return c.next.Enabled(level) }
func (c *core) Sync() error                      { return c.next.Sync() }

func (c *core) With(fields []zapcore.Field) zapcore.Core {
	ctx, own := split(fields)
	if ctx == nil {
		ctx = c.ctx
	}
	return &core{next: c.next.With(fields), ctx: ctx, fields: append(append([]zapcore.Field{}, c.fields...), own...)}
}

// Check lets `next` decide for itself (its level, its sampling), and the record goes to the trace too.
func (c *core) Check(entry zapcore.Entry, checked *zapcore.CheckedEntry) *zapcore.CheckedEntry {
	checked = c.next.Check(entry, checked)
	if hook.Enabled() && c.next.Enabled(entry.Level) {
		checked = checked.AddCore(entry, &recorder{core: c})
	}
	return checked
}

func (c *core) Write(entry zapcore.Entry, fields []zapcore.Field) error {
	return c.next.Write(entry, fields)
}

// recorder adds a record to the trace of its context: the second core of a checked record.
type recorder struct{ *core }

func (r *recorder) Write(entry zapcore.Entry, fields []zapcore.Field) error {
	ctx, own := split(fields)
	if ctx == nil {
		ctx = r.ctx
	}
	if ctx == nil || !hook.Traced(ctx) {
		return nil
	}
	encoder := zapcore.NewMapObjectEncoder()
	for _, field := range r.fields {
		field.AddTo(encoder)
	}
	for _, field := range own {
		field.AddTo(encoder)
	}
	record := hook.Record{
		Time: entry.Time, Level: level(entry.Level), Logger: entry.LoggerName, Message: entry.Message,
		Fields: encoder.Fields,
	}
	if entry.Caller.Defined {
		record.File, record.Line, record.Function = entry.Caller.File, entry.Caller.Line, entry.Caller.Function
	}
	if record.Logger == "" {
		record.Logger = module(record.Function, "zap")
	}
	hook.Log(ctx, record)
	return nil
}

func (r *recorder) Sync() error { return nil }

// split takes the context a Context field names out of fields.
func split(fields []zapcore.Field) (context.Context, []zapcore.Field) {
	var ctx context.Context
	own := make([]zapcore.Field, 0, len(fields))
	for _, field := range fields {
		if named, ok := field.Interface.(carrier); ok && field.Type == zapcore.SkipType {
			ctx = named.ctx
			continue
		}
		own = append(own, field)
	}
	return ctx, own
}

func level(level zapcore.Level) string {
	switch {
	case level >= zapcore.ErrorLevel:
		return "error"
	case level == zapcore.WarnLevel:
		return "warning"
	case level == zapcore.InfoLevel:
		return "info"
	default:
		return "debug"
	}
}

// module is the package of a function, as slog's records name their logger: "example.com/shop/orders".
func module(function, fallback string) string {
	slash := strings.LastIndex(function, "/")
	if dot := strings.Index(function[slash+1:], "."); dot >= 0 {
		return function[:slash+1+dot]
	}
	return fallback
}
