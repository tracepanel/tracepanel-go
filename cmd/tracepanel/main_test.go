package main

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

// The test binary stands in for the agent and the app, on every OS (Windows too): copied as tracepanel-agent, it
// writes its arguments to args beside it and exits 3 (after TRACEPANEL_TEST_AGENT_SLEEP); as shop, it writes
// TRACEPANEL_ENABLED and its arguments to seen beside it and exits 4 (or TRACEPANEL_TEST_EXIT).
func TestMain(m *testing.M) {
	self, _ := os.Executable()
	dir, name := filepath.Dir(self), strings.TrimSuffix(filepath.Base(self), ".exe")
	switch name {
	case "tracepanel-agent":
		_ = os.WriteFile(filepath.Join(dir, "args"), []byte(strings.Join(os.Args[1:], " ")+"\n"), 0o644)
		if pause, err := time.ParseDuration(os.Getenv("TRACEPANEL_TEST_AGENT_SLEEP")); err == nil {
			time.Sleep(pause)
		}
		os.Exit(3)
	case "shop":
		seen := strings.TrimSpace(os.Getenv("TRACEPANEL_ENABLED") + " " + strings.Join(os.Args[1:], " "))
		_ = os.WriteFile(filepath.Join(dir, "seen"), []byte(seen+"\n"), 0o644)
		code, err := strconv.Atoi(os.Getenv("TRACEPANEL_TEST_EXIT"))
		if err != nil {
			code = 4
		}
		os.Exit(code)
	}
	os.Exit(m.Run())
}

// standIn copies the test binary into dir as name (name.exe on Windows): its path.
func standIn(t *testing.T, dir, name string) string {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(self)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	binary := filepath.Join(dir, name)
	if err := os.WriteFile(binary, data, 0o755); err != nil {
		t.Fatal(err)
	}
	return binary
}

// A stand-in agent that writes its arguments where the test reads them.
func fakeAgent(t *testing.T) (binary, argsFile string) {
	t.Helper()
	dir := t.TempDir()
	return standIn(t, dir, "tracepanel-agent"), filepath.Join(dir, "args")
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
	return standIn(t, dir, "shop"), filepath.Join(dir, "seen")
}

func TestRunStartsTheAppRecording(t *testing.T) {
	_, _ = fakeAgent(t)
	shop, seen := fakeApp(t)
	t.Setenv("TRACEPANEL_ENABLED", "")

	said := said(t)

	code := run([]string{"run", shop, "--addr", ":9000"})
	got, _ := os.ReadFile(seen)

	if code != 4 || strings.TrimSpace(string(got)) != "1 --addr :9000" {
		t.Fatalf("exit %d, the app saw %q", code, got)
	}
	// Under go run, which exits 1 whatever the app's code, the app's status is in this line.
	if said.String() != "tracepanel: "+shop+" exited with status 4\n" {
		t.Fatalf("stderr %q", said.String())
	}
}

// said is what the command says on stderr while the test runs.
func said(t *testing.T) *bytes.Buffer {
	t.Helper()
	var out bytes.Buffer
	before := stderr
	stderr = &out
	t.Cleanup(func() { stderr = before })
	return &out
}

func TestRunSaysHowAnAppThatFailedEnded(t *testing.T) {
	dir := t.TempDir()
	standIn(t, dir, "shop")
	t.Chdir(dir)
	for code, want := range map[int]string{0: "", 3: "tracepanel: ./shop exited with status 3\n"} {
		t.Setenv("TRACEPANEL_TEST_EXIT", strconv.Itoa(code))
		said := said(t)

		got := run([]string{"run", "./shop"})

		if got != code || said.String() != want {
			t.Errorf("exit %d: exit %d, said %q", code, got, said.String())
		}
	}
}

func TestRunSaysWhichSignalEndedTheApp(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no signals on Windows")
	}
	dir := t.TempDir()
	cases := []struct {
		script string
		code   int
		said   string
	}{
		{"kill -KILL $$", 128 + 9, "tracepanel: ./shop ended by signal: killed\n"},
		// Ctrl-C's and SIGTERM's: this command passed them on, nothing to say.
		{"kill -TERM $$", 128 + 15, ""},
	}
	for _, c := range cases {
		if err := os.WriteFile(filepath.Join(dir, "shop"), []byte("#!/bin/sh\n"+c.script+"\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		t.Chdir(dir)
		said := said(t)

		code := run([]string{"run", "./shop"})

		if code != c.code || said.String() != c.said {
			t.Errorf("%s: exit %d, said %q", c.script, code, said.String())
		}
	}
}
