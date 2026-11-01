package tracepanel

import (
	"bytes"
	"io"
	"os"
	"runtime"
	"runtime/pprof"
	"time"
)

// How often the profiler samples: every millisecond of CPU time, as the other SDKs do, so a request of a few
// milliseconds has a profile to read. Go's own rate (runtime/pprof's, 100 a second) sees a sample per 10 ms.
//
// runtime/pprof starts its profile at its own rate, and only once the runtime runs at none: the rate is set before
// it, with runtime.SetCPUProfileRate, and runtime/pprof's try to set its own then prints a warning to stderr
// (cpuRateWarning) and does nothing. The process's stderr is a pipe while it starts, and what went there but the
// warning goes on to stderr: an app sees nothing of it. On a platform where stderr can't be swapped so, the profile
// runs at Go's rate. Sampling every millisecond costs too little to measure on a CPU-bound handler (a signal and a
// stack walk per millisecond of CPU), and only while a profiled trace runs.
const (
	defaultProfileInterval = time.Millisecond
	// The most often it samples, whatever the setting.
	minProfileInterval = 100 * time.Microsecond
	pprofHz            = 100
	cpuRateWarning     = "runtime: cannot set cpu profile rate until previous profile has finished.\n"
)

// profileHz is the profiler's rate for an interval: Config.ProfileInterval, TRACEPANEL_PROFILE_INTERVAL_MS.
func profileHz(interval time.Duration) int {
	if interval <= 0 {
		interval = defaultProfileInterval
	}
	interval = max(interval, minProfileInterval)
	return max(int(time.Second/interval), 1)
}

// startCPUProfile starts runtime/pprof's CPU profile, hz samples a second.
func startCPUProfile(out io.Writer, hz int) error {
	if hz == pprofHz {
		return pprof.StartCPUProfile(out)
	}
	var err error
	ran := quietly(func() {
		// With another CPU profile running (the app's own), the rate stays as it is, and runtime/pprof says no.
		runtime.SetCPUProfileRate(hz)
		err = pprof.StartCPUProfile(out)
	})
	if !ran {
		return pprof.StartCPUProfile(out)
	}
	return err
}

// quietly runs f with the process's stderr a pipe, and writes what went there to stderr after it, but for the
// runtime's warning: false, f not run, when stderr can't be swapped.
func quietly(f func()) bool {
	r, w, err := os.Pipe()
	if err != nil {
		return false
	}
	restore, err := swapStderr(w)
	if err != nil {
		_ = r.Close()
		_ = w.Close()
		return false
	}
	read := make(chan []byte, 1)
	go func() {
		written, _ := io.ReadAll(r)
		_ = r.Close()
		read <- written
	}()
	f()
	restore()
	_ = w.Close()
	if rest := withoutRateWarning(<-read); len(rest) > 0 {
		_, _ = os.Stderr.Write(rest)
	}
	return true
}

// withoutRateWarning is what went to stderr, without the runtime's warnings of the rate.
func withoutRateWarning(written []byte) []byte {
	return bytes.ReplaceAll(written, []byte(cpuRateWarning), nil)
}
