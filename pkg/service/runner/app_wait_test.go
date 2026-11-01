package integration

import (
	"context"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

// heldListener is an app whose start is held: it resets the first hold
// connections it accepts, as the port compose publishes does while compose
// waits on a dependency's health before it starts the app behind it, and
// hands the rest to the app. onRefuse, if set, is told each refusal's count.
type heldListener struct {
	net.Listener
	hold     int64
	refused  atomic.Int64
	onRefuse func(n int64)
}

func (l *heldListener) Accept() (net.Conn, error) {
	for {
		c, err := l.Listener.Accept()
		if err != nil {
			return nil, err
		}
		if l.refused.Load() >= l.hold {
			return c, nil
		}
		if tc, ok := c.(*net.TCPConn); ok {
			_ = tc.SetLinger(0) // a reset, as the CI's "connection reset by peer"
		}
		_ = c.Close()
		n := l.refused.Add(1)
		if l.onRefuse != nil {
			l.onRefuse(n)
		}
	}
}

// heldApp starts an app held for hold connections, and returns its URL, its
// listener, and the count of the test requests it served.
func heldApp(t *testing.T, hold int64, onRefuse func(n int64)) (string, *heldListener, *atomic.Int64) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	hl := &heldListener{Listener: ln, hold: hold, onRefuse: onRefuse}
	var served atomic.Int64
	srv := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodGet && r.URL.Path == "/health" {
				served.Add(1)
			}
			w.Header()["Date"] = nil // no header the recording could not hold
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("ok"))
		}),
		ReadHeaderTimeout: time.Minute,
	}
	go func() { _ = srv.Serve(hl) }()
	t.Cleanup(func() { _ = srv.Close() })
	return "http://" + ln.Addr().String(), hl, &served
}

// oneCaseDB holds the one recorded test case the rows run.
type oneCaseDB struct{ tc *models.TestCase }

func (d oneCaseDB) GetTestCase(context.Context, string, string) (*models.TestCase, error) {
	tc := *d.tc
	return &tc, nil
}

// appWaitInstr is an agent that accepts every call and has served no mock.
type appWaitInstr struct{ setupInstr }

func (*appWaitInstr) GetConsumedMocks(context.Context) ([]models.MockState, error) {
	return nil, nil
}

// appWaitRunner is a runner of one recorded GET /health, with probes paced
// at a millisecond and a budget that has run out before the first probe:
// a wait that keeps a budget ends at once, one that has none waits for the
// app.
func appWaitRunner(t *testing.T, logger *zap.Logger) *Runner {
	t.Helper()
	budget, tick, say := appWaitBudget, appProbeTick, appSayEvery
	appWaitBudget, appProbeTick, appSayEvery = time.Nanosecond, time.Millisecond, 2
	t.Cleanup(func() { appWaitBudget, appProbeTick, appSayEvery = budget, tick, say })
	r := &Runner{
		logger: logger,
		testCaseDB: oneCaseDB{tc: &models.TestCase{
			Name: "health",
			Kind: models.HTTP,
			HTTPReq: models.HTTPReq{
				Method:     models.Method(http.MethodGet),
				ProtoMajor: 1,
				ProtoMinor: 1,
				URL:        "http://localhost:8080/health",
				Header:     map[string]string{},
			},
			HTTPResp: models.HTTPResp{StatusCode: http.StatusOK, Body: "ok", Header: map[string]string{
				"Content-Type": "text/plain; charset=utf-8", "Content-Length": "2",
			}},
		}},
		instrumentation: &appWaitInstr{},
		mappingDB:       &stubMappingDB{mappings: map[string][]models.MockEntry{}},
		mockDB:          &skippingMockDB{},
	}
	t.Cleanup(func() { _ = r.Close() })
	return r
}

// An app whose start compose holds behind a slow dependency is waited for
// past the budget, when its caller gave the end of its run: the test runs
// once the app answers, against the app (enterprise atg-with-mocks pipelines
// 13105 and 13112 failed "health prelude" with "app not reachable: timed out
// waiting for app" 8 s before compose started the app). The wait says, while
// it is held, that it still waits and on what.
func TestAnAppHeldFromStartingIsWaitedForPastTheBudget(t *testing.T) {
	core, logs := observer.New(zap.InfoLevel)
	r := appWaitRunner(t, zap.New(core))
	appURL, app, served := heldApp(t, 5, nil)
	ended := make(chan struct{}) // the app's run goes on

	res := r.RunTest(context.Background(), RunTestOpts{TestSetID: "test-set-0", TestStepID: "health", ServiceURL: appURL, AppEnded: ended})

	if !res.Passed {
		t.Fatalf("the test did not pass against an app whose run goes on: error %q, diff %s", res.Error, res.Diff)
	}
	if got := app.refused.Load(); got != 5 {
		t.Fatalf("the wait went on after %d refused probes; want it to wait out all 5 of the app's held start", got)
	}
	if got := served.Load(); got != 1 {
		t.Fatalf("the app served the test's request %d times; want 1, once it answered", got)
	}
	said := logs.FilterMessageSnippet("still waiting for the app to answer").All()
	if len(said) == 0 {
		t.Fatal("the held wait never said it still waits")
	}
	if f, ok := said[0].ContextMap()["lastProbeError"]; !ok || f == "" {
		t.Fatalf("the line that says it still waits names no probe error: %v", said[0].ContextMap())
	}
}

// A caller that gave no end of the app's run keeps the budget: k8s-proxy and
// the sandbox's UI path, which did not start the app.
func TestWithoutTheAppsEndTheWaitKeepsItsBudget(t *testing.T) {
	r := appWaitRunner(t, zap.NewNop())
	appURL, _, served := heldApp(t, 5, nil)

	res := r.RunTest(context.Background(), RunTestOpts{TestSetID: "test-set-0", TestStepID: "health", ServiceURL: appURL})

	if !strings.Contains(res.Error, "app not reachable: timed out waiting for app") {
		t.Fatalf("error = %q; want the budget's", res.Error)
	}
	if served.Load() != 0 {
		t.Fatal("the test ran though its wait's budget had run out")
	}
}

// When the app's run ends before the app answered (compose gave up on a
// dependency that never came healthy, and came down), the wait ends then,
// and the test fails saying the app stopped: it does not wait for an app
// that is no longer starting.
func TestTheWaitEndsWhenTheAppsRunEndsBeforeItAnswers(t *testing.T) {
	r := appWaitRunner(t, zap.NewNop())
	ended := make(chan struct{})
	var once sync.Once
	appURL, app, served := heldApp(t, 1000, func(n int64) {
		if n == 3 {
			once.Do(func() { close(ended) })
		}
	})

	res := r.RunTest(context.Background(), RunTestOpts{TestSetID: "test-set-0", TestStepID: "health", ServiceURL: appURL, AppEnded: ended})

	if !strings.Contains(res.Error, "app not reachable: the app's run ended before the app at") {
		t.Fatalf("error = %q; want the one that says the app's run ended", res.Error)
	}
	if got := app.refused.Load(); got >= 1000 || served.Load() != 0 {
		t.Fatalf("the wait went on after the app's run ended: %d refused probes, %d test requests", got, served.Load())
	}
}

// The caller's ctx ends an unbudgeted wait, and the error says it was
// stopped, not that a budget ran out.
func TestTheCallersContextEndsTheUnbudgetedWait(t *testing.T) {
	r := appWaitRunner(t, zap.NewNop())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	appURL, app, _ := heldApp(t, 1000, func(n int64) {
		if n == 3 {
			cancel()
		}
	})

	res := r.RunTest(ctx, RunTestOpts{TestSetID: "test-set-0", TestStepID: "health", ServiceURL: appURL, AppEnded: make(chan struct{})})

	if !strings.Contains(res.Error, "app not reachable: stopped waiting for app at") || !strings.Contains(res.Error, context.Canceled.Error()) {
		t.Fatalf("error = %q; want the one that says the caller stopped the wait", res.Error)
	}
	if got := app.refused.Load(); got >= 1000 {
		t.Fatalf("the wait went on after its ctx ended: %d refused probes", got)
	}
}
