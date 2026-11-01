//go:build unix

package tracepanel

import (
	"net/http"
	"net/http/httptest"
	"runtime"
	"syscall"
	"testing"
	"time"
)

// burnCPU keeps a CPU busy for d of the process's CPU time, however many other processes share the CPUs.
func burnCPU(t *testing.T, d time.Duration) (sum float64) {
	used := func() time.Duration {
		var usage syscall.Rusage
		if err := syscall.Getrusage(syscall.RUSAGE_SELF, &usage); err != nil {
			t.Fatal(err)
		}
		return time.Duration(usage.Utime.Nano() + usage.Stime.Nano())
	}
	// The process's CPU time is read every 10 ms only: macOS delivers the profiler's signals as the thread enters the
	// kernel, so a loop that calls it often gets more of them than the app's code would.
	for until := used() + d; used() < until; {
		for deadline := time.Now().Add(10 * time.Millisecond); time.Now().Before(deadline); {
			for i := range 10_000 {
				sum += float64(i) * 1.0001
			}
		}
	}
	return sum
}

func TestACPUBoundRequestsProfileHasItsCPUTime(t *testing.T) {
	const cpu = 300 * time.Millisecond
	recorded := record(t)
	recorded.t.profiler = newProfiler()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /report", func(w http.ResponseWriter, r *http.Request) { burnCPU(t, cpu) })
	request := httptest.NewRequest(http.MethodGet, "/report", nil)
	request.Header.Set("X-Tracepanel-Profile", "1")
	recorded.t.middleware(mux).ServeHTTP(httptest.NewRecorder(), request)

	profile := recorded.wait(t, 1)[0].Profile
	sampled := time.Duration(profile.Samples*profile.IntervalUs) * time.Microsecond
	if profile.Dropped != 0 || sampled < cpu*3/4 || sampled > cpu*5/4 {
		t.Fatalf("%d samples, one every %d µs: %v of %v on CPU", profile.Samples, profile.IntervalUs, sampled, cpu)
	}

	// Linux has a timer per thread: every sample the rate asks for, one a millisecond.
	if runtime.GOOS == "linux" && (profile.IntervalUs != 1000 || profile.Samples < 225 || profile.Samples > 375) {
		t.Fatalf("%d samples, one every %d µs", profile.Samples, profile.IntervalUs)
	}
}
