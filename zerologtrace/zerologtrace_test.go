package zerologtrace

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/rs/zerolog"

	tracepanel "github.com/tracepanel/tracepanel-go"
	"github.com/tracepanel/tracepanel-go/internal/tracetest"
)

func TestRecordsJoinTheTrace(t *testing.T) {
	recorder := tracetest.Record(t)
	var out bytes.Buffer
	logger := zerolog.New(Writer(&out)).Hook(Hook{}).Level(zerolog.InfoLevel)

	_ = tracepanel.Command(context.Background(), "job", func(ctx context.Context) error {
		logger.Info().Ctx(ctx).Caller().Int("order", 7).Str("api_key", "k-123").
			Dict("user", zerolog.Dict().Str("name", "ann")).Err(errors.New("card declined")).Msg("order placed")
		child := logger.With().Ctx(ctx).Logger()
		child.Warn().Msg("retrying")
		logger.Debug().Ctx(ctx).Msg("not enabled")
		return nil
	})
	logger.Info().Msg("outside a trace")

	trace := recorder.Wait(t, 1)[0]
	logs := tracetest.List(trace, "logs")
	if len(logs) != 2 {
		t.Fatalf("logs: %v", logs)
	}
	first := logs[0]
	attrs := tracetest.Attrs(first)
	if first["level"] != "info" || first["message"] != "order placed" || first["logger"] != "zerolog" {
		t.Fatalf("record: %v", first)
	}
	if !strings.HasSuffix(attrs["code.filepath"].(string), "zerologtrace_test.go") {
		t.Fatalf("caller: %v", attrs)
	}
	for key, want := range map[string]any{
		"extra.order": float64(7), "extra.api_key": "********", "extra.user.name": "ann", "extra.error": "card declined",
	} {
		if attrs[key] != want {
			t.Errorf("%s = %v, want %v", key, attrs[key], want)
		}
	}
	if logs[1]["level"] != "warning" || logs[1]["message"] != "retrying" {
		t.Fatalf("second record: %v", logs[1])
	}
	// The app's own output keeps its records as they were, without the mark.
	if lines := strings.Count(out.String(), "\n"); lines != 3 || strings.Contains(out.String(), refKey) {
		t.Fatalf("output: %s", out.String())
	}
	if !strings.Contains(out.String(), `"level":"warn","message":"retrying"}`) {
		t.Fatalf("output: %s", out.String())
	}
}

func TestUnmark(t *testing.T) {
	for line, want := range map[string]string{
		`{"level":"info","tracepanel.ref":"3","message":"x"}`: `{"level":"info","message":"x"}`,
		`{"tracepanel.ref":"3","message":"x"}`:                `{"message":"x"}`,
		`{"tracepanel.ref":"3"}`:                              `{}`,
	} {
		if got := string(unmark([]byte(line), "3")); got != want {
			t.Errorf("unmark(%s) = %s, want %s", line, got, want)
		}
	}
}
