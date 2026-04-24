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
	// so elapsed >= failures * healthPollInterval. Allow a small tolerance
	// (50ms) to absorb scheduler jitter and ticker drift in CI — without
	// it this assertion occasionally fires even when the poll cadence is
	// correct.
	const jitterTolerance = 50 * time.Millisecond
	minExpected := time.Duration(failures)*healthPollInterval - jitterTolerance
	if elapsed < minExpected {
		t.Fatalf("expected elapsed >= %v (N*500ms - %v tolerance) after %d failures, got %v", minExpected, jitterTolerance, failures, elapsed)
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

// TestWaitForAppReady_CtxCanceledAtCeiling is the round-3 select-race
// guard. pollCtx is derived from ctx, so when the parent ctx is canceled
// at (or after) the poll ceiling both ctx.Done() and pollCtx.Done() are
// ready simultaneously — Go's select may pick either. If the pollCtx
// branch wins, the old code fell through to the fixed-delay fallback and
// returned true, incorrectly letting replay proceed after a user abort.
// The fix is to check ctx.Err() inside the pollCtx branch and return
// false when the parent context has been canceled. To reliably exercise
// that branch we set HealthPollTimeout == cancel delay so the two wakeups
// are tied; the assertion must hold regardless of which branch select
// picks.
func TestWaitForAppReady_CtxCanceledAtCeiling(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	// 50ms poll ceiling + 50ms cancel => both ctx.Done() and
	// pollCtx.Done() become ready at ~the same moment.
	cfg := newCfg(srv.URL, 50*time.Millisecond)
	cfg.Test.Delay = 10 // if the bug regresses, fallback sleep returns true after 10s

	logger := zap.NewNop()
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	start := time.Now()
	ok := waitForAppReady(ctx, logger, cfg)
	elapsed := time.Since(start)

	if ok {
		t.Fatalf("expected waitForAppReady to return false on user abort at ceiling boundary, got true (fell through to fallback)")
	}
	// If the bug regresses, elapsed would be ~10s (poll ceiling + full
	// Delay fallback). A comfortable ceiling of 2s catches that.
	if elapsed > 2*time.Second {
		t.Fatalf("ctx cancel should abort without fallback sleep; elapsed=%v suggests the fixed-delay branch ran", elapsed)
	}
}

// TestWaitForAppReady_MalformedURL_FailsFastOnInvalidURL locks in the
// fast-fail path for malformed --health-url inputs. Without the URL
// validation at the top of waitForAppReady, http.NewRequestWithContext
// inside httpHealthPoll would return an error on every probe and we
// would burn the entire HealthPollTimeout window (default 60s) plus
// the fixed Delay before returning — with zero actionable feedback to
// the operator. The fix: validate scheme + host up front and return
// immediately so a typo surfaces in milliseconds.
//
// To prove we never enter the poll loop we also swap out healthPoller
// with a stub that fails the test if called; if the pre-loop
// validation regresses this stub catches it even when the time-based
// bound would pass for other reasons.
func TestWaitForAppReady_MalformedURL_FailsFastOnInvalidURL(t *testing.T) {
	orig := healthPoller
	healthPoller = func(_ context.Context, _ string) bool {
		t.Fatalf("healthPoller must not be called when HealthURL is malformed — pre-loop validation should short-circuit")
		return false
	}
	defer func() { healthPoller = orig }()

	// HealthPollTimeout is large on purpose: if the validation regresses
	// and we fall into the poll loop, elapsed balloons and the <100ms
	// assertion below fires. Delay is also large so the fallback sleep
	// (if it ever runs) is visible to the assertion.
	cfg := newCfg("not-a-url", 30*time.Second)
	cfg.Test.Delay = 10

	core, logs := observer.New(zap.ErrorLevel)
	logger := zap.New(core)

	start := time.Now()
	ok := waitForAppReady(context.Background(), logger, cfg)
	elapsed := time.Since(start)

	if ok {
		t.Fatalf("expected waitForAppReady to return false on malformed URL, got true")
	}
	// The key win: fail fast. 100ms is generous for an in-process
	// url.Parse + scheme/host check; anything close to the poll
	// ceiling or fallback delay indicates the pre-loop guard is gone.
	if elapsed >= 100*time.Millisecond {
		t.Fatalf("expected <100ms fast-fail on malformed URL, elapsed=%v (did the validation regress?)", elapsed)
	}

	// Operator-facing log: must mention --health-url so grepping a CI log
	// actually tells the operator what to fix. Without this, the failure
	// mode is "keploy just returned false" with no context.
	var foundLog bool
	for _, entry := range logs.All() {
		if strings.Contains(entry.Message, "invalid --health-url") {
			foundLog = true
			break
		}
	}
	if !foundLog {
		t.Fatalf("expected ERROR log mentioning 'invalid --health-url', entries=%v", logs.All())
	}
}

// TestWaitForAppReady_MalformedURL_TableDriven covers the set of
// malformed inputs we explicitly want to reject at the boundary: no
// scheme, non-http scheme, empty host after scheme, and outright
// garbage. Each must return false in well under 100ms. If any one of
// these slips past validation and into the poll loop, the bug surfaces
// as either a timeout-sized elapsed or a healthPoller call.
func TestWaitForAppReady_MalformedURL_TableDriven(t *testing.T) {
	cases := []struct {
		name string
		url  string
	}{
		{"missing scheme", "localhost:8080/health"},
		{"bare host no scheme", "example.com"},
		{"scheme but no host", "http://"},
		{"ftp scheme not supported", "ftp://example.com/health"},
		{"garbage", "not a url at all"},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			orig := healthPoller
			healthPoller = func(_ context.Context, _ string) bool {
				t.Fatalf("healthPoller must not be called for malformed URL %q", c.url)
				return false
			}
			defer func() { healthPoller = orig }()

			cfg := newCfg(c.url, 10*time.Second)
			cfg.Test.Delay = 5
			logger := zap.NewNop()

			start := time.Now()
			ok := waitForAppReady(context.Background(), logger, cfg)
			elapsed := time.Since(start)

			if ok {
				t.Fatalf("expected false for malformed URL %q, got true", c.url)
			}
			if elapsed >= 100*time.Millisecond {
				t.Fatalf("expected <100ms fast-fail for %q, elapsed=%v", c.url, elapsed)
			}
		})
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
