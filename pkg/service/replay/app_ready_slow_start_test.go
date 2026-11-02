package replay

import (
	"context"
	"net"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/internal/testnet"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

// A slow start: the application keploy started listens on its port only when
// the row says so (startApp), long after every ceiling the wait is given. The
// wait for it ends when the application accepts a connection, or when the
// application exits (its run's context is cancelled), never at a ceiling.

// slowApp is an application that refuses on its port until start serves it.
type slowApp struct {
	t    *testing.T
	port string
}

func newSlowApp(t *testing.T) *slowApp {
	return &slowApp{t: t, port: strconv.Itoa(testnet.SharedPort(t))}
}

func (a *slowApp) addr() string { return net.JoinHostPort("127.0.0.1", a.port) }

// start begins serving HTTP on the application's port: every path answers 200.
func (a *slowApp) start() {
	ln := testnet.ListenShared(a.t, "tcp", a.addr())
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}), ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = srv.Serve(ln) }()
	a.t.Cleanup(func() { _ = srv.Close() })
}

// slowWaitResult runs waitForAppReady in the background and reports its answer.
func slowWaitResult(ctx context.Context, logger *zap.Logger, cfg *config.Config, probe httpProbeTarget) <-chan bool {
	done := make(chan bool, 1)
	go func() { done <- waitForAppReady(ctx, logger, cfg, probe) }()
	return done
}

// requireStillWaiting fails the row when the wait has ended before the
// application accepted a connection: it would have fired the first test at a
// port that refuses it.
func requireStillWaiting(t *testing.T, done <-chan bool, past time.Duration) {
	t.Helper()
	select {
	case ok := <-done:
		t.Fatalf("the wait ended (ready=%v) before the application accepted a connection on its port", ok)
	case <-time.After(past):
	}
}

func requireReady(t *testing.T, done <-chan bool) {
	t.Helper()
	select {
	case ok := <-done:
		if !ok {
			t.Fatal("the wait reported the run stopped, not the application ready")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the wait did not end once the application accepted a connection")
	}
}

// The failure this row reproduces: --health-url names the application's own
// port, the application starts slower than the poll timeout, and replay fell
// back to a fixed delay and fired the first test at a port that refused it.
func TestAHealthURLThatOutlastsItsPollTimeoutWaitsForTheAppToAccept(t *testing.T) {
	app := newSlowApp(t)
	cfg := &config.Config{}
	cfg.Test.Delay = 0
	cfg.Test.HealthURL = "http://" + app.addr() + "/healthz"
	cfg.Test.HealthPollTimeout = 200 * time.Millisecond

	core, logs := observer.New(zap.InfoLevel)
	probe := httpProbeTarget{scheme: "http", host: "127.0.0.1", port: app.port, ok: true}
	done := slowWaitResult(context.Background(), zap.New(core), cfg, probe)

	requireStillWaiting(t, done, 2*time.Second)
	app.start()
	requireReady(t, done)

	said := 0
	for _, e := range logs.All() {
		if strings.Contains(e.Message, "fixed delay") {
			t.Fatalf("the wait still speaks of a fixed delay: %q", e.Message)
		}
		if strings.Contains(e.Message, "waiting for the application to accept a connection") {
			said++
			if !strings.Contains(e.Message, app.addr()) {
				t.Fatalf("the wait does not say what it waits for: %q", e.Message)
			}
		}
	}
	if said != 1 {
		t.Fatalf("the wait said it was waiting %d times, want once", said)
	}
}

// Without --health-url: the address the set's first test dials refuses past
// the ceiling, and the first test waits for it.
func TestTheTestAddressGateWaitsPastItsCeilingForAnAppThatStartsSlowly(t *testing.T) {
	app := newSlowApp(t)
	cfg := &config.Config{}
	cfg.Test.Delay = 0
	cfg.Test.HealthPollTimeout = 200 * time.Millisecond

	probe := httpProbeTarget{scheme: "http", host: "127.0.0.1", port: app.port, ok: true}
	done := slowWaitResult(context.Background(), zap.NewNop(), cfg, probe)

	requireStillWaiting(t, done, 2*time.Second)
	app.start()
	requireReady(t, done)
}

// test.appReadyProbeAddr: the same, for an address the user names.
func TestTheProbeAddrGateWaitsPastItsCeilingForAnAppThatStartsSlowly(t *testing.T) {
	app := newSlowApp(t)
	cfg := &config.Config{}
	cfg.Test.Delay = 0
	cfg.Test.AppReadyProbeAddr = app.addr()
	cfg.Test.HealthPollTimeout = 200 * time.Millisecond

	done := slowWaitResult(context.Background(), zap.NewNop(), cfg, httpProbeTarget{})

	requireStillWaiting(t, done, 2*time.Second)
	app.start()
	requireReady(t, done)
}

// The application exits before it ever accepts: its run's context is
// cancelled, and the wait reports the run stopped (false), so no test is
// fired at it.
func TestTheWaitForASlowAppEndsWhenTheAppExits(t *testing.T) {
	app := newSlowApp(t)
	cfg := &config.Config{}
	cfg.Test.Delay = 0
	cfg.Test.HealthURL = "http://" + app.addr() + "/healthz"
	cfg.Test.HealthPollTimeout = 200 * time.Millisecond

	ctx, appExited := context.WithCancel(context.Background())
	defer appExited()
	probe := httpProbeTarget{scheme: "http", host: "127.0.0.1", port: app.port, ok: true}
	done := slowWaitResult(ctx, zap.NewNop(), cfg, probe)

	requireStillWaiting(t, done, 2*time.Second)
	appExited()
	select {
	case ok := <-done:
		if ok {
			t.Fatal("the wait reported an application that exited ready")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the wait did not end when the application exited")
	}
}
