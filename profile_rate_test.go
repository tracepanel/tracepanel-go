package tracepanel

import (
	"testing"
	"time"
)

func TestTheProfilerSamplesEveryMillisecondUnlessSet(t *testing.T) {
	cases := map[time.Duration]int{
		0:                       1000,
		time.Millisecond:        1000,
		5 * time.Millisecond:    200,
		10 * time.Millisecond:   100,
		time.Microsecond:        10_000,
		3 * time.Second:         1,
		-time.Millisecond:       1000,
		1500 * time.Microsecond: 666,
	}
	for interval, want := range cases {
		if got := profileHz(interval); got != want {
			t.Errorf("profileHz(%v) = %d, want %d", interval, got, want)
		}
	}

	if got := newProfiler().intervalUs; got != 1000 {
		t.Fatalf("intervalUs: %d", got)
	}

	if got := newProfiler(10 * time.Millisecond).intervalUs; got != 10_000 {
		t.Fatalf("intervalUs: %d", got)
	}
}

func TestTheProfileIntervalSetting(t *testing.T) {
	cases := map[string]time.Duration{"": time.Millisecond, "5": 5 * time.Millisecond, "0": time.Millisecond, "x": time.Millisecond}
	for value, want := range cases {
		t.Setenv("TRACEPANEL_PROFILE_INTERVAL_MS", value)
		if got := withDefaults(Config{Service: "shop"}).ProfileInterval; got != want {
			t.Errorf("TRACEPANEL_PROFILE_INTERVAL_MS=%q: %v, want %v", value, got, want)
		}
	}

	t.Setenv("TRACEPANEL_PROFILE_INTERVAL_MS", "5")
	if got := withDefaults(Config{Service: "shop", ProfileInterval: 2 * time.Millisecond}).ProfileInterval; got != 2*time.Millisecond {
		t.Fatalf("Config.ProfileInterval: %v", got)
	}
}

func TestAProfileWhoseSamplesDontSayHasTheProfilersInterval(t *testing.T) {
	tp := &traceProfile{id: "quick", stacks: map[string]*profileStack{}, index: map[cpuFrame]int{}, intervalUs: 2000}

	if got := tp.document(&cpuProfile{}).IntervalUs; got != 2000 {
		t.Fatalf("interval: %d", got)
	}

	if got := tp.document(&cpuProfile{periodNs: 1_000_000}).IntervalUs; got != 1000 {
		t.Fatalf("interval: %d", got)
	}
}

func TestOnlyTheRateWarningIsTakenFromStderr(t *testing.T) {
	written := []byte("before\n" + cpuRateWarning + "after\n" + cpuRateWarning)

	if got := string(withoutRateWarning(written)); got != "before\nafter\n" {
		t.Fatalf("stderr: %q", got)
	}
}
