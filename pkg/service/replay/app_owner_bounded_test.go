package replay

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/internal/testnet"
	"go.keploy.io/server/v3/pkg"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// embedderInstr is an embedder's Instrumentation in k8s-proxy's shape: its
// Run runs no application and only waits for its context to end, and it does
// not say that it runs the application's process (AppProcessRunner).
type embedderInstr struct{ Instrumentation }

func (embedderInstr) Run(ctx context.Context, _ models.RunOptions) models.AppError {
	<-ctx.Done()
	return models.AppError{AppErrorType: models.ErrCtxCanceled, Err: ctx.Err()}
}

// embedderSaysNoInstr says that its Run does not run the application's
// process.
type embedderSaysNoInstr struct{ embedderInstr }

func (embedderSaysNoInstr) RunsAppProcess() bool { return false }

// probeMarksHooks records, for each test request, whether its context says
// the application's exit ends it, and whether its address may be probed.
type probeMarksHooks struct {
	prHooks
	mu     sync.Mutex
	marks  []bool
	probes []bool
}

func (h *probeMarksHooks) SimulateRequest(ctx context.Context, tc *models.TestCase, set string) (interface{}, error) {
	h.mu.Lock()
	h.marks = append(h.marks, pkg.AppExitEnds(ctx))
	h.probes = append(h.probes, pkg.AddressProbeAllowed(ctx))
	h.mu.Unlock()
	return h.prHooks.SimulateRequest(ctx, tc, set)
}

// Only an instrumentation that says its Run runs the application's process
// marks the set's test requests as ended by the application's exit: an
// embedder's command (k8s-proxy sets "replay") runs no application whose exit
// would end a wait. Where probing the address is disabled, the marked
// requests may not probe it.
func TestOnlyAnInstrumentationThatRunsTheAppEndsItsTestsWaitsByTheExit(t *testing.T) {
	for _, c := range []struct {
		name      string
		embedder  func(Instrumentation) Instrumentation
		noProbe   bool
		wantMark  bool
		wantProbe bool
	}{
		{name: "runs the app", wantMark: true, wantProbe: true},
		{name: "runs the app, probing disabled", noProbe: true, wantMark: true, wantProbe: false},
		{name: "embedder", embedder: func(i Instrumentation) Instrumentation { return embedderInstr{i} }, wantProbe: true},
		{name: "embedder that says no", embedder: func(i Instrumentation) Instrumentation { return embedderSaysNoInstr{embedderInstr{i}} }, noProbe: true, wantProbe: true},
	} {
		t.Run(c.name, func(t *testing.T) {
			h := newPartialRunHarness(t, 2, 0)
			h.replayer.config.Command = "replay"
			h.replayer.config.Test.DisableAppReadyProbe = c.noProbe
			if c.embedder != nil {
				h.replayer.instrumentation = c.embedder(h.instr)
			}
			hooks := &probeMarksHooks{}
			h.replayer.hookImpl = hooks
			h.run(t)
			if len(hooks.marks) != 2 {
				t.Fatalf("%d test requests sent, want 2", len(hooks.marks))
			}
			for i := range hooks.marks {
				if hooks.marks[i] != c.wantMark {
					t.Errorf("request %d's context says the app's exit ends it: %v, want %v", i+1, hooks.marks[i], c.wantMark)
				}
				if hooks.probes[i] != c.wantProbe {
					t.Errorf("request %d's context may probe the app's address: %v, want %v", i+1, hooks.probes[i], c.wantProbe)
				}
			}
		})
	}
}

// k8s-proxy's shape: the placeholder command "replay", an instrumentation
// whose Run waits only for its context, probing disabled, and the tests'
// address refusing (the port-forward is down). The set fails on the bounded
// re-sends, as before, instead of waiting for an exit that never comes.
func TestAnEmbeddersRefusedTestsFailOnTheBoundedReSends(t *testing.T) {
	refusing := net.JoinHostPort("127.0.0.1", strconv.Itoa(testnet.SharedPort(t)))
	h := newPartialRunHarness(t, 2, 0)
	h.replayer.config.Command = "replay"
	h.replayer.config.Test.DisableAppReadyProbe = true
	h.replayer.instrumentation = embedderInstr{h.instr}
	h.replayer.hookImpl = newHooks(h.replayer.logger, h.replayer.config, h.replayer.instrumentation, false)
	for _, tc := range h.cases {
		tc.HTTPReq.URL = "http://" + refusing + "/x"
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	type result struct {
		status models.TestSetStatus
		err    error
	}
	done := make(chan result, 1)
	go func() {
		status, err := h.replayer.RunTestSet(ctx, "test-set-0", "test-run-0", false)
		done <- result{status, err}
	}()
	select {
	case r := <-done:
		if r.status != models.TestSetStatusFailed {
			t.Fatalf("status %q (%v), want %q: each refused test fails on the bounded re-sends", r.status, r.err, models.TestSetStatusFailed)
		}
	// The bounded re-sends take under a second on Linux, about 9 s per
	// test on Windows.
	case <-time.After(prTimeout):
		t.Fatal("the set still runs: it waits for an exit that an embedder's run never has")
	}
	if n := len(h.report.results); n != 2 {
		t.Errorf("%d results, want 2", n)
	}
}

// waitForAppReadyBounded runs waitForAppReady on ctx and fails the row when
// it has not returned within prTimeout.
func waitForAppReadyBounded(t *testing.T, ctx context.Context, logger *zap.Logger, cfg *config.Config) bool {
	t.Helper()
	done := make(chan bool, 1)
	go func() { done <- waitForAppReady(ctx, logger, cfg, httpProbeTarget{}) }()
	select {
	case ready := <-done:
		return ready
	case <-time.After(prTimeout):
		t.Fatal("the wait before the first test still runs for an application keploy does not run")
		return false
	}
}

// For an application whose process keploy does not run, the wait before the
// first test stays bounded: an address gate that never accepts warns and
// fires the tests past --health-poll-timeout, and a health URL that never
// answers 2xx falls back to the fixed --delay.
func TestTheWaitBeforeTheFirstTestOfAnAppKeployDoesNotRunIsBounded(t *testing.T) {
	refusing := net.JoinHostPort("127.0.0.1", strconv.Itoa(testnet.SharedPort(t)))
	t.Run("app ready probe address", func(t *testing.T) {
		core, logs := observer.New(zapcore.InfoLevel)
		cfg := &config.Config{}
		cfg.Test.AppReadyProbeAddr = refusing
		cfg.Test.HealthPollTimeout = 500 * time.Millisecond
		if !waitForAppReadyBounded(t, context.Background(), zap.New(core), cfg) {
			t.Fatal("not ready, want the tests fired past the ceiling")
		}
		if n := logs.FilterMessageSnippet("did not accept a connection within the ceiling").Len(); n != 1 {
			t.Errorf("said %d times that the address did not accept within the ceiling, want once", n)
		}
	})
	t.Run("health URL", func(t *testing.T) {
		core, logs := observer.New(zapcore.InfoLevel)
		cfg := &config.Config{}
		cfg.Test.HealthURL = "http://" + refusing + "/health"
		cfg.Test.HealthPollTimeout = 500 * time.Millisecond
		if !waitForAppReadyBounded(t, context.Background(), zap.New(core), cfg) {
			t.Fatal("not ready, want the tests fired after the fixed delay")
		}
		if n := logs.FilterMessageSnippet("falling back to fixed delay").Len(); n != 1 {
			t.Errorf("said %d times that it falls back to the fixed delay, want once", n)
		}
	})
}

type stillCall struct{ what, addr string }

// recordStillWaiting replaces the line a wait with no time limit says every
// so often with one that records what it was asked to say, and sends each
// ask on the returned channel.
func recordStillWaiting(t *testing.T) <-chan stillCall {
	calls := make(chan stillCall, 16)
	was := sayStillWaiting
	sayStillWaiting = func(_ context.Context, _ *zap.Logger, what, addr string, _ time.Time) func() {
		calls <- stillCall{what, addr}
		return func() {}
	}
	t.Cleanup(func() { sayStillWaiting = was })
	return calls
}

// The health URL's wait, with probing the address disabled, for an
// application keploy runs, has no time limit, and says every so often that
// it still waits, naming the URL and the address its request dials.
func TestAHealthURLWaitWithProbingDisabledSaysItStillWaits(t *testing.T) {
	calls := recordStillWaiting(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()
	cfg := &config.Config{}
	cfg.Test.HealthURL = srv.URL + "/health"
	cfg.Test.HealthPollTimeout = 100 * time.Millisecond
	cfg.Test.DisableAppReadyProbe = true
	ctx, cancel := context.WithCancel(pkg.EndsWithApp(context.Background()))
	defer cancel()
	done := make(chan bool, 1)
	go func() { done <- waitForAppReady(ctx, zap.NewNop(), cfg, httpProbeTarget{}) }()
	var call stillCall
	select {
	case call = <-calls:
	case ready := <-done:
		t.Fatalf("the wait ended (%v) without saying it still waits", ready)
	case <-time.After(prTimeout):
		t.Fatal("the wait never said it still waits")
	}
	if want := "answer 2xx at " + srv.URL + "/health"; call.what != want {
		t.Errorf("it says it waits for the application to %q, want %q", call.what, want)
	}
	if want := strings.TrimPrefix(srv.URL, "http://"); call.addr != want {
		t.Errorf("it says it waits on %q, want the health URL's address %q", call.addr, want)
	}
	select {
	case ready := <-done:
		t.Fatalf("the wait ended (%v) while the health URL answered 503 and the application ran", ready)
	default:
	}
	cancel()
	select {
	case ready := <-done:
		if ready {
			t.Error("ready at the stop, want not ready")
		}
	case <-time.After(prTimeout):
		t.Fatal("the wait did not end at the stop")
	}
}

// The reset re-send's wait for an application keploy runs says every so often
// that it still waits, naming the address the request dials.
func TestAResetRequestsWaitSaysItStillWaits(t *testing.T) {
	calls := recordStillWaiting(t)
	addr, serving := resettingApp(t)
	r := &Replayer{config: &config.Config{Command: "node app.js"}, logger: zap.NewNop()}
	ctx, cancel := context.WithCancel(pkg.EndsWithApp(context.Background()))
	defer cancel()
	said := false
	done := make(chan struct{})
	go func() {
		defer close(done)
		r.waitForResetResendReady(ctx, resetTestCase(addr), "test-set-0", &said)
	}()
	var call stillCall
	select {
	case call = <-calls:
	case <-done:
		t.Fatal("the wait ended without saying it still waits")
	case <-time.After(prTimeout):
		t.Fatal("the wait never said it still waits")
	}
	_, port, _ := net.SplitHostPort(addr)
	if call.what != "answer HTTP" || call.addr != "localhost:"+port {
		t.Errorf("it says it waits for the application to %q on %q, want %q on %q", call.what, call.addr, "answer HTTP", "localhost:"+port)
	}
	serving.Store(true)
	select {
	case <-done:
	case <-time.After(prTimeout):
		t.Fatal("the wait did not end once the application answered")
	}
}
