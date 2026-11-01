//go:build unix

package tracepanel

import (
	"os"
	"syscall"
)

// swapStderr makes w the process's stderr (its file descriptor 2) until restore. No process starts meanwhile (the
// fork lock is held), so none is left with the pipe as its stderr.
func swapStderr(w *os.File) (restore func(), err error) {
	syscall.ForkLock.Lock()
	saved, err := syscall.Dup(2)
	if err != nil {
		syscall.ForkLock.Unlock()
		return nil, err
	}
	syscall.CloseOnExec(saved)
	if err := dup2(int(w.Fd()), 2); err != nil {
		_ = syscall.Close(saved)
		syscall.ForkLock.Unlock()
		return nil, err
	}
	return func() {
		_ = dup2(saved, 2)
		_ = syscall.Close(saved)
		syscall.ForkLock.Unlock()
	}, nil
}
