// Command tracepanel runs the Tracepanel agent the Go SDK runs (the one in the user's cache, downloaded the first
// time), for what a Go project has no pip or npm to run it with:
//
//	go run github.com/tracepanel/tracepanel-go/cmd/tracepanel mcp
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
	"os"
	"os/exec"
	"os/signal"
	"slices"
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
		if len(rest) == 0 {
			fmt.Fprintln(os.Stderr, usage)
			return 2
		}
		return foreground(app(rest))
	case "dev":
		var command []string
		if at := slices.Index(rest, "--"); at >= 0 {
			rest, command = rest[:at], rest[at+1:]
		}
		port := "8765"
		if at := slices.Index(rest, "--port"); at >= 0 && at+1 < len(rest) {
			port = rest[at+1]
		}
		if len(command) > 0 {
			// The app beside the console, until the console stops.
			cmd := app(command)
			cmd.Stdin = nil
			if err := cmd.Start(); err != nil {
				fmt.Fprintf(os.Stderr, "tracepanel: %v\n", err)
				return 1
			}
			go func() { _ = cmd.Wait() }()
			defer func() { _ = cmd.Process.Kill() }()
		}
		return agent([]string{"--dir", dir, "--serve", "127.0.0.1:" + port})
	case "agent":
		return agent(rest)
	case "version", "--version":
		fmt.Printf("tracepanel-go %s, tracepanel-agent %s\n", tracepanel.Version, tracepanel.AgentVersion)
		return 0
	case "help", "--help", "-h":
		fmt.Println(usage)
		return 0
	}
	fmt.Fprintln(os.Stderr, usage)
	return 2
}

// app is the app's command, recording: TRACEPANEL_ENABLED=1, whatever built it.
func app(command []string) *exec.Cmd {
	cmd := exec.Command(command[0], command[1:]...)
	cmd.Env = append(os.Environ(), "TRACEPANEL_ENABLED=1")
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	return cmd
}

// agent runs the agent with args, its input and output this command's, and exits as it exits.
func agent(args []string) int {
	binary := agentbin.Path()
	if binary == "" {
		fmt.Fprintln(os.Stderr, "tracepanel: no tracepanel-agent could be had (set TRACEPANEL_AGENT_BIN, or put it on the PATH)")
		return 1
	}
	cmd := exec.Command(binary, args...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	return foreground(cmd)
}

// foreground runs cmd and exits as it exits: Ctrl-C reaches it (it stops on its own) and a SIGTERM is passed on.
func foreground(cmd *exec.Cmd) int {
	signal.Ignore(os.Interrupt)
	if err := cmd.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "tracepanel: %v\n", err)
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
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		if code := exit.ExitCode(); code >= 0 {
			return code
		}
		return 1
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "tracepanel: %v\n", err)
		return 1
	}
	return 0
}
