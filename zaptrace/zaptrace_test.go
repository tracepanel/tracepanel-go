package zaptrace

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"

	tracepanel "github.com/tracepanel/tracepanel-go"
	"github.com/tracepanel/tracepanel-go/internal/tracetest"
)

func logger(out *bytes.Buffer) *zap.Logger {
	encoder := zapcore.NewJSONEncoder(zapcore.EncoderConfig{MessageKey: "msg", LevelKey: "level", EncodeLevel: zapcore.LowercaseLevelEncoder})
	core := zapcore.NewCore(encoder, zapcore.AddSync(out), zapcore.InfoLevel)
	return zap.New(core, zap.AddCaller()).WithOptions(zap.WrapCore(WrapCore))
}

func TestRecordsJoinTheTrace(t *testing.T) {
	recorder := tracetest.Record(t)
	var out bytes.Buffer
	log := logger(&out)

	_ = tracepanel.Command(context.Background(), "job", func(ctx context.Context) error {
		log.Info("order placed", Context(ctx), zap.Int("order", 7), zap.String("password", "hunter2"),
			zap.Dict("user", zap.String("name", "ann")), zap.Error(errors.New("card declined")))
		log.With(Context(ctx)).Named("billing").Warn("retrying")
		log.Debug("not enabled", Context(ctx))
		return nil
	})
	log.Info("outside a trace")

	trace := recorder.Wait(t, 1)[0]
	logs := tracetest.List(trace, "logs")
	if len(logs) != 2 {
		t.Fatalf("logs: %v", logs)
	}
	first := logs[0]
	attrs := tracetest.Attrs(first)
	if first["level"] != "info" || first["message"] != "order placed" || first["logger"] != "github.com/tracepanel/tracepanel-go/zaptrace" {
		t.Fatalf("record: %v", first)
	}
	if !strings.HasSuffix(attrs["code.filepath"].(string), "zaptrace_test.go") || attrs["code.function"] != "TestRecordsJoinTheTrace.func1" {
		t.Fatalf("caller: %v", attrs)
	}
	for key, want := range map[string]any{
		"extra.order": float64(7), "extra.password": "********", "extra.user.name": "ann", "extra.error": "card declined",
	} {
		if attrs[key] != want {
			t.Errorf("%s = %v, want %v", key, attrs[key], want)
		}
	}
	if logs[1]["level"] != "warning" || logs[1]["logger"] != "billing" || logs[1]["message"] != "retrying" {
		t.Fatalf("second record: %v", logs[1])
	}
	// The app's own output keeps every record it enables, without the context.
	if lines := strings.Count(out.String(), "\n"); lines != 3 || strings.Contains(out.String(), "carrier") {
		t.Fatalf("output: %s", out.String())
	}
}
