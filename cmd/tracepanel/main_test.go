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

// `dev -- COMMAND` runs the command after --, whatever comes before it: its status line names the app, and the
// console's exit code is the command's.
func TestDevRunsTheCommandAfterDashDash(t *testing.T) {
	binary, argsFile := fakeAgent(t)
	t.Setenv("TRACEPANEL_AGENT_BIN", binary)
	t.Setenv("TRACEPANEL_DIR", ".tracepanel")
	// The console stays up until the app has ended.
	t.Setenv("TRACEPANEL_TEST_AGENT_SLEEP", "2s")
	t.Setenv("TRACEPANEL_TEST_EXIT", "5")
	dir := t.TempDir()
	standIn(t, dir, "shop")
	t.Chdir(dir)
	cases := []struct {
		args    []string
		console string
	}{
		{[]string{"dev", "--", "./shop", "--addr", ":9000"}, "--dir .tracepanel --serve 127.0.0.1:8765"},
		{[]string{"dev", "--port", "9100", "--", "./shop", "--addr", ":9000"}, "--dir .tracepanel --serve 127.0.0.1:9100"},
	}
	for _, c := range cases {
		_ = os.Remove(filepath.Join(dir, "seen"))
		said := said(t)

		code := run(c.args)
		console, _ := os.ReadFile(argsFile)
		seen, _ := os.ReadFile(filepath.Join(dir, "seen"))

		if code != 3 || strings.TrimSpace(string(console)) != c.console {
			t.Errorf("%v: exit %d, the console ran with %q", c.args, code, console)
		}

		if strings.TrimSpace(string(seen)) != "1 --addr :9000" {
			t.Errorf("%v: the app saw %q", c.args, seen)
		}

		if said.String() != "tracepanel: ./shop exited with status 5\n" {
			t.Errorf("%v: stderr %q", c.args, said.String())
		}
	}
}

// Without a command after --, `dev` is the console alone.
func TestDevWithoutACommandIsTheConsole(t *testing.T) {
	binary, argsFile := fakeAgent(t)
	t.Setenv("TRACEPANEL_AGENT_BIN", binary)
	t.Setenv("TRACEPANEL_DIR", ".tracepanel")
	for _, args := range [][]string{{"dev"}, {"dev", "--"}, {"dev", "--port"}, {"dev", "--port", "9100", "--"}} {
		said := said(t)

		code := run(args)
		console, _ := os.ReadFile(argsFile)

		want := "--dir .tracepanel --serve 127.0.0.1:8765"
		if len(args) == 4 {
			want = "--dir .tracepanel --serve 127.0.0.1:9100"
		}
		if code != 3 || strings.TrimSpace(string(console)) != want || said.String() != "" {
			t.Errorf("%v: exit %d, the console ran with %q, said %q", args, code, console, said.String())
		}
	}
}

// The app the console's end stops says nothing about how it ended.
func TestDevStopsTheAppQuietly(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("a shell script app")
	}
	binary, _ := fakeAgent(t)
	t.Setenv("TRACEPANEL_AGENT_BIN", binary)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "shop"), []byte("#!/bin/sh\nexec sleep 30\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	said := said(t)
	start := time.Now()

	code := run([]string{"dev", "--", "./shop"})

	if code != 3 || said.String() != "" || time.Since(start) > 10*time.Second {
		t.Fatalf("exit %d after %s, said %q", code, time.Since(start), said.String())
	}
}

// `run` without a command says how to use it; `run -- COMMAND` is `run COMMAND`.
func TestRunsEdgeCases(t *testing.T) {
	dir := t.TempDir()
	standIn(t, dir, "shop")
	t.Chdir(dir)
	t.Setenv("TRACEPANEL_TEST_EXIT", "3")
	cases := []struct {
		args []string
		code int
		said string
	}{
		{[]string{"run"}, 2, usage + "\n"},
		{[]string{"run", "--"}, 2, usage + "\n"},
		{[]string{"run", "--", "./shop"}, 3, "tracepanel: ./shop exited with status 3\n"},
	}
	for _, c := range cases {
		said := said(t)

		code := run(c.args)

		if code != c.code || said.String() != c.said {
			t.Errorf("%v: exit %d, said %q", c.args, code, said.String())
		}
	}
}
