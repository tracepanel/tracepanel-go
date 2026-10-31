package main

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
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
