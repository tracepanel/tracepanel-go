package tracepanel

import (
	"context"
	"database/sql/driver"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
)

// The tests record as an app under `go run` does: a test binary is no development run of its own. Started with
// TRACEPANEL_TEST_FAKE_AGENT=1, the test binary is an agent instead (start_test.go).
func TestMain(m *testing.M) {
	if os.Getenv("TRACEPANEL_TEST_FAKE_AGENT") == "1" {
		fakeAgent(os.Args[1:])
		return
	}
	_ = os.Setenv("TRACEPANEL_ENABLED", "1")
	os.Exit(m.Run())
}

// asBuilt makes the test a binary `go build` wrote (or `go run` built, with dev), TRACEPANEL_ENABLED as given.
func asBuilt(t *testing.T, dev bool, enabled string) {
	t.Helper()
	t.Setenv("TRACEPANEL_ENABLED", enabled)
	before := devRun
	devRun = func() bool { return dev }
	t.Cleanup(func() { devRun = before })
	active.Store(nil)
}

type nullConnector struct{}

func (nullConnector) Connect(context.Context) (driver.Conn, error) { return nil, driver.ErrBadConn }
func (nullConnector) Driver() driver.Driver                        { return nil }

func TestABuiltBinaryRecordsNothing(t *testing.T) {
	asBuilt(t, false, "")
	mux := http.NewServeMux()
	connector := nullConnector{}
	logs := slog.NewTextHandler(os.Stderr, nil)

	handler := WrapHandler(mux)
	Setup()

	if handler != http.Handler(mux) || active.Load() != nil {
		t.Fatal("WrapHandler recorded")
	}
	if Driver("pgx") != "pgx" || WrapConnector(connector) != driver.Connector(connector) {
		t.Fatal("the database is wrapped")
	}
	if WrapTransport(nil) != http.DefaultTransport || WrapLogHandler(logs) != slog.Handler(logs) {
		t.Fatal("calls or logs are wrapped")
	}
}

func TestWhatTurnsTracepanelOn(t *testing.T) {
	cases := []struct {
		name    string
		dev     bool
		enabled string
		config  Config
		want    bool
	}{
		{"a built binary", false, "", Config{}, false},
		{"go run", true, "", Config{}, true},
		{"TRACEPANEL_ENABLED=1", false, "1", Config{}, true},
		{"TRACEPANEL_ENABLED=yes", false, "yes", Config{}, true},
		{"Config.Enabled", false, "", Config{Enabled: true}, true},
		{"TRACEPANEL_ENABLED=0 under go run", true, "0", Config{}, false},
		{"TRACEPANEL_ENABLED=false with Config.Enabled", false, "false", Config{Enabled: true}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			asBuilt(t, c.dev, c.enabled)

			if got := wanted(c.config); got != c.want {
				t.Fatalf("on: %v", got)
			}
		})
	}
}

func TestABinaryGoRunBuiltIsADevelopmentRun(t *testing.T) {
	cases := map[string]bool{
		// go run's work directory, under the temporary directory or GOTMPDIR.
		"/var/folders/2y/xfx/T/go-build1003866124/b001/exe/shop":       true,
		"/tmp/go-build42/b001/exe/shop":                                true,
		`C:\Users\ann\AppData\Local\Temp\go-build42\b001\exe\shop.exe`: true,
		// The build cache, where go run keeps what it built since Go 1.24: the default one.
		"/Users/ann/Library/Caches/go-build/c6/c6a395c9b31d05c60f3c81f366338283e62a2461e45d548614378b67d8663b66-d/shop": true,
		"/home/ann/.cache/go-build/c6/c6a395c9b31d05c60f3c81f366338283e62a2461e45d548614378b67d8663b66-d/shop":          true,
		// GOCACHE's.
		"/srv/cache/c6/c6a395c9b31d05c60f3c81f366338283e62a2461e45d548614378b67d8663b66-d/shop": true,
		// What go build or go install wrote, a container's binary, a test binary.
		"/usr/local/bin/shop":             false,
		"/app/shop":                       false,
		"/home/ann/go/bin/shop":           false,
		"/tmp/go-build42/b001/shop.test":  false,
		"/opt/c6/c6a395c9b31d05c6-d/shop": false,
		"/other/c6/c6a395c9b31d05c60f3c81f366338283e62a2461e45d548614378b67d8663b66-d/shop": false,
	}
	got := map[string]bool{}
	for path := range cases {
		got[path] = goRunBinary(path, "/srv/cache")
	}

	for path, want := range cases {
		if got[path] != want {
			t.Errorf("%s: %v, want %v", path, got[path], want)
		}
	}
}

// Windows' paths, as cmd/go makes them there: the work directory os.MkdirTemp(GOTMPDIR or %TEMP%, "go-build"), the
// build cache os.UserCacheDir()\go-build (%LocalAppData%) or GOCACHE, and the binary's name with .exe.
func TestABinaryGoRunBuiltOnWindowsIsADevelopmentRun(t *testing.T) {
	const hash = `c6\c6a395c9b31d05c60f3c81f366338283e62a2461e45d548614378b67d8663b66-d`
	cases := map[string]bool{
		`C:\Users\ann\AppData\Local\Temp\go-build1003866124\b001\exe\shop.exe`: true,
		// GOTMPDIR on another drive.
		`D:\tmp\go-build42\b001\exe\shop.exe`: true,
		// The default build cache.
		`C:\Users\ann\AppData\Local\go-build\` + hash + `\shop.exe`: true,
		// GOCACHE's, its drive and folders in another case.
		`d:\cache\GO\` + hash + `\shop.exe`: true,
		// What go build or go install wrote, a test binary, another cache.
		`C:\Users\ann\src\shop\shop.exe`:                                false,
		`C:\Users\ann\go\bin\shop.exe`:                                  false,
		`C:\Users\ann\AppData\Local\Temp\go-build42\b001\shop.test.exe`: false,
		`E:\other\` + hash + `\shop.exe`:                                false,
	}
	got := map[string]bool{}
	for path := range cases {
		got[path] = goRunBinary(path, `D:\Cache\Go\`)
	}

	for path, want := range cases {
		if got[path] != want {
			t.Errorf("%s: %v, want %v", path, got[path], want)
		}
	}
	// No GOCACHE (or off): only the default cache's.
	if goRunBinary(`d:\cache\go\`+hash+`\shop.exe`, "") || goRunBinary(`d:\cache\go\`+hash+`\shop.exe`, "off") {
		t.Error("a cache that isn't GOCACHE's")
	}
}

func TestGoRunRecordsAndGoBuildDoesnt(t *testing.T) {
	if testing.Short() {
		t.Skip("builds an app")
	}
	core, _ := filepath.Abs(".")
	app := t.TempDir()
	files := map[string]string{
		"go.mod": "module example.com/shop\n\ngo 1.26\n\nrequire github.com/tracepanel/tracepanel-go v0.0.0\n\n" +
			"replace github.com/tracepanel/tracepanel-go => " + filepath.ToSlash(core) + "\n",
		"main.go": "package main\n\nimport (\n\t\"fmt\"\n\n\ttracepanel \"github.com/tracepanel/tracepanel-go\"\n)\n\n" +
			"func main() { fmt.Print(tracepanel.Driver(\"sqlite\")) }\n",
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(app, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	env := []string{"TRACEPANEL_ENABLED="}
	run := func(name string, args ...string) string {
		t.Helper()
		cmd := exec.Command(name, args...)
		cmd.Dir, cmd.Env = app, append(os.Environ(), env...)
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("%s %v: %v", name, args, err)
		}
		return string(out)
	}

	// The second go run runs the binary the build cache kept.
	first, second := run("go", "run", "."), run("go", "run", ".")
	// go build -o takes the name as it is: on Windows, a program is run by its .exe.
	shop := "shop"
	if runtime.GOOS == "windows" {
		shop += ".exe"
	}
	run("go", "build", "-o", shop, ".")
	built := run(filepath.Join(app, shop))
	env = []string{"TRACEPANEL_ENABLED=1"}
	enabled := run(filepath.Join(app, shop))
	env = []string{"TRACEPANEL_ENABLED=0"}
	off := run("go", "run", ".")

	if first != "tracepanel:sqlite" || second != "tracepanel:sqlite" || built != "sqlite" || enabled != "tracepanel:sqlite" || off != "sqlite" {
		t.Fatalf("go run %q, again %q, built %q, built with TRACEPANEL_ENABLED=1 %q, go run with 0 %q", first, second, built, enabled, off)
	}
}
