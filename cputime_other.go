//go:build linux || !unix

package tracepanel

import "time"

// processCPUTime: none, where every sample the rate asks for is taken (Linux: a timer per thread) or the process's CPU
// time isn't read (cputime_setitimer.go).
var processCPUTime func() time.Duration
