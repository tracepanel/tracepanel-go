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

// fakeCPU counts what the profiler does to the CPU profile; stop may be held, as macOS holds it up to 100 ms.
type fakeCPU struct {
	mu            sync.Mutex
	starts, stops int
	hold          chan struct{}
}

func (f *fakeCPU) install(p *profiler) {
	p.startCPU = func(*bytes.Buffer) error { f.mu.Lock(); f.starts++; f.mu.Unlock(); return nil }
	p.stopCPU = func() {
		f.mu.Lock()
		f.stops++
		hold := f.hold
		f.mu.Unlock()
		if hold != nil {
			<-hold
		}
	}
}

func (f *fakeCPU) counts() (int, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.starts, f.stops
}

type ended struct {
	doc *profileDoc
	gap time.Duration
}

func collect(got chan ended) func(*profileDoc, time.Duration) {
	return func(doc *profileDoc, gap time.Duration) { got <- ended{doc, gap} }
}

func TestBackToBackTracesAreSampledWithoutAStop(t *testing.T) {
	p := newProfiler()
	p.cutIdle = time.Hour
	cpu := &fakeCPU{}
	cpu.install(p)
	got := make(chan ended, 3)

	// One trace after the other, and a third while the second runs: the profile runs on, never stopped between.
	_, first := p.start(context.Background(), "first")
	p.end(first, collect(got))
	_, second := p.start(context.Background(), "second")
	_, third := p.start(context.Background(), "third")
	p.end(second, collect(got))
	p.end(third, collect(got))

	if starts, stops := cpu.counts(); starts != 1 || stops != 0 {
		t.Fatalf("starts %d, stops %d", starts, stops)
	}

	// Flush cuts at once: one stop hands every ended trace its profile, none with a gap.
	p.wait(time.Second)

	for range 3 {
		if e := <-got; e.doc == nil || e.gap != 0 {
			t.Fatalf("ended %+v", e)
		}
	}
	if starts, stops := cpu.counts(); starts != 1 || stops != 1 || p.running || len(p.traces) != 0 {
		t.Fatalf("starts %d, stops %d, running %v, traces %v", starts, stops, p.running, p.traces)
	}
}

func TestTheCutComesOnceNoTraceRanForAWhile(t *testing.T) {
	p := newProfiler()
	p.cutIdle = 20 * time.Millisecond
	cpu := &fakeCPU{}
	cpu.install(p)
	got := make(chan ended, 1)

	_, first := p.start(context.Background(), "first")
	p.end(first, collect(got))

	if e := <-got; e.doc == nil {
		t.Fatalf("ended %+v", e)
	}
	if starts, stops := cpu.counts(); starts != 1 || stops != 1 {
		t.Fatalf("starts %d, stops %d", starts, stops)
	}
}

func TestUnderSteadyLoadAnEndedTraceWaitsAtMostCutWait(t *testing.T) {
	p := newProfiler()
	p.cutIdle, p.cutWait = 10*time.Millisecond, 50*time.Millisecond
	cpu := &fakeCPU{}
	cpu.install(p)
	got := make(chan ended, 1)
	_, long := p.start(context.Background(), "long")
	_, short := p.start(context.Background(), "short")

	began := time.Now()
	p.end(short, collect(got))
	e := <-got
	waited := time.Since(began)

	// Cut while the long one runs, which goes on being profiled.
	if e.doc == nil || waited < 50*time.Millisecond {
		t.Fatalf("ended %+v after %v", e, waited)
	}
	if starts, stops := cpu.counts(); starts != 2 || stops != 1 || !p.running {
		t.Fatalf("starts %d, stops %d, running %v", starts, stops, p.running)
	}
	p.end(long, func(*profileDoc, time.Duration) {})
	p.wait(time.Second)
}

func TestATraceRunningWhileACutStopsTheProfilerSaysHowLong(t *testing.T) {
	p := newProfiler()
	p.cutIdle = time.Hour
	cpu := &fakeCPU{hold: make(chan struct{})}
	cpu.install(p)
	got := make(chan ended, 2)
	_, first := p.start(context.Background(), "first")
	p.end(first, collect(got))

	// The cut stops the profiler (held, as macOS holds it), and a trace starts meanwhile.
	go p.wait(time.Second)
	for _, stops := cpu.counts(); stops == 0; _, stops = cpu.counts() {
		time.Sleep(time.Millisecond)
	}
	_, second := p.start(context.Background(), "second")
	time.Sleep(30 * time.Millisecond)
	close(cpu.hold)
	<-got
	p.end(second, collect(got))
	p.wait(time.Second)

	if e := <-got; e.doc == nil || e.gap < 30*time.Millisecond || e.gap > time.Second {
		t.Fatalf("second %+v", e)
	}
	if starts, stops := cpu.counts(); starts != 2 || stops != 2 {
		t.Fatalf("starts %d, stops %d", starts, stops)
	}
}

func TestBackToBackProfiledRequestsLoseNoSamples(t *testing.T) {
	recorded := record(t)
	recorded.t.profiler = newProfiler()
	var mu sync.Mutex
	stops := 0
	recorded.t.profiler.stopCPU = func() {
		mu.Lock()
		stops++
		mu.Unlock()
		pprof.StopCPUProfile()
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /report", func(w http.ResponseWriter, r *http.Request) {
		burnProfiled(150 * time.Millisecond)
	})
	handler := recorded.t.middleware(mux)

	for range 3 {
		request := httptest.NewRequest(http.MethodGet, "/report", nil)
		request.Header.Set("X-Tracepanel-Profile", "1")
		handler.ServeHTTP(httptest.NewRecorder(), request)
	}
	mu.Lock()
	stopsWhileRunning := stops
	mu.Unlock()
	Flush(5 * time.Second)

	// The profiler ran through the three: each has its samples from its start, none a gap.
	if stopsWhileRunning != 0 {
		t.Fatalf("the profiler stopped %d times between the requests", stopsWhileRunning)
	}
	for _, doc := range recorded.wait(t, 3) {
		if doc.Profile == nil || doc.Profile.Samples == 0 || doc.Attrs["tracepanel.profile_gap_ms"] != nil {
			t.Fatalf("%s: profile %+v, attrs %v", doc.Name, doc.Profile, doc.Attrs)
		}
	}
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

func TestAProfiledTraceSaysWhyItIs(t *testing.T) {
	cases := map[string]struct {
		header, cookie string
		all            bool
		want           any
	}{
		"the header":           {header: "1", want: "header"},
		"the header, memory":   {header: "memory", want: "header"},
		"the panel's cookie":   {cookie: "1", want: "cookie"},
		"the setting":          {all: true, want: "setting"},
		"asked, with settings": {cookie: "1", all: true, want: "cookie"},
		"nothing":              {want: nil},
		"an ask it can't read": {header: "yes please", want: nil},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			recorded := record(t)
			recorded.t.profiler = newProfiler()
			recorded.t.profileAll = c.all
			request := httptest.NewRequest(http.MethodGet, "/", nil)
			if c.header != "" {
				request.Header.Set("X-Tracepanel-Profile", c.header)
			}
			if c.cookie != "" {
				request.AddCookie(&http.Cookie{Name: "tracepanel_profile", Value: c.cookie})
			}

			recorded.t.middleware(http.NotFoundHandler()).ServeHTTP(httptest.NewRecorder(), request)
			Flush(time.Second)

			doc := recorded.wait(t, 1)[0]
			if doc.Attrs["profile.reason"] != c.want || (doc.Profile != nil) != (c.want != nil) {
				t.Fatalf("profile.reason %v, profile %+v", doc.Attrs["profile.reason"], doc.Profile)
			}
		})
	}
}

func TestATaskOfAProfiledTraceSaysItsParentAskedForIt(t *testing.T) {
	recorded := record(t)
	recorded.t.profiler = newProfiler()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /signup", func(w http.ResponseWriter, r *http.Request) {
		Go(r.Context(), "send welcome email", func(ctx context.Context) error { return nil })
	})
	request := httptest.NewRequest(http.MethodGet, "/signup", nil)
	request.Header.Set("X-Tracepanel-Profile", "1")

	recorded.t.middleware(mux).ServeHTTP(httptest.NewRecorder(), request)
	Flush(time.Second)

	reasons := map[string]any{}
	for _, doc := range recorded.wait(t, 2) {
		reasons[doc.Kind] = doc.Attrs["profile.reason"]
	}
	if !equalJSON(reasons, map[string]any{"http": "header", "task": "parent"}) {
		t.Fatalf("reasons %v", reasons)
	}
}

func TestCodeProfilesItsTraceFromWhereItAsks(t *testing.T) {
	recorded := record(t)
	recorded.t.profiler = newProfiler()
	var asked string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked = r.Header.Get("X-Tracepanel-Profile")
	}))
	defer upstream.Close()
	client := &http.Client{Transport: WrapTransport(http.DefaultTransport)}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /report", func(w http.ResponseWriter, r *http.Request) {
		ctx := Profile(r.Context())
		// Asked twice: profiled once.
		ctx = Profile(ctx)
		burnProfiled(100 * time.Millisecond)
		request, _ := http.NewRequestWithContext(ctx, http.MethodGet, upstream.URL, nil)
		if response, err := client.Do(request); err == nil {
			response.Body.Close()
		}
		Go(ctx, "mail the report", func(context.Context) error { return nil })
	})

	recorded.t.middleware(mux).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/report", nil))
	Flush(time.Second)

	reasons := map[string]any{}
	for _, doc := range recorded.wait(t, 2) {
		reasons[doc.Kind] = doc.Attrs["profile.reason"]
		if doc.Kind == "http" && (doc.Profile == nil || !functions(doc.Profile)["burnProfiled"]) {
			t.Fatalf("profile %+v", doc.Profile)
		}
	}
	if !equalJSON(reasons, map[string]any{"http": "code", "task": "parent"}) || asked != "1" {
		t.Fatalf("reasons %v, the call asked %q", reasons, asked)
	}
	// Outside a trace: nothing to profile.
	if ctx := context.Background(); Profile(ctx) != ctx {
		t.Fatal("a context without a trace changed")
	}
}
