package replay

import (
	"context"
	"errors"
	"net"
	"net/url"
	"os"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/agent/proxy"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// connResetURLErr builds the exact error chain net/http returns for a
// docker-proxy / mid-exchange "connection reset by peer":
// *url.Error -> *net.OpError(Op:"read") -> *os.SyscallError -> syscall.ECONNRESET.
func connResetURLErr() error {
	return &url.Error{Op: "Get", URL: "http://localhost:8080/", Err: &net.OpError{Op: "read", Net: "tcp", Err: os.NewSyscallError("read", syscall.ECONNRESET)}}
}

// realMockManagerHook delegates GetConsumedMocks to a REAL *proxy.MockManager
// so the safety gate is exercised against the genuine per-call / DRAINING
// semantics — not a stub that returns a fixed slice. SimulateRequest fails the
// first failFirst calls with a transport reset, then returns a 200; simErr,
// when set, makes every attempt fail with that error.
type realMockManagerHook struct {
	TestHooks
	mm        *proxy.MockManager
	failFirst int32
	calls     int32
	simErr    error
}

func (h *realMockManagerHook) SimulateRequest(_ context.Context, _ *models.TestCase, _ string) (interface{}, error) {
	n := atomic.AddInt32(&h.calls, 1)
	if h.simErr != nil {
		return nil, h.simErr
	}
	if n <= h.failFirst {
		return nil, connResetURLErr()
	}
	return &models.HTTPResp{StatusCode: 200}, nil
}

// GetConsumedMocks hits the real MockManager, so the gate sees true per-call
// draining: only what was consumed since the last drain, then the list clears.
func (h *realMockManagerHook) GetConsumedMocks(_ context.Context) ([]models.MockState, error) {
	return h.mm.GetConsumedMocks(), nil
}

// errConsumedHook always fails the consumed-mocks fetch — the safety gate must
// then refuse to retry (fail safe).
type errConsumedHook struct {
	TestHooks
	calls int32
}

func (h *errConsumedHook) SimulateRequest(_ context.Context, _ *models.TestCase, _ string) (interface{}, error) {
	atomic.AddInt32(&h.calls, 1)
	return nil, connResetURLErr()
}

func (h *errConsumedHook) GetConsumedMocks(_ context.Context) ([]models.MockState, error) {
	return nil, errors.New("agent unreachable")
}

func newTestReplayer(hook TestHooks) *Replayer {
	// A native command (no -p publish) makes dockerPublishedHostPort return
	// ok=false, so waitForResetResendReady is a no-op and these unit tests
	// exercise the safety gate without dialing a real port. The docker
	// port-readiness re-poll itself is covered by docker_port_readiness_test.go.
	return &Replayer{
		logger:   zap.NewNop(),
		config:   &config.Config{Command: "node app.js"},
		hookImpl: hook,
	}
}

// A reset with NO mock consumed (per-call drain returns empty) is re-sent and
// recovers the real response. Drives a REAL MockManager that recorded nothing.
func TestRetryResetOnce_RecoversWhenNoMockConsumed(t *testing.T) {
	mm := proxy.NewMockManager(nil, nil, zap.NewNop())
	defer mm.Close()
	// The caller's original request already reset (docker-proxy); the app saw
	// nothing, so the manager's consumed list is empty and the very first
	// re-send recovers the real response.
	hook := &realMockManagerHook{mm: mm, failFirst: 0}
	r := newTestReplayer(hook)
	tc := &models.TestCase{Name: "get-root-1", Kind: models.HTTP}

	resp, retried, drained := r.retryResetOnce(context.Background(), tc, "test-set-0", connResetURLErr())
	if !retried {
		t.Fatal("expected the reset to be re-sent and recovered")
	}
	if len(drained) != 0 {
		t.Fatalf("safe path must not carry drained mocks, got %#v", drained)
	}
	hr, ok := resp.(*models.HTTPResp)
	if !ok || hr.StatusCode != 200 {
		t.Fatalf("expected recovered 200 response, got %#v", resp)
	}
	if got := atomic.LoadInt32(&hook.calls); got != 1 {
		t.Fatalf("expected 1 re-send to recover, got %d", got)
	}
}

// A reset AFTER a mock was consumed (per-call drain returns >0) must NOT be
// retried — re-running would exhaust single-use mocks and fabricate a verdict.
// The gate must also hand back the mocks it DRAINED so the caller can keep
// totalConsumedMocks accurate. Driven by a REAL MockManager that recorded a
// consumption, so the >0 check reflects genuine per-call draining.
func TestRetryResetOnce_RefusesWhenMockConsumed(t *testing.T) {
	mm := proxy.NewMockManager(nil, nil, zap.NewNop())
	defer mm.Close()
	// The failed request DID burn a single-use mock before the mid-stream reset.
	if !mm.MarkMockAsUsed(models.Mock{Name: "mongo-1", Kind: models.Mongo}) {
		t.Fatal("failed to seed a consumed mock into the real MockManager")
	}
	hook := &realMockManagerHook{mm: mm, failFirst: 0}
	r := newTestReplayer(hook)
	tc := &models.TestCase{Name: "create-user-1", Kind: models.HTTP}

	resp, retried, drained := r.retryResetOnce(context.Background(), tc, "test-set-0", connResetURLErr())
	if retried {
		t.Fatalf("must NOT retry a reset that consumed a mock; got resp=%#v", resp)
	}
	if got := atomic.LoadInt32(&hook.calls); got != 0 {
		t.Fatalf("expected 0 re-sends (refused before re-issuing), got %d", got)
	}
	// The gate drained the consumed mock; it must be returned for the caller to
	// fold into totalConsumedMocks (else the agent re-serves it to a later test
	// via filterOutDeleted).
	if len(drained) != 1 || drained[0].Name != "mongo-1" {
		t.Fatalf("expected the drained consumed mock [mongo-1] to be returned, got %#v", drained)
	}
	// And the real manager's list is now drained, proving the gate consumed it.
	if leftover := mm.GetConsumedMocks(); len(leftover) != 0 {
		t.Fatalf("gate should have drained the consumed list; leftover=%#v", leftover)
	}
}

// If the consumed-mock count can't be fetched, fail safe: do not retry.
func TestRetryResetOnce_FailSafeOnConsumedFetchError(t *testing.T) {
	hook := &errConsumedHook{}
	r := newTestReplayer(hook)
	tc := &models.TestCase{Name: "get-root-1", Kind: models.HTTP}

	_, retried, drained := r.retryResetOnce(context.Background(), tc, "test-set-0", connResetURLErr())
	if retried {
		t.Fatal("must not retry when consumed-mock count is unknown")
	}
	if len(drained) != 0 {
		t.Fatalf("fail-safe path must not fabricate drained mocks, got %#v", drained)
	}
	if got := atomic.LoadInt32(&hook.calls); got != 0 {
		t.Fatalf("expected 0 re-sends on fetch error, got %d", got)
	}
}

// A persistently reset app stops after the re-send DEADLINE and reports
// not-retried, so the caller surfaces the original transport error (no
// fabricated success). Driven by a REAL MockManager that records nothing, so
// every per-call drain is empty and every attempt is deemed safe-to-resend. A
// short HealthPollTimeout makes the re-send budget small so the test is fast
// while still proving the loop is bounded by wall-clock (not the old fixed
// count) and terminates.
func TestRetryResetOnce_BoundedAndStopsWithoutFabricating(t *testing.T) {
	mm := proxy.NewMockManager(nil, nil, zap.NewNop())
	defer mm.Close()
	hook := &realMockManagerHook{mm: mm, failFirst: 1_000_000} // always resets
	r := newTestReplayer(hook)
	r.config.Test.HealthPollTimeout = 1500 * time.Millisecond // short re-send budget
	tc := &models.TestCase{Name: "get-root-1", Kind: models.HTTP}

	start := time.Now()
	resp, retried, drained := r.retryResetOnce(context.Background(), tc, "test-set-0", connResetURLErr())
	elapsed := time.Since(start)

	if retried || resp != nil {
		t.Fatalf("a persistently reset app must not yield a fabricated response; retried=%v resp=%#v", retried, resp)
	}
	if len(drained) != 0 {
		t.Fatalf("no mock was consumed, so nothing should be drained, got %#v", drained)
	}
	// Deadline-bounded: it must loop more than once (proving it is no longer
	// capped at the old fixed 6) yet still terminate near the budget.
	if got := atomic.LoadInt32(&hook.calls); got < 2 {
		t.Fatalf("expected the re-send to loop until the deadline (>=2 attempts), got %d", got)
	}
	if elapsed > 5*time.Second {
		t.Fatalf("a 1.5s re-send budget should bound the loop well under 5s, took %s", elapsed)
	}
}

// resetResendMaxWait defaults to the floor, passes a configured HealthPollTimeout
// through, clamps a very large one to the ceiling (so a crash-looping container
// can't hang a test for minutes), and is nil-config safe.
func TestResetResendMaxWait_FloorAndCeiling(t *testing.T) {
	cases := []struct {
		name string
		hpt  time.Duration
		want time.Duration
	}{
		{"unset uses floor", 0, resetResendFloor},
		{"negative uses floor", -1 * time.Second, resetResendFloor},
		{"below ceiling passes through", 45 * time.Second, 45 * time.Second},
		{"above ceiling clamps", 10 * time.Minute, resetResendCeiling},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := &Replayer{config: &config.Config{}}
			r.config.Test.HealthPollTimeout = tc.hpt
			if got := r.resetResendMaxWait(); got != tc.want {
				t.Fatalf("HealthPollTimeout=%s: want %s, got %s", tc.hpt, tc.want, got)
			}
		})
	}
	if got := (&Replayer{}).resetResendMaxWait(); got != resetResendFloor {
		t.Fatalf("nil config: want floor %s, got %s", resetResendFloor, got)
	}
}

// A non-reset error on a re-send stops immediately (lets the caller report the
// original reset) instead of looping.
func TestRetryResetOnce_StopsOnNonResetError(t *testing.T) {
	mm := proxy.NewMockManager(nil, nil, zap.NewNop())
	defer mm.Close()
	hook := &realMockManagerHook{mm: mm, simErr: errors.New("response body mismatch")}
	r := newTestReplayer(hook)
	tc := &models.TestCase{Name: "get-root-1", Kind: models.HTTP}

	_, retried, _ := r.retryResetOnce(context.Background(), tc, "test-set-0", connResetURLErr())
	if retried {
		t.Fatal("a non-reset failure on re-send must not be treated as a recovery")
	}
	if got := atomic.LoadInt32(&hook.calls); got != 1 {
		t.Fatalf("expected to stop after the first non-reset error, got %d calls", got)
	}
}

// Per-call gate semantics across the retry loop: the FIRST attempt's gate sees
// the failed request's (empty) consumption and proceeds; if a re-send then burns
// a mock and itself resets, the SECOND attempt's gate sees ONLY that attempt's
// consumption (>0) and refuses, returning exactly those drained mocks. This pins
// that each attempt checks only THAT attempt's per-call drain.
func TestRetryResetOnce_PerAttemptGateUsesPerCallDrain(t *testing.T) {
	mm := proxy.NewMockManager(nil, nil, zap.NewNop())
	defer mm.Close()
	hook := &perAttemptConsumerHook{mm: mm}
	r := newTestReplayer(hook)
	tc := &models.TestCase{Name: "get-root-1", Kind: models.HTTP}

	resp, retried, drained := r.retryResetOnce(context.Background(), tc, "test-set-0", connResetURLErr())
	if retried || resp != nil {
		t.Fatalf("the second attempt burned a mock then reset; must refuse, got retried=%v resp=%#v", retried, resp)
	}
	// Exactly one re-send happened (attempt 1 passed the empty-drain gate and
	// re-sent; attempt 2's gate saw the >0 drain and refused before re-sending).
	if got := atomic.LoadInt32(&hook.calls); got != 1 {
		t.Fatalf("expected exactly 1 re-send before the second gate refused, got %d", got)
	}
	if len(drained) != 1 || drained[0].Name != "attempt1-mock" {
		t.Fatalf("expected the second gate to drain only attempt 1's mock, got %#v", drained)
	}
}

// perAttemptConsumerHook makes its (only) re-send both consume a mock into the
// real MockManager AND reset, so the next gate observes a per-call drain of >0.
type perAttemptConsumerHook struct {
	TestHooks
	mm    *proxy.MockManager
	calls int32
}

func (h *perAttemptConsumerHook) SimulateRequest(_ context.Context, _ *models.TestCase, _ string) (interface{}, error) {
	atomic.AddInt32(&h.calls, 1)
	// This re-send burns a single-use mock before the connection resets.
	h.mm.MarkMockAsUsed(models.Mock{Name: "attempt1-mock", Kind: models.Mongo})
	return nil, connResetURLErr()
}

func (h *perAttemptConsumerHook) GetConsumedMocks(_ context.Context) ([]models.MockState, error) {
	return h.mm.GetConsumedMocks(), nil
}
