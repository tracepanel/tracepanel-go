package main

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// A stand-in agent that writes its arguments where the test reads them.
func fakeAgent(t *testing.T) (binary, argsFile string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("a shell script stands in for the agent")
	}
	dir := t.TempDir()
	argsFile = filepath.Join(dir, "args")
	binary = filepath.Join(dir, "tracepanel-agent")
	script := "#!/bin/sh\necho \"$@\" > " + argsFile + "\nexit 3\n"
	if err := os.WriteFile(binary, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return binary, argsFile
}

func TestEachCommandRunsTheAgentOnTheProjectsTraces(t *testing.T) {
	binary, argsFile := fakeAgent(t)
	t.Setenv("TRACEPANEL_AGENT_BIN", binary)
	t.Setenv("TRACEPANEL_DIR", "/srv/shop/.tracepanel")
	cases := []struct {
		args []string
		want string
	}{
		{[]string{"mcp"}, "mcp --dir /srv/shop/.tracepanel"},
		{[]string{"mcp", "--values"}, "mcp --dir /srv/shop/.tracepanel --values"},
		{[]string{"dev", "--port", "9100"}, "--dir /srv/shop/.tracepanel --serve 127.0.0.1:9100"},
		{[]string{"agent", "licenses"}, "licenses"},
	}
	for _, c := range cases {
		code := run(c.args)
		got, _ := os.ReadFile(argsFile)

		// The agent's exit code is the command's.
		if code != 3 || strings.TrimSpace(string(got)) != c.want {
			t.Errorf("%v: exit %d, agent ran with %q, want %q", c.args, code, got, c.want)
		}
	}
}

func TestAnUnknownCommandSaysHowToUseIt(t *testing.T) {
	if code := run([]string{"bogus"}); code != 2 {
		t.Fatalf("exit %d", code)
	}
	if code := run([]string{"version"}); code != 0 {
		t.Fatalf("version: exit %d", code)
	}
}

func TestWithoutAnAgentTheCommandFails(t *testing.T) {
	t.Setenv("TRACEPANEL_AGENT_BIN", filepath.Join(t.TempDir(), "missing"))

	if code := run([]string{"mcp"}); code != 1 {
		t.Fatalf("exit %d", code)
	}
}

// A stand-in app that writes TRACEPANEL_ENABLED and its arguments where the test reads them.
func fakeApp(t *testing.T) (binary, seen string) {
	t.Helper()
	dir := t.TempDir()
	seen = filepath.Join(dir, "seen")
	binary = filepath.Join(dir, "shop")
	script := "#!/bin/sh\necho \"$TRACEPANEL_ENABLED $@\" > " + seen + "\nexit 4\n"
	if err := os.WriteFile(binary, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return binary, seen
}

func TestRunStartsTheAppRecording(t *testing.T) {
	_, _ = fakeAgent(t)
	shop, seen := fakeApp(t)
	t.Setenv("TRACEPANEL_ENABLED", "")

	code := run([]string{"run", shop, "--addr", ":9000"})
	got, _ := os.ReadFile(seen)

	if code != 4 || strings.TrimSpace(string(got)) != "1 --addr :9000" {
		t.Fatalf("exit %d, the app saw %q", code, got)
	}
}

func TestDevStartsTheAppBesideTheConsole(t *testing.T) {
	binary, argsFile := fakeAgent(t)
	shop, seen := fakeApp(t)
	// The console runs a while: the app with it.
	script, _ := os.ReadFile(binary)
	_ = os.WriteFile(binary, []byte(strings.Replace(string(script), "exit 3", "sleep 1\nexit 3", 1)), 0o755)
	t.Setenv("TRACEPANEL_AGENT_BIN", binary)
	t.Setenv("TRACEPANEL_DIR", "/srv/shop/.tracepanel")

	code := run([]string{"dev", "--port", "9100", "--", shop, "-v"})
	agentArgs, _ := os.ReadFile(argsFile)
	var app []byte
	for deadline := time.Now().Add(5 * time.Second); len(app) == 0 && time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		app, _ = os.ReadFile(seen)
	}

	if code != 3 || strings.TrimSpace(string(agentArgs)) != "--dir /srv/shop/.tracepanel --serve 127.0.0.1:9100" {
		t.Fatalf("exit %d, the agent ran with %q", code, agentArgs)
	}
	if strings.TrimSpace(string(app)) != "1 -v" {
		t.Fatalf("the app saw %q", app)
	}
}
