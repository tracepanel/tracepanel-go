package tracepanel

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
)

// Tracepanel records in development only, as every SDK does: when TRACEPANEL_ENABLED=1, when the app's Config says
// Enabled, or when the app was started by `go run` (or by `tracepanel run`, which sets TRACEPANEL_ENABLED=1). A
// binary built with `go build` and run as it is (in production, in a container) records nothing: Setup, WrapHandler,
// Driver and the rest hand back what they were given, and no agent is downloaded or started.
// TRACEPANEL_ENABLED=0 always wins.

// wanted tells whether Tracepanel records, with the app's Config.
func wanted(config Config) bool {
	value := os.Getenv("TRACEPANEL_ENABLED")
	if off(value) {
		return false
	}
	return on(value) || config.Enabled || devRun()
}

// records tells whether what the app wraps is to be recorded: Tracepanel is on, or will be when set up.
func records() bool {
	return active.Load() != nil || wanted(Config{})
}

// devRun tells whether this process is a binary `go run` built: once, it doesn't change.
var devRun = sync.OnceValue(func() bool {
	executable, err := os.Executable()
	if err != nil {
		return false
	}
	return goRunBinary(executable, os.Getenv("GOCACHE"))
})

var (
	// Where `go run` links a binary: its work directory (under os.TempDir(), or GOTMPDIR), $WORK/b001/exe/<name>.
	goRunWork = regexp.MustCompile(`/go-build[0-9]+/b[0-9]+/exe/[^/]+$`)
	// Where `go run` keeps the binary it built since Go 1.24, in the build cache: go-build/<2 hex>/<64 hex>-d/<name>.
	goRunCached = regexp.MustCompile(`(^|/)[0-9a-f]{2}/[0-9a-f]{64}-d/[^/]+$`)
)

// goRunBinary tells whether an executable is one `go run` built: in its work directory, or in the build cache (the
// default one, go-build in the user's cache directory, or GOCACHE's). A binary `go build` wrote, or `go install`, is
// wherever the app put it, and isn't.
func goRunBinary(executable, gocache string) bool {
	// Windows' paths too, wherever this runs.
	path := strings.ReplaceAll(executable, `\`, "/")
	if goRunWork.MatchString(path) {
		return true
	}
	if !goRunCached.MatchString(path) {
		return false
	}
	if strings.Contains(path, "/go-build/") {
		return true
	}
	cache := strings.ReplaceAll(filepath.Clean(gocache), `\`, "/")
	return gocache != "" && gocache != "off" && strings.HasPrefix(path, strings.TrimSuffix(cache, "/")+"/")
}
