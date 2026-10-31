package tracepanel

import (
	"os"
	"runtime"
	"runtime/debug"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// What the app is, for the console's App tab, said once per connection in the hello (`app`): the settings it was
// built and runs with, those that hold no secret, and the versions it runs on: Go, the router it is built with, its
// own module and the modules it depends on, as the build recorded them.

// The routers an app may be built with: the framework line of the App tab.
var frameworks = map[string]string{
	"github.com/gin-gonic/gin":            "Gin",
	"github.com/labstack/echo/v4":         "Echo",
	"github.com/labstack/echo/v5":         "Echo",
	"github.com/go-chi/chi/v5":            "chi",
	"github.com/gofiber/fiber/v2":         "Fiber",
	"github.com/gofiber/fiber/v3":         "Fiber",
	"github.com/gorilla/mux":              "gorilla/mux",
	"github.com/julienschmidt/httprouter": "httprouter",
}

// The build settings worth a line: none of them holds a secret, as -ldflags (-X main.apiKey=...) may.
var buildSettings = map[string]bool{
	"GOOS": true, "GOARCH": true, "CGO_ENABLED": true, "-race": true, "-tags": true, "-trimpath": true,
	"-buildmode": true, "-compiler": true, "GOAMD64": true, "GOARM": true, "GOARM64": true, "GO386": true,
	"GOEXPERIMENT": true, "vcs": true, "vcs.revision": true, "vcs.time": true, "vcs.modified": true,
}

// maxModules is the most dependencies the App tab lists.
const maxModules = 300

type versionDoc struct {
	Name    string `json:"name"`
	Version string `json:"version"`
	Kind    string `json:"kind"`
}

var about struct {
	once sync.Once
	info *debug.BuildInfo
}

func buildInfo() *debug.BuildInfo {
	about.once.Do(func() {
		if info, ok := debug.ReadBuildInfo(); ok {
			about.info = info
		}
	})
	return about.info
}

// aboutApp is the hello's `app`: {"settings": {...}, "versions": [...]}.
func aboutApp(config Config) map[string]any {
	return map[string]any{"settings": appSettings(config, buildInfo()), "versions": appVersions(buildInfo())}
}

// appSettings are the build's settings that hold no secret, how the process runs, and Tracepanel's own.
func appSettings(config Config, info *debug.BuildInfo) map[string]string {
	settings := map[string]string{
		"GOMAXPROCS":            strconv.Itoa(runtime.GOMAXPROCS(0)),
		"NumCPU":                strconv.Itoa(runtime.NumCPU()),
		"TRACEPANEL_SERVICE":    config.Service,
		"TRACEPANEL_DIR":        config.StoreDir,
		"TRACEPANEL_MAX_TRACES": strconv.Itoa(config.MaxTraces),
	}
	if info != nil {
		for _, setting := range info.Settings {
			if buildSettings[setting.Key] {
				settings[setting.Key] = setting.Value
			}
		}
	}
	for _, name := range thresholdVariables() {
		if value := os.Getenv(name); value != "" {
			settings[name] = value
		}
	}
	return settings
}

// appVersions are Go's, the router's, then the app's module and the modules it depends on, by path.
func appVersions(info *debug.BuildInfo) []versionDoc {
	versions := []versionDoc{{Name: "Go", Version: goVersion(), Kind: "runtime"}}
	if info == nil {
		return versions
	}
	var packages []versionDoc
	for _, module := range info.Deps {
		version := module.Version
		if module.Replace != nil {
			// What the build used: the replacement's version, or its directory's "(devel)".
			version = module.Replace.Version
			if version == "" {
				version = "(devel)"
			}
		}
		if name, ok := frameworks[module.Path]; ok {
			versions = append(versions, versionDoc{Name: name, Version: version, Kind: "framework"})
		}
		packages = append(packages, versionDoc{Name: module.Path, Version: version, Kind: "package"})
	}
	sort.Slice(packages, func(i, j int) bool { return strings.ToLower(packages[i].Name) < strings.ToLower(packages[j].Name) })
	if info.Main.Path != "" {
		versions = append(versions, versionDoc{Name: info.Main.Path, Version: info.Main.Version, Kind: "package"})
	}
	return append(versions, packages[:min(len(packages), maxModules)]...)
}
