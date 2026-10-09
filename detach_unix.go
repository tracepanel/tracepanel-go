//go:build !windows

package tracepanel

import (
	"os/exec"
	"syscall"
)

// detach puts the agent in a session of its own: Ctrl-C and the app's restarts don't reach it.
func detach(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
}
