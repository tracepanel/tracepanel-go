package tracepanel

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func TestConsoleURL(t *testing.T) {
	dir := t.TempDir()

	if got := (&console{configured: "http://elsewhere:9/", storeDir: dir}).URL(); got != "http://elsewhere:9/" {
		t.Fatalf("configured: %q", got)
	}
	// No agent: no console, so no widget.
	if got := (&console{storeDir: dir}).URL(); got != "" {
		t.Fatalf("without an agent: %q", got)
	}
	if err := os.WriteFile(filepath.Join(dir, "console.addr"), []byte("http://127.0.0.1:52811/\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := (&console{storeDir: dir}).URL(); got != "http://127.0.0.1:52811/" {
		t.Fatalf("the agent's: %q", got)
	}
}

func TestNoAgentToStart(t *testing.T) {
	t.Setenv("TRACEPANEL_AGENT", "")
	t.Setenv("TRACEPANEL_AGENT_BIN", filepath.Join(t.TempDir(), "missing"))
	s := &starter{storeDir: t.TempDir()}

	if s.ensure() {
		t.Fatal("no binary, yet an agent")
	}
	// A named agent is never started for the project.
	t.Setenv("TRACEPANEL_AGENT", "off")
	if autoStart() {
		t.Fatal("off, yet started")
	}
}

// The real agent, when a build of the sibling tracepanel-agent checkout is there (or $TRACEPANEL_AGENT_BIN).
func TestTheFirstTraceStartsTheAgent(t *testing.T) {
	executable := os.Getenv("TRACEPANEL_AGENT_BIN")
	if executable == "" {
		executable = filepath.Join("..", "tracepanel-agent", "target", "debug", agentBinary)
	}
	if _, err := os.Stat(executable); err != nil || runtime.GOOS == "windows" {
		t.Skip("no tracepanel-agent build")
	}
	absolute, _ := filepath.Abs(executable)
	t.Setenv("TRACEPANEL_AGENT", "")
	t.Setenv("TRACEPANEL_AGENT_BIN", absolute)
	dir := filepath.Join(t.TempDir(), ".tracepanel")
	t.Cleanup(func() { _ = exec.Command("pkill", "-TERM", "-f", "--", dir).Run(); time.Sleep(200 * time.Millisecond) })
	s := &starter{storeDir: dir}

	if !s.ensure() {
		t.Fatal("no agent after ensure")
	}
	if got := (&console{storeDir: dir}).URL(); got == "" {
		t.Fatal("the agent serves no console")
	}
	// One per process: asking again starts nothing new.
	if !s.ensure() {
		t.Fatal("the agent went away")
	}
}
