// Command tracepanel runs the Tracepanel agent the Go SDK runs (the one in the user's cache, downloaded the first
// time), for what a Go project has no pip or npm to run it with, and a built app recording:
//
//	go install github.com/tracepanel/tracepanel-go/cmd/tracepanel@v0.10.0
//	tracepanel run ./shop
//	tracepanel mcp
//
// `run` exits as the app exits, with its exit code, and says on stderr how an app that failed ended
// ("tracepanel: ./shop exited with status 3"): under `go run`, which exits 1 whatever the program's code, that line
// is where the app's status is.
//
// Its commands:
//
//	run COMMAND...                    the app, recording: TRACEPANEL_ENABLED=1 for it (go run records anyway)
//	dev [--port 8765] [-- COMMAND...] the agent in the foreground, with the console on http://127.0.0.1:8765/,
//	                                  and the app with TRACEPANEL_ENABLED=1 when a command follows --
//	mcp [--values]                    an MCP server for AI coding agents, on stdin and stdout, on the project's traces
//	agent [ARGS...]                   the agent itself, with its own arguments (agent --help lists them)
//	version                           this SDK's version and the agent's
//
// The SDK records in development only: under `go run`, or with TRACEPANEL_ENABLED=1, which `run` and `dev --` set
// for a binary `go build` wrote.
//
// The project's traces are in TRACEPANEL_DIR, else .tracepanel, as the SDK keeps them. TRACEPANEL_AGENT_BIN runs
// another build of the agent.
package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"slices"
	"sync/atomic"
	"syscall"

	tracepanel "github.com/tracepanel/tracepanel-go"
	"github.com/tracepanel/tracepanel-go/internal/agentbin"
)

const usage = `usage: tracepanel <command>
  run COMMAND...                     the app, recording (TRACEPANEL_ENABLED=1)
  dev [--port 8765] [-- COMMAND...]  the agent in the foreground, with the console on http://127.0.0.1:8765/,
                                     and the app, recording, when a command follows --
  mcp [--values]                     an MCP server for AI coding agents, on stdin and stdout
  agent [ARGS...]                    the agent itself, with its own arguments (agent --help lists them)
  version                            this SDK's version and the agent's`

// stderr is where the command says what went wrong (a test's buffer).
var stderr io.Writer = os.Stderr

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	if len(args) == 0 {
		fmt.Println(usage)
		return 0
	}
	command, rest := args[0], args[1:]
	dir := os.Getenv("TRACEPANEL_DIR")
	if dir == "" {
		dir = ".tracepanel"
	}
	switch command {
	case "mcp":
		agentArgs := []string{"mcp", "--dir", dir}
		if slices.Contains(rest, "--values") {
			agentArgs = append(agentArgs, "--values")
		}
		return agent(agentArgs)
	case "run":
		if len(rest) > 0 && rest[0] == "--" {
			rest = rest[1:]
		}
		if len(rest) == 0 {
			fmt.Fprintln(stderr, usage)
			return 2
		}
		return foreground(app(rest), rest[0])
	case "dev":
		var command []string
		if at := slices.Index(rest, "--"); at >= 0 {
			rest, command = rest[:at], rest[at+1:]
		}
		port := "8765"
		if at := slices.Index(rest, "--port"); at >= 0 && at+1 < len(rest) {
			port = rest[at+1]
		}
		console := []string{"--dir", dir, "--serve", "127.0.0.1:" + port}
		if len(command) == 0 {
			return agent(console)
		}
		return beside(app(command), command[0], console)
	case "agent":
		return agent(rest)
	case "version", "--version":
		fmt.Printf("tracepanel-go %s, tracepanel-agent %s\n", tracepanel.Version, tracepanel.AgentVersion)
		return 0
	case "help", "--help", "-h":
		fmt.Println(usage)
		return 0
	}
	fmt.Fprintln(stderr, usage)
	return 2
}

// app is the app's command, recording: TRACEPANEL_ENABLED=1, whatever built it.
func app(command []string) *exec.Cmd {
	cmd := exec.Command(command[0], command[1:]...)
	cmd.Env = append(os.Environ(), "TRACEPANEL_ENABLED=1")
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	return cmd
}

// beside runs the app (`name`) beside the console, until the console stops: the console's exit code is the command's.
// An app that fails on its own says how it ended on stderr; the one this command stops says nothing.
func beside(cmd *exec.Cmd, name string, console []string) int {
	cmd.Stdin = nil
	if err := cmd.Start(); err != nil {
		fmt.Fprintf(stderr, "tracepanel: %v\n", err)
		return 1
	}
	var stopping atomic.Bool
	ended := make(chan struct{})
	go func() {
		defer close(ended)
		if err := cmd.Wait(); !stopping.Load() {
			reportExit(name, err)
		}
	}()
	code := agent(console)
	stopping.Store(true)
	_ = cmd.Process.Kill()
	<-ended
	return code
}

// agent runs the agent with args, its input and output this command's, and exits as it exits.
func agent(args []string) int {
	binary := agentbin.Path()
	if binary == "" {
		fmt.Fprintln(stderr, "tracepanel: no tracepanel-agent could be had (set TRACEPANEL_AGENT_BIN, or put it on the PATH)")
		return 1
	}
	cmd := exec.Command(binary, args...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	return foreground(cmd, "")
}

// foreground runs cmd and exits as it exits: Ctrl-C reaches it (it stops on its own) and a SIGTERM is passed on. An
// app (`name`, "" for the agent) that fails says how it ended on stderr.
func foreground(cmd *exec.Cmd, name string) int {
	signal.Ignore(os.Interrupt)
	if err := cmd.Start(); err != nil {
		fmt.Fprintf(stderr, "tracepanel: %v\n", err)
		return 1
	}
	terminate := make(chan os.Signal, 1)
	signal.Notify(terminate, syscall.SIGTERM)
	go func() {
		for sig := range terminate {
			_ = cmd.Process.Signal(sig)
		}
	}()
	err := cmd.Wait()
	if name != "" {
		reportExit(name, err)
	}
	return exitCode(err)
}

// exitCode is the command's for a child that ended with err: its own, 128 and the signal's number for one a signal
// ended (as a shell has it), 1 when it couldn't run.
func exitCode(err error) int {
	var exit *exec.ExitError
	if !errors.As(err, &exit) {
		if err != nil {
			fmt.Fprintf(stderr, "tracepanel: %v\n", err)
			return 1
		}
		return 0
	}
	if code := exit.ExitCode(); code >= 0 {
		return code
	}
	if status, ok := exit.Sys().(syscall.WaitStatus); ok && status.Signaled() && status.Signal() > 0 {
		return 128 + int(status.Signal())
	}
	return 1
}

// reportExit says on stderr how an app that failed ended: its status, or the signal that ended it (not Ctrl-C's nor
// SIGTERM's, which this command passed on).
func reportExit(name string, err error) {
	var exit *exec.ExitError
	if !errors.As(err, &exit) {
		return
	}
	if code := exit.ExitCode(); code > 0 {
		fmt.Fprintf(stderr, "tracepanel: %s exited with status %d\n", name, code)
		return
	}
	if status, ok := exit.Sys().(syscall.WaitStatus); ok && status.Signaled() {
		if signal := status.Signal(); signal != syscall.SIGINT && signal != syscall.SIGTERM {
			fmt.Fprintf(stderr, "tracepanel: %s ended by %s\n", name, exit.ProcessState)
		}
	}
}
