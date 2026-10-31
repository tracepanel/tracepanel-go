package tracepanel

import (
	"runtime/debug"
	"testing"
)

func TestTheAppTabHasGoTheRouterTheAppAndItsModules(t *testing.T) {
	info := &debug.BuildInfo{
		Main: debug.Module{Path: "example.com/shop", Version: "v1.2.0"},
		Deps: []*debug.Module{
			{Path: "modernc.org/sqlite", Version: "v1.40.0"},
			{Path: "github.com/gin-gonic/gin", Version: "v1.11.0"},
			{Path: "github.com/tracepanel/tracepanel-go", Version: "v0.9.0", Replace: &debug.Module{Path: "../tracepanel-go"}},
		},
		Settings: []debug.BuildSetting{
			{Key: "GOOS", Value: "linux"}, {Key: "-ldflags", Value: "-X main.apiKey=secret"}, {Key: "vcs.revision", Value: "abc123"},
		},
	}

	versions := appVersions(info)
	settings := appSettings(Config{Service: "shop", StoreDir: ".tracepanel", MaxTraces: 2000}, info)

	if !equalJSON(versions, []map[string]string{
		{"name": "Go", "version": goVersion(), "kind": "runtime"},
		{"name": "Gin", "version": "v1.11.0", "kind": "framework"},
		{"name": "example.com/shop", "version": "v1.2.0", "kind": "package"},
		{"name": "github.com/gin-gonic/gin", "version": "v1.11.0", "kind": "package"},
		{"name": "github.com/tracepanel/tracepanel-go", "version": "(devel)", "kind": "package"},
		{"name": "modernc.org/sqlite", "version": "v1.40.0", "kind": "package"},
	}) {
		t.Fatalf("versions %v", versions)
	}
	// -ldflags may carry a secret: never sent.
	if _, sent := settings["-ldflags"]; sent || settings["GOOS"] != "linux" || settings["vcs.revision"] != "abc123" ||
		settings["TRACEPANEL_SERVICE"] != "shop" || settings["TRACEPANEL_MAX_TRACES"] != "2000" {
		t.Fatalf("settings %v", settings)
	}
}

func TestThresholdsComeFromTheEnvironmentOnePerDatabaseToo(t *testing.T) {
	for name := range thresholdNames {
		t.Setenv(name, "")
	}
	t.Setenv("TRACEPANEL_N_PLUS_ONE_THRESHOLD", "5")
	t.Setenv("TRACEPANEL_N_PLUS_ONE_MIN_MS", "0")
	t.Setenv("TRACEPANEL_SLOW_SQL_MS", "100, Snowflake=3000,bigquery=5000")
	t.Setenv("TRACEPANEL_LONG_TRANSACTION_MS", "snowflake=20000")
	t.Setenv("TRACEPANEL_QUEUE_WAIT_MS", "fast")
	t.Setenv("TRACEPANEL_SLOW_HTTP_MS", "redis=3")

	got := thresholds()

	if !equalJSON(got, map[string]any{
		"n_plus_one": 5, "n_plus_one_min_ms": 0, "slow_sql_ms": 100,
		"slow_sql_ms_by_db":         map[string]int{"snowflake": 3000, "bigquery": 5000},
		"long_transaction_ms_by_db": map[string]int{"snowflake": 20000},
	}) {
		t.Fatalf("thresholds %v", got)
	}
}

func TestMaxTracesComesFromTheEnvironment(t *testing.T) {
	t.Setenv("TRACEPANEL_MAX_TRACES", "500")

	if got := withDefaults(Config{Service: "shop"}).MaxTraces; got != 500 {
		t.Fatalf("max traces %d", got)
	}
}
