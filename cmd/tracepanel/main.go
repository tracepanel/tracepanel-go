// Command tracepanel runs the Tracepanel agent the Go SDK runs (the one in the user's cache, downloaded the first
// time), for what a Go project has no pip or npm to run it with:
//
//	go run github.com/tracepanel/tracepanel-go/cmd/tracepanel mcp
//
// Its commands:
//
//	mcp [--values]       an MCP server for AI coding agents, on stdin and stdout, on the project's traces
//	dev [--port 8765]    the agent in the foreground, with the console on http://127.0.0.1:8765/
//	agent [ARGS...]      the agent itself, with its own arguments (agent --help lists them)
//	version              this SDK's version and the agent's
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
  mcp [--values]       an MCP server for AI coding agents, on stdin and stdout
  dev [--port 8765]    the agent in the foreground, with the console on http://127.0.0.1:8765/
  agent [ARGS...]      the agent itself, with its own arguments (agent --help lists them)
  version              this SDK's version and the agent's`

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
	case "dev":
		port := "8765"
		if at := slices.Index(rest, "--port"); at >= 0 && at+1 < len(rest) {
			port = rest[at+1]
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

// agent runs the agent with args, its input and output this command's, and exits as it exits.
func agent(args []string) int {
	binary := agentbin.Path()
	if binary == "" {
		fmt.Fprintln(os.Stderr, "tracepanel: no tracepanel-agent could be had (set TRACEPANEL_AGENT_BIN, or put it on the PATH)")
		return 1
	}
	cmd := exec.Command(binary, args...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	// Ctrl-C reaches the agent too, which stops on its own: this command waits for it. A SIGTERM is passed on.
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
