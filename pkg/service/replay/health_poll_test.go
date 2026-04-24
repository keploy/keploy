package replay

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"

	"go.keploy.io/server/v3/config"
)

// newCfg builds a minimal config.Config with the replay/health knobs set.
// A 1-second --delay fallback keeps the "timeout" test fast.
func newCfg(healthURL string, pollTimeout time.Duration) *config.Config {
	cfg := &config.Config{}
	cfg.Test.Delay = 1
	cfg.Test.HealthURL = healthURL
	cfg.Test.HealthPollTimeout = pollTimeout
	return cfg
}

// TestWaitForAppReady_200OnFirstTry verifies the happy path: a health endpoint
// that is up immediately unblocks replay in well under the --delay fallback
// window (and far under healthPollInterval).
func TestWaitForAppReady_200OnFirstTry(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	cfg := newCfg(srv.URL, 5*time.Second)
	logger := zap.NewNop()

	start := time.Now()
	ok := waitForAppReady(context.Background(), logger, cfg)
	elapsed := time.Since(start)

	if !ok {
		t.Fatalf("expected waitForAppReady to return true on 200, got false")
	}
	if elapsed >= time.Second {
		t.Fatalf("expected <1s proceed on first-try 200, elapsed=%v", elapsed)
	}
}

// TestWaitForAppReady_503ThenOK verifies we honor the poll cadence: after N
// failing probes the (N+1)th 2xx unblocks replay at roughly N * healthPollInterval
// wall time (lower bound). This proves the loop is actually retrying rather
// than short-circuiting.
func TestWaitForAppReady_503ThenOK(t *testing.T) {
	const failures int32 = 3
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n := atomic.AddInt32(&hits, 1)
		if n <= failures {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	cfg := newCfg(srv.URL, 10*time.Second)
	logger := zap.NewNop()

	start := time.Now()
	ok := waitForAppReady(context.Background(), logger, cfg)
	elapsed := time.Since(start)

	if !ok {
		t.Fatalf("expected waitForAppReady to eventually succeed, got false")
	}
	// Lower bound: N failing probes are spaced by healthPollInterval each,
	// so elapsed >= failures * healthPollInterval.
	minExpected := time.Duration(failures) * healthPollInterval
	if elapsed < minExpected {
		t.Fatalf("expected elapsed >= %v (N*500ms) after %d failures, got %v", minExpected, failures, elapsed)
	}
	// Upper bound guard: still well below any plausible fallback window.
	if elapsed > 5*time.Second {
		t.Fatalf("elapsed %v unexpectedly high; poll loop may be mis-cadenced", elapsed)
	}
	if got := atomic.LoadInt32(&hits); got < failures+1 {
		t.Fatalf("expected at least %d hits, got %d", failures+1, got)
	}
}

// TestWaitForAppReady_NeverOK verifies the fallback path: when the health
// endpoint never returns 2xx we log at INFO and sleep for --delay.
func TestWaitForAppReady_NeverOK(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	// Short HealthPollTimeout so the test finishes fast; Delay=1s fallback is the
	// observable lower bound after the ceiling elapses.
	cfg := newCfg(srv.URL, 300*time.Millisecond)

	core, logs := observer.New(zap.InfoLevel)
	logger := zap.New(core)

	start := time.Now()
	ok := waitForAppReady(context.Background(), logger, cfg)
	elapsed := time.Since(start)

	if !ok {
		t.Fatalf("expected waitForAppReady to proceed via fallback, got false")
	}
	// Must have waited at least pollCeiling + fallback Delay.
	if elapsed < 1200*time.Millisecond {
		t.Fatalf("expected fallback to wait >=~1.2s (ceiling + delay), got %v", elapsed)
	}

	found := false
	for _, entry := range logs.All() {
		if strings.Contains(entry.Message, "health probe timed out") {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected INFO 'health probe timed out' log, entries=%v", logs.All())
	}
}

// TestWaitForAppReady_EmptyURLUsesFixedDelay locks in the zero-change default:
// an empty HealthURL must reproduce the previous time.After(Delay) behavior and
// never touch the network.
func TestWaitForAppReady_EmptyURLUsesFixedDelay(t *testing.T) {
	// Point healthPoller at a stub that would fail the test if called, to
	// prove the empty-URL branch never invokes the poller.
	orig := healthPoller
	healthPoller = func(_ context.Context, _ string) bool {
		t.Fatalf("healthPoller must not be called when HealthURL is empty")
		return false
	}
	defer func() { healthPoller = orig }()

	cfg := newCfg("", 60*time.Second)
	cfg.Test.Delay = 0 // keep the test fast; semantics are the same
	logger := zap.NewNop()

	ok := waitForAppReady(context.Background(), logger, cfg)
	if !ok {
		t.Fatalf("expected waitForAppReady to return true in the default path")
	}
}

// TestWaitForAppReady_CtxCanceled verifies ctx cancellation is honored during
// the poll loop — caller should see false so it can return TestSetStatusUserAbort.
func TestWaitForAppReady_CtxCanceled(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	cfg := newCfg(srv.URL, 10*time.Second)
	logger := zap.NewNop()

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(200 * time.Millisecond)
		cancel()
	}()

	start := time.Now()
	ok := waitForAppReady(ctx, logger, cfg)
	elapsed := time.Since(start)

	if ok {
		t.Fatalf("expected waitForAppReady to return false on ctx cancel, got true")
	}
	if elapsed > 2*time.Second {
		t.Fatalf("ctx cancel should unblock quickly, elapsed=%v", elapsed)
	}
}
