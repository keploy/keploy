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
	"go.keploy.io/server/v3/pkg"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
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
	// An application keploy runs: its exit cancels its run's context.
	go func() { done <- waitForAppReady(pkg.EndsWithApp(ctx), logger, cfg, probe) }()
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

// With probing the app's address disabled, a health URL that outlasts its
// poll timeout is polled on: the first test waits for its 2xx, not for a
// ceiling.
func TestAHealthURLThatOutlastsItsPollTimeoutIsPolledOnWithTheAddressProbeDisabled(t *testing.T) {
	app := newSlowApp(t)
	cfg := &config.Config{}
	cfg.Test.Delay = 0
	cfg.Test.HealthURL = "http://" + app.addr() + "/healthz"
	cfg.Test.HealthPollTimeout = 200 * time.Millisecond
	cfg.Test.DisableAppReadyProbe = true

	core, logs := observer.New(zap.InfoLevel)
	done := slowWaitResult(context.Background(), zap.New(core), cfg, httpProbeTarget{})

	requireStillWaiting(t, done, 2*time.Second)
	if logs.FilterMessageSnippet("probing the app's address is disabled").Len() != 1 {
		t.Errorf("the wait did not say once that it waits for the health URL with address probing disabled: %v", logs.All())
	}
	app.start()
	requireReady(t, done)
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

// The health URL's own address is waited on only when keploy knows no other
// address of the application: once the address the set's tests dial, or
// test.appReadyProbeAddr, accepts, the first test is fired, though the health
// URL names an address that refuses.
func TestAHealthURLsAddressIsWaitedOnOnlyWhenNoOtherAddressIsKnown(t *testing.T) {
	for _, viaProbeAddr := range []bool{false, true} {
		name := "the tests' address"
		if viaProbeAddr {
			name = "test.appReadyProbeAddr"
		}
		t.Run(name, func(t *testing.T) {
			app := newSlowApp(t)
			app.start()
			refusing := newSlowApp(t) // never started: its port refuses
			cfg := &config.Config{}
			cfg.Test.Delay = 0
			cfg.Test.HealthURL = "http://" + refusing.addr() + "/healthz"
			cfg.Test.HealthPollTimeout = 200 * time.Millisecond
			probe := httpProbeTarget{scheme: "http", host: "127.0.0.1", port: app.port, ok: true}
			if viaProbeAddr {
				cfg.Test.AppReadyProbeAddr = app.addr()
				probe = httpProbeTarget{}
			}

			// A wait on the refusing address would never end by itself: the
			// row ends it the moment the wait says it waits there.
			waitsThere := "waiting for the application to accept a connection on " + refusing.addr()
			ctx, cancel := context.WithCancel(context.Background())
			t.Cleanup(cancel)
			core, logs := observer.New(zap.InfoLevel)
			logger := zap.New(zapcore.RegisterHooks(core, func(e zapcore.Entry) error {
				if strings.Contains(e.Message, waitsThere) {
					cancel()
				}
				return nil
			}))

			requireReady(t, slowWaitResult(ctx, logger, cfg, probe))
			if logs.FilterMessageSnippet(waitsThere).Len() != 0 {
				t.Fatalf("the first test waited on the health URL's address after the app's own address accepted: %v", logs.All())
			}
		})
	}
}

// A health URL with no host, "http://:<port>/healthz", is the local machine's
// port: past its poll timeout the first test waits for that address to accept,
// as it does for a health URL that names its host.
func TestAHealthURLWithNoHostWaitsForTheLocalAppToAccept(t *testing.T) {
	app := newSlowApp(t)
	cfg := &config.Config{}
	cfg.Test.Delay = 0
	cfg.Test.HealthURL = "http://:" + app.port + "/healthz"
	cfg.Test.HealthPollTimeout = 200 * time.Millisecond

	done := slowWaitResult(context.Background(), zap.NewNop(), cfg, httpProbeTarget{})

	requireStillWaiting(t, done, 2*time.Second)
	app.start()
	requireReady(t, done)
}

// healthURLAddress is the address the health URL's request dials: its port,
// or the scheme's default port when it names none; no address without a URL.
func TestTheHealthURLsAddressIsTheOneItsRequestDials(t *testing.T) {
	for _, c := range []struct {
		url, host, port string
		ok              bool
	}{
		{"", "", "", false},
		{"http://127.0.0.1:8080/healthz", "127.0.0.1", "8080", true},
		{"http://127.0.0.1/healthz", "127.0.0.1", "80", true},
		{"https://127.0.0.1/healthz", "127.0.0.1", "443", true},
		{"https://127.0.0.1:8443/healthz", "127.0.0.1", "8443", true},
		{"http://[::1]:8080/healthz", "::1", "8080", true},
		{"http://:8080/healthz", "", "8080", true},
	} {
		host, port, ok := healthURLAddress(c.url)
		if host != c.host || port != c.port || ok != c.ok {
			t.Errorf("healthURLAddress(%q) = (%q, %q, %v), want (%q, %q, %v)", c.url, host, port, ok, c.host, c.port, c.ok)
		}
	}
}

// An unset --health-poll-timeout is the default, not zero.
func TestAnUnsetHealthPollTimeoutIsItsDefault(t *testing.T) {
	// The health URL is polled for the default time, and the poll says so.
	t.Run("the health URL's poll", func(t *testing.T) {
		app := newSlowApp(t)
		app.start()
		cfg := &config.Config{}
		cfg.Test.Delay = 0
		cfg.Test.HealthURL = "http://" + app.addr() + "/healthz"

		core, logs := observer.New(zap.InfoLevel)
		requireReady(t, slowWaitResult(context.Background(), zap.New(core), cfg, httpProbeTarget{}))
		polled := logs.FilterMessageSnippet("polling application health endpoint before firing tests").All()
		if len(polled) != 1 {
			t.Fatalf("the health URL's poll was announced %d times, want once: %v", len(polled), logs.All())
		}
		if got := polled[0].ContextMap()["pollTimeout"]; got != config.DefaultHealthPollTimeout {
			t.Fatalf("the health URL is polled for %v, want the default %v", got, config.DefaultHealthPollTimeout)
		}
	})
	// The address gate's HTTP check after the accept gets the default time:
	// an app that answers at once is confirmed, not warned about.
	t.Run("the address gate's HTTP check", func(t *testing.T) {
		app := newSlowApp(t)
		app.start()
		cfg := &config.Config{}
		cfg.Test.Delay = 0

		core, logs := observer.New(zap.DebugLevel)
		probe := httpProbeTarget{scheme: "http", host: "127.0.0.1", port: app.port, ok: true}
		requireReady(t, slowWaitResult(context.Background(), zap.New(core), cfg, probe))
		if logs.FilterMessageSnippet("never completed an HTTP round-trip").Len() != 0 {
			t.Fatalf("an app that answered at once was warned about as never answering: %v", logs.All())
		}
		if logs.FilterMessageSnippet("confirmed by a completed HTTP round-trip").Len() != 1 {
			t.Fatalf("the app's answer was not confirmed once: %v", logs.All())
		}
	})
}
