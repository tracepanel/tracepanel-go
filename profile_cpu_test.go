//go:build unix

package tracepanel

import (
	"bufio"
	"net"
	"net/http"
	"net/http/httptest"
	"runtime"
	"syscall"
	"testing"
	"time"
)

// cpuUsed is the CPU time the process has used.
func cpuUsed(t *testing.T) time.Duration {
	var usage syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &usage); err != nil {
		t.Fatal(err)
	}
	return time.Duration(usage.Utime.Nano() + usage.Stime.Nano())
}

// burnCPU keeps a CPU busy for d of the process's CPU time, however many other processes share the CPUs.
func burnCPU(t *testing.T, d time.Duration) (sum float64) {
	// The process's CPU time is read every 10 ms only: a read is a system call, and the loop computes.
	for until := cpuUsed(t) + d; cpuUsed(t) < until; {
		for deadline := time.Now().Add(10 * time.Millisecond); time.Now().Before(deadline); {
			for i := range 10_000 {
				sum += float64(i) * 1.0001
			}
		}
	}
	return sum
}

// queryLike asks a server on a socket again and again for d, as an app asks its database: a write and a read each,
// and a wait for the answer.
func queryLike(t *testing.T, address string, d time.Duration) int {
	conn, err := net.Dial("tcp", address)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	reader := bufio.NewReader(conn)
	asked := 0
	for until := time.Now().Add(d); time.Now().Before(until); asked++ {
		if _, err := conn.Write([]byte("SELECT 1\n")); err != nil {
			t.Fatal(err)
		}
		if _, err := reader.ReadString('\n'); err != nil {
			t.Fatal(err)
		}
	}
	return asked
}

// databaseLike answers each line after a short while, as a database does.
func databaseLike(t *testing.T) string {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { listener.Close() })
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				reader := bufio.NewReader(conn)
				for {
					if _, err := reader.ReadString('\n'); err != nil {
						return
					}
					time.Sleep(100 * time.Microsecond)
					if _, err := conn.Write([]byte("1\n")); err != nil {
						return
					}
				}
			}()
		}
	}()
	return listener.Addr().String()
}

// sampledIn is the CPU time the profile gives a function, it and what it called.
func sampledIn(profile *profileDoc, function string) time.Duration {
	var count int64
	for _, stack := range profile.Stacks {
		for _, frame := range stack.Frames {
			if profile.Frames[frame].Function == function {
				count += stack.Count
				break
			}
		}
	}
	return time.Duration(count*profile.IntervalUs) * time.Microsecond
}

// A handler that queries for a while, then computes: the profile gives the loop its CPU time, not the queries'. On
// macOS the kernel hands the profiler's signals mostly to the thread that enters it (the queries'), so taken as they
// are the loop would get a fraction of its time; there the profile is weighed and says it is an estimate.
func TestAProfileGivesACPULoopItsTimeBesideQueries(t *testing.T) {
	const queries, cpu = 100 * time.Millisecond, 200 * time.Millisecond
	address := databaseLike(t)
	recorded := record(t)
	recorded.t.profiler = newProfiler()
	var burnt time.Duration
	mux := http.NewServeMux()
	mux.HandleFunc("GET /report", func(w http.ResponseWriter, r *http.Request) {
		queryLike(t, address, queries)
		before := cpuUsed(t)
		burnCPU(t, cpu)
		burnt = cpuUsed(t) - before
	})
	request := httptest.NewRequest(http.MethodGet, "/report", nil)
	request.Header.Set("X-Tracepanel-Profile", "1")
	started := time.Now()
	recorded.t.middleware(mux).ServeHTTP(httptest.NewRecorder(), request)
	took := time.Since(started)

	profile := recorded.wait(t, 1)[0].Profile
	loop, asking := sampledIn(profile, "burnCPU"), sampledIn(profile, "queryLike")
	if profile.IntervalUs != 1000 || profile.Dropped != 0 || loop < burnt*7/10 || loop > burnt*13/10 {
		t.Fatalf("burnCPU %v of its %v on CPU (queryLike %v), from %d samples, %d estimated", loop, burnt, asking,
			profile.Samples, profile.EstimatedFrom)
	}
	t.Logf("burnCPU %v of its %v on CPU, queryLike %v, %d ms in all from %d samples", loop, burnt, asking,
		profile.Samples, profile.EstimatedFrom)
	// Nothing on the CPU longer than it ran.
	if asking > queries+10*time.Millisecond || time.Duration(profile.Samples)*time.Millisecond > took {
		t.Fatalf("queryLike %v on CPU in %v, %d ms in all in %v", asking, queries, profile.Samples, took)
	}

	// Linux has a timer per thread: every sample the rate asks for, one a millisecond, nothing to estimate.
	if (processCPUTime == nil) != (profile.EstimatedFrom == 0) {
		t.Fatalf("estimated from %d samples on %s", profile.EstimatedFrom, runtime.GOOS)
	}
}

func TestWeighingGivesSamplesAtTheKernelsDoorAPeriodEach(t *testing.T) {
	at := func(function string) []cpuFrame { return []cpuFrame{{function: function}, {function: "app.handler"}} }
	parsed := &cpuProfile{periodNs: 1_000_000, samples: []cpuSample{
		{stack: at("syscall.rawsyscalln"), count: 6},
		{stack: at("runtime.kevent"), count: 2},
		{stack: at("app.loop"), count: 4},
	}}

	weigh(parsed, 48*time.Millisecond)

	weights := []float64{parsed.samples[0].weight, parsed.samples[1].weight, parsed.samples[2].weight}
	if weights[0] != 6 || weights[1] != 2 || weights[2] != 40 {
		t.Fatalf("weights %v", weights)
	}
}

func TestWeighingNeverGivesASampleLessThanAPeriod(t *testing.T) {
	parsed := &cpuProfile{periodNs: 1_000_000, samples: []cpuSample{
		{stack: []cpuFrame{{function: "syscall.syscall6"}}, count: 10},
		{stack: []cpuFrame{{function: "app.loop"}}, count: 5},
	}}

	weigh(parsed, 8*time.Millisecond)

	if parsed.samples[0].weight != 10 || parsed.samples[1].weight != 5 {
		t.Fatalf("weights %v %v", parsed.samples[0].weight, parsed.samples[1].weight)
	}
}

func TestAnEstimatedProfileCountsWholePeriodsAddingUp(t *testing.T) {
	stacks := []*profileStack{{weight: 2.6}, {weight: 1.3}, {weight: 0.4}, {weight: 0.2}}

	total := estimate(stacks)

	counts := []int64{stacks[0].Count, stacks[1].Count, stacks[2].Count, stacks[3].Count}
	if total != 5 || counts[0] != 3 || counts[1] != 1 || counts[2] != 1 || counts[3] != 0 {
		t.Fatalf("%d in all: %v", total, counts)
	}
}
