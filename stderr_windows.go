//go:build windows

package tracepanel

import (
	"os"
	"syscall"
)

var setStdHandle = syscall.NewLazyDLL("kernel32.dll").NewProc("SetStdHandle")

// swapStderr makes w the process's standard error handle, which the runtime writes its warnings to, until restore.
func swapStderr(w *os.File) (restore func(), err error) {
	which := syscall.STD_ERROR_HANDLE
	saved, err := syscall.GetStdHandle(which)
	if err != nil {
		return nil, err
	}
	if ok, _, err := setStdHandle.Call(uintptr(which), w.Fd()); ok == 0 {
		return nil, err
	}
	return func() { _, _, _ = setStdHandle.Call(uintptr(which), uintptr(saved)) }, nil
}

// writeStderr writes b to the process's standard error handle, whatever os.Stderr is.
func writeStderr(b []byte) {
	handle, err := syscall.GetStdHandle(syscall.STD_ERROR_HANDLE)
	if err != nil {
		return
	}
	for len(b) > 0 {
		var n uint32
		if err := syscall.WriteFile(handle, b, &n, nil); err != nil || n == 0 {
			return
		}
		b = b[n:]
	}
}
