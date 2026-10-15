package tracepanel

import (
	"context"
	"fmt"
	"log/slog"
	"regexp"
	"runtime"
	"strings"
)

// LogHandler adds slog records to the trace of the context they were logged with (slog.InfoContext(ctx, ...)),
// then passes them on to `next`. Setup puts it in front of the standard logger's handler; a logger of the app's own
// takes it as its handler:
//
//	logger := slog.New(tracepanel.LogHandler(slog.NewJSONHandler(os.Stdout, nil)))
//
// A record keeps its level, its message, the line that logged it, and each attribute as an extra. field.
func LogHandler(next slog.Handler) slog.Handler {
	if _, ok := next.(*logHandler); ok {
		return next
	}
	return &logHandler{next: next}
}

type logHandler struct {
	next   slog.Handler
	attrs  []slog.Attr
	groups []string
}

func (h *logHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.next.Enabled(ctx, level)
}

func (h *logHandler) Handle(ctx context.Context, record slog.Record) error {
	if s := scopeOf(ctx); s != nil {
		attrs := Attrs{}
		prefix := "extra."
		if len(h.groups) > 0 {
			prefix += strings.Join(h.groups, ".") + "."
		}
		for _, attr := range h.attrs {
			flatten(attrs, "extra.", attr)
		}
		record.Attrs(func(attr slog.Attr) bool {
			flatten(attrs, prefix, attr)
			return true
		})
		logger := "slog"
		if record.PC != 0 {
			frame, _ := runtime.CallersFrames([]uintptr{record.PC}).Next()
			attrs["code.filepath"] = relative(frame.File)
			attrs["code.lineno"] = frame.Line
			attrs["code.function"] = shortFunction(frame.Function)
			if module, _ := splitFunction(frame.Function); module != "" {
				logger = module
			}
		}
		s.rec.log(logDoc{
			SpanID: ptr(s.span), Time: micros(record.Time), Level: levelName(record.Level), Logger: logger,
			Message: record.Message, Attrs: attrs,
		})
	}
	return h.next.Handle(ctx, record)
}

// secret is a field name that looks like it holds one: its value is kept as mask.
var secret = regexp.MustCompile(`(?i)pass(word|wd)?|secret|token|api_?key|auth|cookie|session|csrf|signature|private`)

const mask = "********"

// flatten names a group's attributes by their path: extra.user.id. A secret-looking one keeps the mask.
func flatten(attrs Attrs, prefix string, attr slog.Attr) {
	value := attr.Value.Resolve()
	if value.Kind() == slog.KindGroup {
		for _, member := range value.Group() {
			flatten(attrs, prefix+attr.Key+".", member)
		}
		return
	}
	if attr.Key == "" {
		return
	}
	if secret.MatchString(attr.Key) {
		attrs[prefix+attr.Key] = mask
		return
	}
	attrs[prefix+attr.Key] = logValue(value)
}

func logValue(value slog.Value) any {
	switch value.Kind() {
	case slog.KindBool:
		return value.Bool()
	case slog.KindInt64:
		return value.Int64()
	case slog.KindUint64:
		return value.Uint64()
	case slog.KindFloat64:
		return value.Float64()
	case slog.KindString:
		return value.String()
	default:
		if err, ok := value.Any().(error); ok {
			return err.Error()
		}
		return fmt.Sprint(value.Any())
	}
}

func (h *logHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	grouped := attrs
	if len(h.groups) > 0 {
		// Attributes added inside a group are that group's.
		grouped = []slog.Attr{slog.Attr{Key: strings.Join(h.groups, "."), Value: slog.GroupValue(attrs...)}}
	}
	return &logHandler{next: h.next.WithAttrs(attrs), attrs: append(append([]slog.Attr{}, h.attrs...), grouped...), groups: h.groups}
}

func (h *logHandler) WithGroup(name string) slog.Handler {
	if name == "" {
		return h
	}
	return &logHandler{next: h.next.WithGroup(name), attrs: h.attrs, groups: append(append([]string{}, h.groups...), name)}
}

func levelName(level slog.Level) string {
	switch {
	case level >= slog.LevelError:
		return "error"
	case level >= slog.LevelWarn:
		return "warning"
	case level >= slog.LevelInfo:
		return "info"
	default:
		return "debug"
	}
}

func shortType(value any) string { return fmt.Sprintf("%T", value) }
