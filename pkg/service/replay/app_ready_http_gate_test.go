package replay

import (
	"context"
	"crypto/tls"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// acceptOnlyListener mimics docker's userland proxy in the failure state: it
// ACCEPTS the connection (so a TCP-accept gate is satisfied instantly) and then
// closes it without ever speaking HTTP, so no request can complete.
func acceptOnlyListener(t *testing.T) (host, port string) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			_ = c.Close() // accepted, then reset — exactly the docker-proxy shape
		}
	}()
	h, p, _ := net.SplitHostPort(ln.Addr().String())
	return h, p
}

// countingListener is acceptOnlyListener that also reports how many
// connections were made to it.
//
// The disable-probe contract is "no connect-then-close happens", and
// elapsed time cannot express that: against a listener that accepts
// instantly, a probe that IS made still returns in microseconds, so a
// wall-clock assertion passes whether or not the flag was honoured. The
// hazard being guarded against is the connection itself.
func countingListener(t *testing.T) (host, port string, conns *atomic.Int64) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	var n atomic.Int64
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			n.Add(1)
			_ = c.Close()
		}
	}()
	h, p, _ := net.SplitHostPort(ln.Addr().String())
	return h, p, &n
}

func gateCfg(ceiling time.Duration) *config.Config {
	c := &config.Config{}
	c.Test.HealthPollTimeout = ceiling
	return c
}

// THE REGRESSION. A port that accepts TCP but never serves HTTP must NOT be
// accepted as ready for an HTTP app. This is the parse-server-linux failure:
// docker-proxy binds and accepts the instant the container is created, ~17-28s
// before the app inside is listening, so a TCP-accept gate passes immediately and
// replay fires into a port that resets its first request (status_code got=0 on
// the first test of the set).
func TestGateOnAppAddress_AcceptOnlyPortIsNotReadyForAnHTTPApp(t *testing.T) {
	host, port := acceptOnlyListener(t)
	core, logs := observer.New(zapcore.DebugLevel)

	start := time.Now()
	ok := gateOnAppAddress(context.Background(), zap.New(core), gateCfg(700*time.Millisecond),
		host, port, "test", httpProbeTarget{scheme: "http", host: host, port: port, ok: true})
	elapsed := time.Since(start)

	if !ok {
		t.Fatal("gate must stay best-effort and return true on timeout, never fail the run")
	}
	if elapsed < 500*time.Millisecond {
		t.Fatalf("gate returned after %v — it accepted a TCP-only port as ready for an HTTP app, "+
			"which is exactly the docker-proxy false-ready that fails the first test", elapsed)
	}
	if n := len(logs.FilterMessageSnippet("never completed an HTTP round-trip").All()); n != 1 {
		t.Fatalf("expected the HTTP-escalation warning, got %d", n)
	}
}

// The same port MUST still satisfy the gate for a non-HTTP app (MySQL, Redis,
// gRPC-only): probing it with HTTP could never parse a response, so escalating
// would burn the whole ceiling on every run. TCP accept is all we can prove.
func TestGateOnAppAddress_AcceptOnlyPortIsReadyForANonHTTPApp(t *testing.T) {
	host, port := acceptOnlyListener(t)

	start := time.Now()
	ok := gateOnAppAddress(context.Background(), zap.NewNop(), gateCfg(5*time.Second),
		host, port, "test", httpProbeTarget{})
	elapsed := time.Since(start)

	if !ok {
		t.Fatal("non-HTTP app must pass on TCP accept")
	}
	if elapsed > 2*time.Second {
		t.Fatalf("non-HTTP app waited %v — the HTTP stage must be skipped entirely, "+
			"or every non-HTTP replay pays the ceiling", elapsed)
	}
}

// A genuinely serving app satisfies the gate immediately, so the raised ceiling
// costs a ready app nothing.
func TestGateOnAppAddress_ServingAppIsReadyImmediately(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound) // any completed response proves serving
	}))
	defer srv.Close()
	host, port, _ := net.SplitHostPort(srv.Listener.Addr().String())

	start := time.Now()
	ok := gateOnAppAddress(context.Background(), zap.NewNop(), gateCfg(3*time.Minute), host, port, "test",
		httpProbeTarget{scheme: "http", host: host, port: port, ok: true})
	if !ok {
		t.Fatal("a serving app must pass the gate")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("a ready app waited %v; the gate must satisfy on the first probe", elapsed)
	}
}

// An operator-supplied health path must actually be the path requested.
func TestGateOnAppAddress_UsesConfiguredHealthPath(t *testing.T) {
	got := make(chan string, 4)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case got <- r.URL.Path:
		default:
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	host, port, _ := net.SplitHostPort(srv.Listener.Addr().String())

	cfg := gateCfg(10 * time.Second)
	cfg.Test.HealthPath = "healthz" // deliberately missing the leading slash
	if !gateOnAppAddress(context.Background(), zap.NewNop(), cfg, host, port, "test",
		httpProbeTarget{scheme: "http", host: host, port: port, ok: true}) {
		t.Fatal("gate should pass against a serving app")
	}
	select {
	case p := <-got:
		if p != "/healthz" {
			t.Fatalf("probed %q, want /healthz — the configured health path was ignored", p)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("app never received the probe")
	}
}

// REGRESSION (review finding 1): stage 2 must probe the address the recorded
// tests actually DIAL, not whatever the docker/compose heuristic guessed.
//
// dockerPublishedHostPort returns the FIRST -p flag with no check that it is the
// HTTP port, so for `-p 5432:5432 -p 8080:8080` the gated address is the
// database. Stage 1 passes instantly (the port is bound), and an earlier revision
// then ran stage 2 against that same port — where HTTP can never answer — burning
// the whole ceiling on EVERY run, not just a cold start.
func TestGateOnAppAddress_ProbesTheResolvedTargetNotTheGatedPort(t *testing.T) {
	// The "published port" the heuristic picked: accepts TCP, never speaks HTTP.
	dbHost, dbPort := acceptOnlyListener(t)
	// The address the recorded tests actually dial: a real HTTP server.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	appHost, appPort, _ := net.SplitHostPort(srv.Listener.Addr().String())

	start := time.Now()
	ok := gateOnAppAddress(context.Background(), zap.NewNop(), gateCfg(10*time.Second),
		dbHost, dbPort, "docker-published-port",
		httpProbeTarget{scheme: "http", host: appHost, port: appPort, ok: true})
	elapsed := time.Since(start)

	if !ok {
		t.Fatal("gate must pass")
	}
	if elapsed > 3*time.Second {
		t.Fatalf("gate took %v — stage 2 probed the GATED port instead of the resolved dial "+
			"target, so a multi-port app burns the full ceiling on every run", elapsed)
	}
}

// resolveTestSetProbeTarget must license stage 2 from EVIDENCE and decline when
// it has none — an empty or non-HTTP set leaves the gate TCP-only.
func TestResolveTestSetProbeTarget(t *testing.T) {
	cfg := config.Test{}
	if got := resolveTestSetProbeTarget(cfg, nil, "test-set-0", zap.NewNop()); got.ok {
		t.Error("an empty test set proves nothing and must not license HTTP probing")
	}
	if got := resolveTestSetProbeTarget(cfg, []*models.TestCase{nil}, "test-set-0", zap.NewNop()); got.ok {
		t.Error("a nil entry must not license HTTP probing")
	}
	if got := resolveTestSetProbeTarget(cfg, []*models.TestCase{{Kind: models.GRPC_EXPORT}}, "test-set-0", zap.NewNop()); got.ok {
		t.Error("a non-HTTP test set must not license HTTP probing")
	}
	// A recorded HTTP case resolves to the address the simulation would dial.
	tc := &models.TestCase{Kind: models.HTTP}
	tc.HTTPReq.URL = "http://localhost:6219/parse/health"
	got := resolveTestSetProbeTarget(cfg, []*models.TestCase{{Kind: models.GRPC_EXPORT}, tc}, "test-set-0", zap.NewNop())
	if !got.ok {
		t.Fatal("a recorded HTTP test case must resolve a probe target")
	}
	if got.port != "6219" || got.scheme != "http" {
		t.Fatalf("resolved %s://%s:%s, want scheme http and port 6219", got.scheme, got.host, got.port)
	}
}

// The default ceiling is only ever paid by an app that is NOT serving, so it is
// sized for a cold start on a loaded runner rather than for a warm one.
func TestDefaultHealthPollTimeoutIsGenerous(t *testing.T) {
	if config.DefaultHealthPollTimeout < 2*time.Minute {
		t.Fatalf("config.DefaultHealthPollTimeout=%v is too tight: a measured cold start on a loaded "+
			"shared runner took ~55s to first serve, and timing out here fires tests into a "+
			"not-yet-listening app", config.DefaultHealthPollTimeout)
	}
}

// --health-scheme must actually override the scheme resolved from the test set,
// AND a certificate we do not trust must still count as ready.
//
// Both are checked here because they are entangled. Certificate verification
// stays ON (disabling it is a real security finding, and CodeQL flags it), so an
// httptest self-signed server fails verification and the HTTP handler is never
// reached — which means asserting on the handler cannot prove the scheme. The
// TLS handshake itself is the observable: GetConfigForClient fires before
// verification, so it records that an https probe genuinely happened. If the
// override were ignored the probe would stay on http:// and no handshake would
// occur at all.
func TestGateOnAppAddress_HealthSchemeOverridesTheResolvedScheme(t *testing.T) {
	handshakes := make(chan string, 4)
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	srv.TLS = &tls.Config{
		MinVersion: tls.VersionTLS12,
		GetConfigForClient: func(chi *tls.ClientHelloInfo) (*tls.Config, error) {
			select {
			case handshakes <- chi.ServerName:
			default:
			}
			return nil, nil
		},
	}
	srv.StartTLS()
	defer srv.Close()
	host, port, _ := net.SplitHostPort(srv.Listener.Addr().String())

	cfg := gateCfg(8 * time.Second)
	cfg.Test.HealthScheme = "https" // force https over the resolved "http"

	start := time.Now()
	if !gateOnAppAddress(context.Background(), zap.NewNop(), cfg, host, port, "test",
		httpProbeTarget{scheme: "http", host: host, port: port, ok: true}) {
		t.Fatal("gate must pass")
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("gate took %v — an untrusted certificate was treated as 'not ready', so any "+
			"self-signed local HTTPS fixture stalls the gate for its whole ceiling", elapsed)
	}

	select {
	case <-handshakes:
		// A TLS handshake happened: the override took effect.
	case <-time.After(2 * time.Second):
		t.Fatal("no TLS handshake was attempted — --health-scheme=https was ignored and the probe " +
			"stayed on http://")
	}
}

// A NATIVE app — no docker -p publish, no compose file, no appReadyProbeAddr —
// had no readiness gate at all beyond the fixed --delay, so one slower than that
// delay had its first test fired into a socket nothing was listening on yet.
// Observed on the python-cred-expiry lane (`python3 recorded_app.py`, --delay 5):
// 2 of 4 tests failed with `status_code expected=200 got=0`.
//
// The recorded test set already names the dial target, so it is used as the gate.
func TestWaitForAppReady_NativeAppGatesOnTheResolvedTarget(t *testing.T) {
	host, port := acceptOnlyListener(t) // accepts, never serves HTTP
	cfg := gateCfg(700 * time.Millisecond)
	cfg.Command = "python3 recorded_app.py" // native: none of the address gates fire
	cfg.Test.Delay = 0

	start := time.Now()
	ok := waitForAppReady(context.Background(), zap.NewNop(), cfg,
		httpProbeTarget{scheme: "http", host: host, port: port, ok: true})
	elapsed := time.Since(start)

	if !ok {
		t.Fatal("the gate must stay best-effort and proceed on timeout, never fail the run")
	}
	if elapsed < 500*time.Millisecond {
		t.Fatalf("returned after %v — a native app was declared ready without any HTTP round-trip, "+
			"so its first test can still fire into a not-yet-serving socket", elapsed)
	}
}

// ...and it must not gate when the test set gave no resolvable HTTP target,
// preserving the pure --delay behaviour for non-HTTP native apps.
func TestWaitForAppReady_NativeAppWithoutEvidenceIsNotGated(t *testing.T) {
	cfg := gateCfg(10 * time.Second)
	cfg.Command = "python3 recorded_app.py"
	cfg.Test.Delay = 0

	start := time.Now()
	if !waitForAppReady(context.Background(), zap.NewNop(), cfg, httpProbeTarget{}) {
		t.Fatal("must proceed")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("waited %v with no resolvable target; a non-HTTP native app must keep the "+
			"pure --delay behaviour", elapsed)
	}
}

// TestWaitForAppReady_DisableProbeIsHonoured covers the embedder that
// drives replay as a library where a connect-then-close probe is
// DESTRUCTIVE.
//
// keploy/k8s-proxy is the motivating case: on its cluster-mode path the
// app is reached through a kubectl port-forward, and probing it can make
// kubelet's SPDY relay tear the session down ("lost connection to pod"),
// killing the app and agent forwards together. It therefore supplies no
// test.appReadyProbeAddr — but it DOES set test.host, because that is
// what rewrites recorded request URLs.
//
// That is why the opt-out is a stated flag rather than an inference from
// "no gate input was supplied": for this caller that inference is simply
// false, and the resolved-test-target fallback would reach for test.host
// and probe it anyway.
func TestWaitForAppReady_DisableProbeIsHonoured(t *testing.T) {
	host, port := acceptOnlyListener(t) // accepts, never serves HTTP
	cfg := gateCfg(10 * time.Second)
	cfg.Command = "replay"
	cfg.CommandType = "docker-compose"
	cfg.Test.Host = host // set for request rewriting, NOT for probing
	cfg.Test.AppReadyProbeAddr = ""
	cfg.Test.DisableAppReadyProbe = true
	cfg.Test.Delay = 0

	start := time.Now()
	ok := waitForAppReady(context.Background(), zap.NewNop(), cfg,
		httpProbeTarget{scheme: "http", host: host, port: port, ok: true})
	elapsed := time.Since(start)

	if !ok {
		t.Fatal("the gate must stay best-effort and proceed, never fail the run")
	}
	if elapsed > 2*time.Second {
		t.Fatalf("waited %v with probing disabled: the caller said a connect-then-close probe "+
			"is destructive in its environment, and this one tears down the very connection "+
			"it is testing", elapsed)
	}
}

// The opt-out must cover EVERY address gate, not just the fallback: the
// hazard is the connect-then-close itself, and each of them performs
// one. Asserted by counting connections, because against a listener that
// accepts instantly a wall-clock assertion passes whether or not the
// probe was made.
func TestWaitForAppReady_DisableProbeMakesNoConnection(t *testing.T) {
	for _, tc := range []struct {
		name  string
		apply func(cfg *config.Config, host, port string)
	}{
		{
			name: "explicit appReadyProbeAddr",
			apply: func(cfg *config.Config, host, port string) {
				cfg.Test.AppReadyProbeAddr = net.JoinHostPort(host, port)
			},
		},
		{
			name: "docker published port",
			apply: func(cfg *config.Config, host, port string) {
				cfg.Command = "docker run -p " + host + ":" + port + ":" + port + " myapp"
				cfg.CommandType = "docker-run"
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			host, port, conns := countingListener(t)
			cfg := gateCfg(10 * time.Second)
			cfg.Command = "replay"
			cfg.CommandType = "docker-compose"
			cfg.Test.DisableAppReadyProbe = true
			cfg.Test.Delay = 0
			tc.apply(cfg, host, port)

			if !waitForAppReady(context.Background(), zap.NewNop(), cfg,
				httpProbeTarget{scheme: "http", host: host, port: port, ok: true}) {
				t.Fatal("must proceed")
			}
			if got := conns.Load(); got != 0 {
				t.Fatalf("%d connection(s) made to the app address with probing disabled. The "+
					"caller said a connect-then-close is destructive in its environment; a "+
					"gate that still dials tears down the very connection it is testing.", got)
			}
		})
	}
}

// ...and with the flag OFF the same gates still probe, so the opt-out is
// the only thing suppressing them.
func TestWaitForAppReady_ProbeConnectsWhenNotDisabled(t *testing.T) {
	host, port, conns := countingListener(t)
	cfg := gateCfg(700 * time.Millisecond)
	cfg.Command = "replay"
	cfg.CommandType = "docker-compose"
	cfg.Test.AppReadyProbeAddr = net.JoinHostPort(host, port)
	cfg.Test.Delay = 0

	if !waitForAppReady(context.Background(), zap.NewNop(), cfg,
		httpProbeTarget{scheme: "http", host: host, port: port, ok: true}) {
		t.Fatal("must proceed")
	}
	if got := conns.Load(); got == 0 {
		t.Fatal("no connection was made with probing ENABLED: the disable tests above would " +
			"then pass for the wrong reason")
	}
}

// And with the flag off, nothing changes — every gate that shipped in
// v3.6.41 still fires. This is the half the previous attempt got wrong:
// a guard keyed on command type silently dropped the gate for
// docker-start, docker run without a parseable -p, and compose without
// a published host port, all of which are gated on released v3.6.41.
func TestWaitForAppReady_DockerStartKeepsItsGate(t *testing.T) {
	host, port := acceptOnlyListener(t) // accepts TCP, never serves HTTP
	cfg := gateCfg(700 * time.Millisecond)
	cfg.Command = "docker start myapp" // no -p to parse
	cfg.CommandType = "docker-start"
	cfg.Test.Delay = 0

	start := time.Now()
	if !waitForAppReady(context.Background(), zap.NewNop(), cfg,
		httpProbeTarget{scheme: "http", host: host, port: port, ok: true}) {
		t.Fatal("must proceed")
	}
	if elapsed := time.Since(start); elapsed < 500*time.Millisecond {
		t.Fatalf("returned after %v — a docker-start app was declared ready with no HTTP "+
			"round-trip. Its ports were published at create time, so there is no -p to parse "+
			"and the resolved-test-target fallback is the only gate it has.", elapsed)
	}
}
