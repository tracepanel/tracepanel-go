//go:build unix && !linux

package tracepanel

import (
	"syscall"
	"time"
)

// processCPUTime is the CPU time the process has used (user and system), for a profiler whose samples fall short of
// its rate. Go's profiler runs off one process-wide timer here (setitimer), and the kernel delivers its signals as its
// own clock ticks allow: on macOS a fifth to a third of 1,000 a second, however high the rate is set (Linux has a timer
// per thread, and every one). So a sample stands for more CPU time than the rate says: the profile's CPU time over
// its samples.
var processCPUTime = func() time.Duration {
	var usage syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &usage); err != nil {
		return 0
	}
	return time.Duration(usage.Utime.Nano() + usage.Stime.Nano())
}
