package replay

import (
	"context"
	"net"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// resettingUntil is docker's proxy in front of an application still starting:
// it accepts every connection and resets it, until serving is set; then the
// application answers HTTP.
type resettingUntil struct {
	net.Listener
	serving *atomic.Bool
}

func (l resettingUntil) Accept() (net.Conn, error) {
	for {
		c, err := l.Listener.Accept()
		if err != nil || l.serving.Load() {
			return c, err
		}
		if tc, ok := c.(*net.TCPConn); ok {
			_ = tc.SetLinger(0)
		}
		_ = c.Close()
	}
}

func resettingApp(t *testing.T) (addr string, serving *atomic.Bool) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	serving = new(atomic.Bool)
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }), ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = srv.Serve(resettingUntil{ln, serving}) }()
	t.Cleanup(func() { _ = srv.Close() })
	return ln.Addr().String(), serving
}

func resetTestCase(addr string) *models.TestCase {
	return &models.TestCase{Name: "test-1", Kind: models.HTTP, HTTPReq: models.HTTPReq{URL: "http://" + addr + "/x"}}
}

// An application keploy started, still starting behind docker's published
// port, resets a test request (docker's proxy) instead of refusing it. Its
// re-send waits for the application to answer HTTP, past the bound of an
// application keploy did not start, and only its answer, its exit or
// keploy's stop ends the wait, said once per request.
func TestAResetRequestWaitsForTheAppKeployStartedToAnswer(t *testing.T) {
	addr, serving := resettingApp(t)
	core, logs := observer.New(zapcore.InfoLevel)
	r := &Replayer{config: &config.Config{Command: "node app.js"}, logger: zap.New(core)}
	ctx, cancel := context.WithCancel(pkg.EndsWithApp(context.Background()))
	defer cancel()
	said := false
	done := make(chan struct{})
	go func() {
		defer close(done)
		r.waitForResetResendReady(ctx, resetTestCase(addr), "test-set-0", &said)
	}()

	select {
	case <-done:
		t.Fatal("the re-send's wait ended while the application still reset, keploy having started it")
	case <-time.After(resetResendReadyTimeout + time.Second):
	}
	serving.Store(true)
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the re-send's wait did not end once the application answered")
	}
	// The address is the one the test request dials (keploy test's host,
	// localhost by default, and the recorded port).
	_, port, _ := net.SplitHostPort(addr)
	if n := logs.FilterMessageSnippet("was reset at localhost:" + port + " before the application took it").Len(); n != 1 {
		t.Errorf("said %d times that the re-send waits on localhost:%s, want once: %v", n, port, logs.All())
	}

	// The request's next reset does not say it again.
	serving.Store(false)
	ctx2, cancel2 := context.WithCancel(pkg.EndsWithApp(context.Background()))
	go func() {
		time.Sleep(200 * time.Millisecond)
		cancel2()
	}()
	r.waitForResetResendReady(ctx2, resetTestCase(addr), "test-set-0", &said)
	if n := logs.FilterMessageSnippet("before the application took it").Len(); n != 1 {
		t.Errorf("said %d times for one request, want once", n)
	}
}

// The application's exit ends the wait at once.
func TestAResetRequestsWaitEndsWhenTheAppExits(t *testing.T) {
	addr, _ := resettingApp(t)
	r := &Replayer{config: &config.Config{Command: "node app.js"}, logger: zap.NewNop()}
	ctx, cancel := context.WithCancel(pkg.EndsWithApp(context.Background()))
	done := make(chan struct{})
	go func() {
		defer close(done)
		r.waitForResetResendReady(ctx, resetTestCase(addr), "test-set-0", new(bool))
	}()
	time.Sleep(300 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("the re-send's wait went on after the application exited")
	}
}

// keploy did not start the application, so nothing would end a wait: the
// re-send waits at most resetResendReadyTimeout, as before.
func TestAResetRequestWaitsABoundedTimeWithoutAnAppKeployStarted(t *testing.T) {
	addr, _ := resettingApp(t)
	r := &Replayer{config: &config.Config{Command: "node app.js"}, logger: zap.NewNop()}
	// The row's own stop ends a wait that has no bound, so the row fails by
	// its own check, not by the test binary's timeout.
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	t.Cleanup(func() {
		cancel()
		<-done
	})
	go func() {
		defer close(done)
		r.waitForResetResendReady(ctx, resetTestCase(addr), "test-set-0", new(bool))
	}()
	select {
	case <-done:
	case <-time.After(resetResendReadyTimeout + 3*time.Second):
		t.Fatalf("still waiting after %s, want at most about %s", resetResendReadyTimeout+3*time.Second, resetResendReadyTimeout)
	}
}
