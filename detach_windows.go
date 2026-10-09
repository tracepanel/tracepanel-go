//go:build windows

package tracepanel

import (
	"os/exec"
	"syscall"
)

const detachedProcess = 0x00000008

// detach puts the agent in a process group of its own, without the app's console: Ctrl-C doesn't reach it.
func detach(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP | detachedProcess}
}
