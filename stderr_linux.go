//go:build linux

package tracepanel

import "syscall"

// dup2: Linux on arm64 (and others) has dup3 only.
func dup2(from, to int) error { return syscall.Dup3(from, to, 0) }
