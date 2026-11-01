//go:build unix && !linux

package tracepanel

import "syscall"

func dup2(from, to int) error { return syscall.Dup2(from, to) }
