package tracepanel

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"reflect"
	"runtime"
	"runtime/pprof"
	"sync"
	"testing"
	"time"

	"github.com/tracepanel/tracepanel-go/internal/hook"
)

// burnProfiled and burnOther keep a CPU busy for d: the profile names the one its trace ran.
func burnProfiled(d time.Duration) (sum float64) {
	for deadline := time.Now().Add(d); time.Now().Before(deadline); {
		for i := range 10_000 {
			sum += float64(i) * 1.0001
		}
	}
	return sum
}

func burnOther(d time.Duration) (sum float64) {
	for deadline := time.Now().Add(d); time.Now().Before(deadline); {
		for i := range 10_000 {
			sum -= float64(i) * 0.9999
		}
	}
	return sum
}

// functions are the functions a profile's stacks name, by their short name.
func functions(profile *profileDoc) map[string]bool {
	named := map[string]bool{}
	for _, stack := range profile.Stacks {
		for _, frame := range stack.Frames {
			named[profile.Frames[frame].Function] = true
		}
	}
	return named
}

func TestAProfiledRequestHasItsOwnSamplesOnly(t *testing.T) {
	recorded := record(t)
	recorded.t.profiler = newProfiler()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /report", func(w http.ResponseWriter, r *http.Request) {
		// Work on a goroutine of its own is the request's too: it carries the request's label.
		var wg sync.WaitGroup
		wg.Go(func() { burnProfiled(150 * time.Millisecond) })
		burnProfiled(150 * time.Millisecond)
		wg.Wait()
		_, _ = w.Write([]byte("done"))
	})
	mux.HandleFunc("GET /other", func(w http.ResponseWriter, r *http.Request) {
		burnOther(300 * time.Millisecond)
	})
	handler := recorded.t.middleware(mux)

	// Another request runs at the same time, not asked to be profiled: none of its samples are the report's.
	var wg sync.WaitGroup
	wg.Go(func() { handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/other", nil)) })
	request := httptest.NewRequest(http.MethodGet, "/report", nil)
	request.Header.Set("X-Tracepanel-Profile", "1")
	handler.ServeHTTP(httptest.NewRecorder(), request)
	wg.Wait()

	var report, other *traceDoc
	for _, doc := range recorded.wait(t, 2) {
		switch doc.Name {
		case "GET /report":
			report = doc
		case "GET /other":
			other = doc
		}
	}
	if other == nil || other.Profile != nil {
		t.Fatalf("the request not asked has a profile: %+v", other)
	}
	profile := report.Profile
	if profile == nil || profile.IntervalUs != 10_000 || profile.Samples < 5 || profile.Dropped != 0 {
		t.Fatalf("profile: %+v", profile)
	}
	named := functions(profile)
	if !named["burnProfiled"] || named["burnOther"] {
		t.Fatalf("functions: %v", named)
	}
	var counted int64
	for _, stack := range profile.Stacks {
		counted += stack.Count
		if len(stack.Frames) == 0 {
			t.Fatalf("an empty stack: %+v", profile)
		}
	}
	if counted != profile.Samples {
		t.Fatalf("stacks count %d of %d samples", counted, profile.Samples)
	}
	burn := runtime.FuncForPC(reflect.ValueOf(burnProfiled).Pointer())
	_, declared := burn.FileLine(burn.Entry())
	for _, frame := range profile.Frames {
		if frame.Function == "burnProfiled" && (!frame.InApp || frame.File != "profile_test.go" || frame.Line != declared || *frame.Module != modulePath) {
			t.Fatalf("frame: %+v", frame)
		}
		if *frame.Module == modulePath && frame.Function == "(*tracepanel).middleware.func1" {
			t.Fatalf("Tracepanel's own frame: %+v", frame)
		}
	}
}

func TestATaskOfAProfiledRequestIsProfiled(t *testing.T) {
	recorded := record(t)
	recorded.t.profiler = newProfiler()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /signup", func(w http.ResponseWriter, r *http.Request) {
		Go(r.Context(), "send welcome email", func(ctx context.Context) error {
			burnProfiled(300 * time.Millisecond)
			return nil
		})
	})
	request := httptest.NewRequest(http.MethodGet, "/signup", nil)
	request.AddCookie(&http.Cookie{Name: "tracepanel_profile", Value: "1"})
	recorded.t.middleware(mux).ServeHTTP(httptest.NewRecorder(), request)

	docs := recorded.wait(t, 2)
	for _, doc := range docs {
		if doc.Profile == nil {
			t.Fatalf("%s has no profile", doc.Name)
		}
		if doc.Kind == "task" && !functions(doc.Profile)["burnProfiled"] {
			t.Fatalf("task profile: %+v", doc.Profile)
		}
		if doc.Kind == "http" && functions(doc.Profile)["burnProfiled"] {
			t.Fatalf("the task's samples are the request's: %v", functions(doc.Profile))
		}
	}
}

func TestEveryTraceIsProfiledWithProfileAll(t *testing.T) {
	recorded := record(t)
	recorded.t.profiler = newProfiler()
	recorded.t.profileAll = true

	_ = Command(context.Background(), "nightly report", func(context.Context) error { return nil })

	doc := recorded.wait(t, 1)[0]
	// Too quick for a sample: a profile that says so.
	if doc.Profile == nil || doc.Profile.Samples != 0 || len(doc.Profile.Stacks) != 0 {
		t.Fatalf("profile: %+v", doc.Profile)
	}
}

func TestNoProfileWhileTheAppRunsItsOwn(t *testing.T) {
	recorded := record(t)
	recorded.t.profiler = newProfiler()
	var own bytes.Buffer
	if err := pprof.StartCPUProfile(&own); err != nil {
		t.Fatal(err)
	}
	defer pprof.StopCPUProfile()
	request := httptest.NewRequest(http.MethodGet, "/", nil)
	request.Header.Set("X-Tracepanel-Profile", "1")

	recorded.t.middleware(http.NotFoundHandler()).ServeHTTP(httptest.NewRecorder(), request)

	if doc := recorded.wait(t, 1)[0]; doc.Profile != nil {
		t.Fatalf("profile: %+v", doc.Profile)
	}
}

func TestSamplesGoToTheTraceTheirLabelNames(t *testing.T) {
	p := newProfiler()
	report := &traceProfile{id: "report", stacks: map[string]*profileStack{}, index: map[cpuFrame]int{}}
	other := &traceProfile{id: "other", stacks: map[string]*profileStack{}, index: map[cpuFrame]int{}}
	p.traces = map[string]*traceProfile{"report": report, "other": other}
	handler := cpuFrame{function: "example.com/shop.listBooks", file: "/src/shop/books.go", line: 12, start: 10}
	serve := cpuFrame{function: "net/http.(*conn).serve", file: "/go/src/net/http/server.go", line: 2100, start: 2000}
	own := cpuFrame{function: modulePath + ".(*tracepanel).middleware.func1", file: "/x/middleware.go", line: 60, start: 50}

	module := "example.com/shop"
	appModule.Store(&module)

	p.share(&cpuProfile{periodNs: 10_000_000, samples: []cpuSample{
		{stack: []cpuFrame{handler, own, serve, {function: "runtime.goexit"}}, count: 3, trace: "report"},
		{stack: []cpuFrame{{function: "example.com/shop.render", file: "/src/shop/books.go", start: 30}, handler, serve}, count: 1, trace: "report"},
		{stack: []cpuFrame{handler, serve}, count: 2, trace: "gone"},
		{stack: []cpuFrame{{function: "runtime/pprof.lostProfileEvent"}}, count: 4},
		{stack: []cpuFrame{{function: "runtime.gcBgMarkWorker"}}, count: 5},
	}})

	got := report.document(&cpuProfile{periodNs: 10_000_000})
	shop, http := "example.com/shop", "net/http"
	want := &profileDoc{
		IntervalUs: 10_000, Samples: 4, Dropped: 4,
		Frames: []profileFrame{
			{Function: "(*conn).serve", Module: &http, File: "net/http/server.go", Line: 2000},
			{Function: "listBooks", Module: &shop, File: "/src/shop/books.go", Line: 10, InApp: true},
			{Function: "render", Module: &shop, File: "/src/shop/books.go", Line: 30, InApp: true},
		},
		Stacks: []profileStack{{Frames: []int{0, 1}, Count: 3}, {Frames: []int{0, 1, 2}, Count: 1}},
	}
	if !equalJSON(got, want) {
		t.Fatalf("profile %+v", got)
	}
	if other.dropped != 4 || other.samples != 0 {
		t.Fatalf("other: %+v", other)
	}
}

func TestACutHandsEveryEndedTraceItsProfile(t *testing.T) {
	p := newProfiler()
	starts := 0
	p.startCPU = func(*bytes.Buffer) error { starts++; return nil }
	p.stopCPU = func() {}
	_, first := p.start(context.Background(), "first")
	_, second := p.start(context.Background(), "second")
	got := make(chan *profileDoc, 2)

	p.end(first, func(doc *profileDoc) { got <- doc })
	if doc := <-got; doc == nil || doc.Samples != 0 {
		t.Fatalf("first: %+v", doc)
	}
	p.end(second, func(doc *profileDoc) { got <- doc })
	<-got
	p.wait(time.Second)

	// Started for the first, again for the second once the first's cut stopped it, and not after.
	if p.mu.Lock(); starts != 2 || p.running || len(p.traces) != 0 {
		t.Fatalf("starts %d, running %v, traces %v", starts, p.running, p.traces)
	}
	p.mu.Unlock()
}

func TestTheHelloSaysWhatTheAppDoes(t *testing.T) {
	cases := map[string]struct {
		env  map[string]string
		want []string
	}{
		"default":    {map[string]string{}, []string{"request", "profile"}},
		"everything": {map[string]string{"TRACEPANEL_PROFILE": "1"}, []string{"request", "profile", "profile_all"}},
		"nothing":    {map[string]string{"TRACEPANEL_PROFILE_ENABLED": "0", "TRACEPANEL_CAPTURE_REQUEST": "0"}, []string{}},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			for _, key := range []string{"TRACEPANEL_PROFILE", "TRACEPANEL_PROFILE_ENABLED", "TRACEPANEL_CAPTURE_REQUEST"} {
				t.Setenv(key, c.env[key])
			}
			got := newTracepanel(withDefaults(Config{Service: "shop", StoreDir: t.TempDir()})).sender.capabilities

			if !equalJSON(append([]string{}, got...), c.want) {
				t.Fatalf("capabilities %v", got)
			}
		})
	}
}

func TestAnIntegrationsTraceIsProfiledWithProfileAll(t *testing.T) {
	recorded := record(t)
	recorded.t.profiler = newProfiler()
	recorded.t.profileAll = true

	trace := hook.Start("rpc", "rpc", "shop.Books/List", map[string]any{})
	_ = trace.Context(context.Background())
	trace.Finish()

	if doc := recorded.wait(t, 1)[0]; doc.Profile == nil {
		t.Fatal("no profile")
	}
}
