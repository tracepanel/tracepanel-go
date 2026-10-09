package tracepanel

import (
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"runtime/debug"

	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

// Wrap is tracepanel in one line, around the app's handler (its ServeMux):
//
//	http.ListenAndServe(addr, tracepanel.Wrap(mux))
//
// It joins the app's OpenTelemetry TracerProvider (or sets one when the app has none), adds the default slog
// handler's records to the traces, and returns the handler with the widget's middleware. Handlers on a ServeMux
// become `view` spans named after their function. A Config is optional: what it leaves empty comes from TRACEPANEL_*
// variables, then defaults (the service is the executable's name).
func Wrap(handler http.Handler, config ...Config) http.Handler {
	var settings Config
	if len(config) > 0 {
		settings = config[0]
	}
	t := New(withDefaults(settings))
	t.install()
	return t.Middleware(handler)
}

func withDefaults(config Config) Config {
	if config.Service == "" {
		config.Service = filepath.Base(os.Args[0])
	}
	if config.ConsoleURL == "" {
		config.ConsoleURL = envOr("TRACEPANEL_CONSOLE_URL", "http://127.0.0.1:8765/")
	}
	if config.StoreDir == "" {
		config.StoreDir = envOr("TRACEPANEL_DIR", ".tracepanel")
	}
	if config.Module == "" {
		if info, ok := debug.ReadBuildInfo(); ok {
			config.Module = info.Main.Path
		}
	}
	return config
}

func envOr(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}

// install makes the app's OTel spans and slog records reach tracepanel.
func (t *Tracepanel) install() {
	if provider, ok := otel.GetTracerProvider().(*sdktrace.TracerProvider); ok {
		// The app's own provider (with its exporters): tracepanel is one more processor.
		provider.RegisterSpanProcessor(t)
	} else {
		otel.SetTracerProvider(sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(t)))
	}
	next := slog.Default().Handler()
	if fmt.Sprintf("%T", next) == "*slog.defaultHandler" {
		// slog's built-in handler writes through the log package, which SetDefault sends back to slog: wrapping it
		// would deadlock. The app hasn't chosen one, so the same text on stderr.
		next = slog.NewTextHandler(os.Stderr, nil)
	}
	slog.SetDefault(slog.New(t.LogHandler(next)))
}
