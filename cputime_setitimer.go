//go:build unix && !linux

package tracepanel

import (
	"syscall"
	"time"
)

// processCPUTime is the CPU time the process has used (user and system), for a profiler whose samples fall short of
// its rate and fall unevenly. Go's profiler runs off one process-wide timer here (setitimer), and the kernel posts its
// signals only as a thread enters or leaves it (Linux has a timer per thread, and takes every sample). So the samples
// are weighed against the process's CPU time (weigh).
var processCPUTime = func() time.Duration {
	var usage syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &usage); err != nil {
		return 0
	}
	return time.Duration(usage.Utime.Nano() + usage.Stime.Nano())
}
