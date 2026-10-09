package tracepanel

import (
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// The agent starts on demand, as tracepanel-python starts it: the first trace finds none for the project, so this
// process starts one in its own session, serving the console on a free port. It outlives the app's restarts and exits
// after idleExit with nothing to do; one per project (a second one on the same directory exits at once).

const (
	agentBinary  = "tracepanel-agent"
	idleExit     = "1800"
	startWait    = time.Second
	consoleCheck = 2 * time.Second
)

// starter starts the project's agent at most once per process.
type starter struct {
	storeDir string
	once     sync.Once
}

// autoStart: unless TRACEPANEL_AGENT names an agent (or is "off"), the project's own is started when missing.
func autoStart() bool {
	value := os.Getenv("TRACEPANEL_AGENT")
	return value == "" || value == "auto"
}

// agentExecutable is $TRACEPANEL_AGENT_BIN, or tracepanel-agent on the PATH (pip install tracepanel puts it there).
func agentExecutable() string {
	if explicit := os.Getenv("TRACEPANEL_AGENT_BIN"); explicit != "" {
		if _, err := os.Stat(explicit); err == nil {
			return explicit
		}
		return ""
	}
	path, err := exec.LookPath(agentBinary)
	if err != nil {
		return ""
	}
	return path
}

// ensure makes sure an agent runs for the project: one already answers, or this process starts it (once). It
// reports whether there is one.
func (s *starter) ensure() bool {
	if agentAddress(s.storeDir) != "" {
		return true
	}
	if !autoStart() {
		return false
	}
	s.once.Do(s.start)
	return agentAddress(s.storeDir) != ""
}

func (s *starter) start() {
	executable := agentExecutable()
	if executable == "" {
		log.Printf("tracepanel: no %s found (pip install tracepanel, or set TRACEPANEL_AGENT_BIN): traces aren't kept", agentBinary)
		return
	}
	dir, err := filepath.Abs(s.storeDir)
	if err == nil {
		err = os.MkdirAll(dir, 0o755)
	}
	if err != nil {
		log.Printf("tracepanel: %v", err)
		return
	}
	logFile, err := os.OpenFile(filepath.Join(dir, "agent.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		log.Printf("tracepanel: %v", err)
		return
	}
	defer logFile.Close()
	cmd := exec.Command(executable, "--dir", dir, "--serve", "127.0.0.1:0", "--idle-exit", idleExit)
	cmd.Stdout, cmd.Stderr = logFile, logFile
	detach(cmd)
	if err := cmd.Start(); err != nil {
		log.Printf("tracepanel: the agent didn't start: %v", err)
		return
	}
	// Reaped when it exits (its own session: the app's exit doesn't stop it).
	go func() { _ = cmd.Wait() }()
	for deadline := time.Now().Add(startWait); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		if agentAddress(dir) != "" {
			log.Printf("tracepanel: agent started for %s", dir)
			return
		}
	}
	log.Printf("tracepanel: the agent didn't start (see %s): traces aren't kept", filepath.Join(dir, "agent.log"))
}

// console is where the widget loads from and opens the panel: the configured URL, else the one the project's agent
// serves (console.addr, read again every few seconds), else "" (no agent: no widget).
type console struct {
	configured string
	storeDir   string
	mu         sync.Mutex
	url        string
	checked    time.Time
}

func (c *console) URL() string {
	if c.configured != "" {
		return c.configured
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if time.Since(c.checked) > consoleCheck || c.url == "" {
		c.checked = time.Now()
		c.url = ""
		if data, err := os.ReadFile(filepath.Join(c.storeDir, "console.addr")); err == nil {
			c.url = strings.TrimSpace(string(data))
		}
	}
	return c.url
}
