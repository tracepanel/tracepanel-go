//go:build !windows

package e2e

import (
	"os/exec"
	"syscall"
)

// ownGroup starts cmd in a process group of its own, which killGroup kills: it and the agent it runs.
func ownGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

func killGroup(cmd *exec.Cmd) {
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
}
