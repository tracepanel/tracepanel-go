package tracepanel

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/tracepanel/tracepanel-go/internal/agentbin"
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

// fakeAgent is what the test binary does as the agent: it says how it was started on its output (the agent's log),
// and where it listens, as the agent writes it into its directory, and exits.
func fakeAgent(args []string) {
	fmt.Println("fake agent:", strings.Join(args, " "))
	for i, arg := range args {
		if arg == "--dir" && i+1 < len(args) {
			_ = os.WriteFile(filepath.Join(args[i+1], "console.addr"), []byte("http://127.0.0.1:9/\n"), 0o644)
			_ = os.WriteFile(filepath.Join(args[i+1], "agent.addr"), []byte("tcp:127.0.0.1:9\n"), 0o644)
		}
	}
}

// The agent starts as every OS has it (Windows too: a process group of its own, without the app's console), with its
// arguments, its output in agent.log, the store's directory made for it.
func TestTheAgentStartsDetachedWithItsLog(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("TRACEPANEL_AGENT", "")
	t.Setenv("TRACEPANEL_AGENT_BIN", executable)
	t.Setenv("TRACEPANEL_TEST_FAKE_AGENT", "1")
	dir := filepath.Join(t.TempDir(), "shop", ".tracepanel")
	s := &starter{storeDir: dir}

	started := s.ensure()
	var log []byte
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		if log, _ = os.ReadFile(filepath.Join(dir, "agent.log")); len(log) > 0 {
			break
		}
	}

	want := "fake agent: --dir " + dir + " --serve 127.0.0.1:0 --idle-exit 1800\n"
	if !started || strings.ReplaceAll(string(log), "\r\n", "\n") != want {
		t.Fatalf("started %v, agent.log %q, want %q", started, log, want)
	}
	if got := (&console{storeDir: dir}).URL(); got != "http://127.0.0.1:9/" {
		t.Fatalf("console %q", got)
	}
}

// The real agent, when a build of the sibling tracepanel checkout is there (or $TRACEPANEL_AGENT_BIN).
func TestTheFirstTraceStartsTheAgent(t *testing.T) {
	executable := os.Getenv("TRACEPANEL_AGENT_BIN")
	if executable == "" {
		executable = filepath.Join("..", "tracepanel", "target", "debug", agentbin.Binary)
	}
	if _, err := os.Stat(executable); err != nil || runtime.GOOS == "windows" {
		t.Skip("no tracepanel-agent build")
	}
	absolute, _ := filepath.Abs(executable)
	t.Setenv("TRACEPANEL_AGENT", "")
	t.Setenv("TRACEPANEL_AGENT_BIN", absolute)
	// Not the machine's OpenTelemetry port: another agent may have it.
	t.Setenv("TRACEPANEL_OTLP", "off")
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
