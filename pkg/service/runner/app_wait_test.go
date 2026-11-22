package integration

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
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
// hands the rest to the app. A refusal is counted, and onRefuse (if set) is
// told its count, before the reset: a probe the reset failed is counted, and
// what onRefuse does is done, by the time the probe returns.
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
		n := l.refused.Add(1)
		if l.onRefuse != nil {
			l.onRefuse(n)
		}
		if tc, ok := c.(*net.TCPConn); ok {
			_ = tc.SetLinger(0) // a reset, as the CI's "connection reset by peer"
		}
		_ = c.Close()
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

// Root: the case keeps no file beside it.
func (oneCaseDB) Root() string { return "" }

// appWaitInstr is an agent that accepts every call and has served no mock.
type appWaitInstr struct{ setupInstr }

func (*appWaitInstr) GetConsumedMocks(context.Context) ([]models.MockState, error) {
	return nil, nil
}

// appWaitRunner is a runner of one recorded GET /health, with probes paced
// at a millisecond and a budget that has run out before the first probe:
// a wait that keeps a budget ends at once, one that has none waits for the
// app. Its probes have no timeout of their own, so the rows' counts of
// probes and connections hold on a host that stalls the test: a stall can
// delay a probe, but not end one and so add another.
func appWaitRunner(t *testing.T, logger *zap.Logger) *Runner {
	t.Helper()
	budget, tick, say, probeTimeout := appWaitBudget, appProbeTick, appSayEvery, appProbeTimeout
	appWaitBudget, appProbeTick, appSayEvery, appProbeTimeout = time.Nanosecond, time.Millisecond, 2, 0
	t.Cleanup(func() { appWaitBudget, appProbeTick, appSayEvery, appProbeTimeout = budget, tick, say, probeTimeout })
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
	// Probes 2 and 4 of the 5 the app does not answer: one line every
	// appSayEvery (2) unanswered probes, not once.
	said := logs.FilterMessageSnippet("still waiting for the app to answer").All()
	if len(said) < 2 {
		t.Fatalf("the held wait said %d times that it still waits; want it said every 2 of the 5 unanswered probes", len(said))
	}
	if !strings.Contains(said[0].Message, "ServiceURL (--app-url)") {
		t.Fatalf("the line that says it still waits does not name the ServiceURL as the other cause: %q", said[0].Message)
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

// tickedWait has waitForApp tick every tick for the test, with no timeout of
// its own on a probe, and say every say unanswered probes that it still waits.
func tickedWait(t *testing.T, tick time.Duration, say int) {
	t.Helper()
	was, wasSay, wasTimeout := appProbeTick, appSayEvery, appProbeTimeout
	appProbeTick, appSayEvery, appProbeTimeout = tick, say, 0
	t.Cleanup(func() { appProbeTick, appSayEvery, appProbeTimeout = was, wasSay, wasTimeout })
}

// The end of the app's run wins over a tick that is ready with it (a probe
// that outlasted the tick leaves one ready), so no probe follows the end. A
// tick of a nanosecond is ready at every select, and the end has closed
// before the wait starts: each of 400 waits makes its first probe and no
// other. With no priority, select would pick the tick about half the time.
func TestTheEndWinsOverATickReadyWithIt(t *testing.T) {
	tickedWait(t, time.Nanosecond, 2)
	appURL, app, _ := heldApp(t, 1<<40, nil)
	ended := make(chan struct{})
	close(ended)

	for i := 0; i < 400; i++ {
		before := app.refused.Load()
		err := waitForApp(context.Background(), appURL, time.Hour, ended, zap.NewNop())
		if err == nil || !strings.Contains(err.Error(), "the app's run ended before the app at") {
			t.Fatalf("wait %d: err = %v; want the one that says the app's run ended", i, err)
		}
		if got := app.refused.Load() - before; got != 1 {
			t.Fatalf("wait %d made %d probes; want its first only, as the app's run had ended", i, got)
		}
	}
}

// The caller's stop wins over a tick that is ready with it in the same way: a
// wait whose ctx has ended probes no more, and so never says it still waits.
// The ctx is cancelled before each of 400 waits, and the wait says it still
// waits after every unanswered probe but its first.
func TestTheCallersStopWinsOverATickReadyWithIt(t *testing.T) {
	tickedWait(t, time.Nanosecond, 1)
	core, logs := observer.New(zap.InfoLevel)
	appURL, _, _ := heldApp(t, 1<<40, nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	for i := 0; i < 400; i++ {
		err := waitForApp(ctx, appURL, time.Hour, make(chan struct{}), zap.New(core))
		if err == nil || !strings.Contains(err.Error(), "stopped waiting for app at") || !errors.Is(err, context.Canceled) {
			t.Fatalf("wait %d: err = %v; want the one that says the caller stopped the wait", i, err)
		}
	}
	if said := logs.FilterMessageSnippet("still waiting for the app to answer").Len(); said != 0 {
		t.Fatalf("the waits said %d times that they still wait after their ctx had ended; want none", said)
	}
}

// Neither the end nor the caller's stop waits for the next tick: with the
// tick an hour away, each ends the wait after its first probe. (A wait that
// waited for the tick would hold the test until go test's own timeout.)
func TestATickAnHourAwayHoldsNeitherTheEndNorTheCallersStop(t *testing.T) {
	tickedWait(t, time.Hour, 2)
	appURL, app, _ := heldApp(t, 1<<40, nil)

	t.Run("the end", func(t *testing.T) {
		ended := make(chan struct{})
		close(ended)
		err := waitForApp(context.Background(), appURL, time.Hour, ended, zap.NewNop())
		if err == nil || !strings.Contains(err.Error(), "the app's run ended before the app at") {
			t.Fatalf("err = %v; want the one that says the app's run ended", err)
		}
	})
	t.Run("the caller's stop", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		err := waitForApp(ctx, appURL, time.Hour, make(chan struct{}), zap.NewNop())
		if err == nil || !strings.Contains(err.Error(), "stopped waiting for app at") {
			t.Fatalf("err = %v; want the one that says the caller stopped the wait", err)
		}
	})
	if got := app.refused.Load(); got != 1 {
		t.Fatalf("the waits made %d probes the app refused; want the first wait's one (the second's ctx had ended)", got)
	}
}

// answerApp is an app that answers at once, over TLS with the config cfg when
// cfg is set (the test server's own certificate when cfg has none). onConn,
// if set, is told each connection's count as it is accepted.
func answerApp(t *testing.T, cfg *tls.Config, onConn func(n int64)) (*httptest.Server, *atomic.Int64) {
	t.Helper()
	srv, conns, _ := heldAnswerApp(t, 0, cfg, onConn)
	return srv, conns
}

// heldAnswerApp is answerApp with its start held for hold connections (see
// heldListener): only the connections after those reach the app, and its
// TLS, and are counted.
func heldAnswerApp(t *testing.T, hold int64, cfg *tls.Config, onConn func(n int64)) (*httptest.Server, *atomic.Int64, *heldListener) {
	t.Helper()
	conns := &atomic.Int64{}
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "ok")
	}))
	held := &heldListener{Listener: srv.Listener, hold: hold}
	srv.Listener = held
	srv.Config.ConnState = func(_ net.Conn, s http.ConnState) {
		if s == http.StateNew {
			if n := conns.Add(1); onConn != nil {
				onConn(n)
			}
		}
	}
	if cfg != nil {
		srv.TLS = cfg
		srv.StartTLS()
	} else {
		srv.Start()
	}
	t.Cleanup(srv.Close)
	return srv, conns, held
}

// expiredCert is a certificate for the loopback addresses whose validity
// ended a day ago.
func expiredCert(t *testing.T) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "expired"},
		NotBefore:    time.Now().Add(-48 * time.Hour),
		NotAfter:     time.Now().Add(-24 * time.Hour),
		IPAddresses:  []net.IP{net.IPv4(127, 0, 0, 1), net.IPv6loopback},
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

// An unbudgeted wait ends at once, failing the test, on an answer that no
// later probe can accept: it would otherwise hold the run for as long as the
// app runs. Each case's app runs on (its end never closes); the caller's ctx
// ends the wait at the third connection, so a wait that goes on fails the
// row instead of holding it.
func TestAnAnswerTheTestCanNeverAcceptEndsTheUnbudgetedWaitAtOnce(t *testing.T) {
	for _, c := range []struct {
		name  string
		cfg   *tls.Config
		url   func(srvURL string) string
		cause string
	}{
		{"a certificate of an unknown authority", &tls.Config{}, func(u string) string { return u }, "certificate signed by unknown authority"},
		{"another host's certificate", &tls.Config{}, func(u string) string { return strings.Replace(u, "127.0.0.1", "localhost", 1) }, "not localhost"},
		{"an expired certificate", &tls.Config{Certificates: []tls.Certificate{expiredCert(t)}}, func(u string) string { return u }, "certificate has expired"},
		{"plain HTTP to an https ServiceURL", nil, func(u string) string { return strings.Replace(u, "http://", "https://", 1) }, "server gave HTTP response to HTTPS client"},
	} {
		t.Run(c.name, func(t *testing.T) {
			r := appWaitRunner(t, zap.NewNop())
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			srv, conns := answerApp(t, c.cfg, func(n int64) {
				if n == 3 {
					cancel()
				}
			})
			appURL := c.url(srv.URL)

			res := r.RunTest(ctx, RunTestOpts{TestSetID: "test-set-0", TestStepID: "health", ServiceURL: appURL, AppEnded: make(chan struct{})})

			if !strings.Contains(res.Error, "app not reachable: failed to probe") || !strings.Contains(res.Error, c.cause) {
				t.Fatalf("error = %q; want the wait to fail at once on %q", res.Error, c.cause)
			}
			if !strings.Contains(res.Error, appURL) || !strings.Contains(res.Error, "ServiceURL (--app-url)") {
				t.Fatalf("error = %q; want it to name the app's URL %s and the ServiceURL (--app-url)", res.Error, appURL)
			}
			if got := conns.Load(); got != 1 {
				t.Fatalf("the wait probed the app %d times; want it to end on the first answer", got)
			}
		})
	}
}

// The same answer ends the unbudgeted wait at once when it comes after a held
// start, the case the wait has no budget for: compose holds the app's start
// behind a slow dependency, so the first probes are reset, and then the app
// comes up with a certificate the probe cannot verify. The app's run goes on
// (its end never closes); the caller's ctx ends the wait at the app's third
// TLS connection, so a wait that goes on fails the row instead of holding it.
func TestAnAnswerNoProbeCanAcceptEndsTheWaitWhenItComesAfterAHeldStart(t *testing.T) {
	r := appWaitRunner(t, zap.NewNop())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	srv, conns, held := heldAnswerApp(t, 4, &tls.Config{}, func(n int64) {
		if n == 3 {
			cancel()
		}
	})

	res := r.RunTest(ctx, RunTestOpts{TestSetID: "test-set-0", TestStepID: "health", ServiceURL: srv.URL, AppEnded: make(chan struct{})})

	if ctx.Err() != nil || !strings.Contains(res.Error, "app not reachable: failed to probe") || !strings.Contains(res.Error, "certificate signed by unknown authority") {
		t.Fatalf("error = %q (ctx: %v); want the wait to fail on the app's first answer, with its ctx live", res.Error, ctx.Err())
	}
	if refused, got := held.refused.Load(), conns.Load(); refused != 4 || got != 1 {
		t.Fatalf("the wait ended after %d reset probes and %d answered; want it to wait out the 4 resets, then end on the first answer", refused, got)
	}
}

// Other TLS handshake errors stay not-ready: a TLS sidecar can send them
// while it starts. Here the app's TLS fails every handshake with an internal
// error, and the wait goes on until the app's run ends, at the third
// connection. Should the wait not end on that, the caller's ctx ends it at
// the twentieth.
func TestAnotherTLSHandshakeErrorLeavesTheAppNotReady(t *testing.T) {
	r := appWaitRunner(t, zap.NewNop())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ended := make(chan struct{})
	var once sync.Once
	srv, conns := answerApp(t, &tls.Config{GetConfigForClient: func(*tls.ClientHelloInfo) (*tls.Config, error) {
		return nil, errors.New("certificate not loaded yet")
	}}, func(n int64) {
		switch n {
		case 3:
			once.Do(func() { close(ended) })
		case 20:
			cancel()
		}
	})

	res := r.RunTest(ctx, RunTestOpts{TestSetID: "test-set-0", TestStepID: "health", ServiceURL: srv.URL, AppEnded: ended})

	if !strings.Contains(res.Error, "app not reachable: the app's run ended before the app at") || !strings.Contains(res.Error, "internal error") {
		t.Fatalf("error = %q; want the wait to go on past the handshake errors until the app's run ended", res.Error)
	}
	if got := conns.Load(); got < 3 {
		t.Fatalf("the wait ended after %d connections; want it to go on until the app's run ended at the third", got)
	}
}

// A wait with a budget (a caller that did not start the app: k8s-proxy, the
// sandbox's UI path) retries a certificate it cannot verify until its budget,
// as before: a k8s ingress serves a placeholder certificate until its own is
// loaded. The caller's ctx ends it at the third connection.
func TestABudgetedWaitRetriesACertificateItCannotVerify(t *testing.T) {
	r := appWaitRunner(t, zap.NewNop())
	appWaitBudget = time.Hour
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	srv, conns := answerApp(t, &tls.Config{}, func(n int64) {
		if n == 3 {
			cancel()
		}
	})

	res := r.RunTest(ctx, RunTestOpts{TestSetID: "test-set-0", TestStepID: "health", ServiceURL: srv.URL})

	if !strings.Contains(res.Error, "app not reachable: stopped waiting for app at") {
		t.Fatalf("error = %q; want the budgeted wait to retry the certificate until its ctx ended", res.Error)
	}
	if got := conns.Load(); got < 3 {
		t.Fatalf("the budgeted wait ended after %d connections; want it to retry until its ctx ended at the third", got)
	}
}

// A wait with a budget has no run to wait on, so it never says that its run
// has not ended, however many probes go unanswered.
func TestABudgetedWaitSaysNothingOfAnAppsRun(t *testing.T) {
	core, logs := observer.New(zap.InfoLevel)
	r := appWaitRunner(t, zap.New(core))
	appWaitBudget = time.Hour
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	appURL, app, _ := heldApp(t, 1000, func(n int64) {
		if n == 6 {
			cancel()
		}
	})

	res := r.RunTest(ctx, RunTestOpts{TestSetID: "test-set-0", TestStepID: "health", ServiceURL: appURL})

	if !strings.Contains(res.Error, "app not reachable: stopped waiting for app at") || app.refused.Load() < 6 {
		t.Fatalf("error = %q after %d refused probes; want the budgeted wait to go on until its ctx ended at the sixth", res.Error, app.refused.Load())
	}
	if said := logs.FilterMessageSnippet("its run has not ended").All(); len(said) != 0 {
		t.Fatalf("the budgeted wait said %d times that its run has not ended: %q", len(said), said[0].Message)
	}
}
