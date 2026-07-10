// Package proxy — regression coverage for the proxyless (skipListener)
// owned-window flush wiring in start().
//
// In proxyless (low-latency) record mode there is no TCP accept loop, so the
// listener path's *periodic* FlushOwnedWindows never runs. An outbound mock
// that lands in the syncMock buffer AFTER its ingress window resolved (the
// async egress-parse-vs-ingress-resolve race, or a keep-alive outbound call
// made while a request is being served) would then sit unflushed until
// shutdown, where the cancelled recorder ctx drops it — silently losing every
// post-first-request outbound mock. The fix wires two drains onto the
// skipListener path: a periodic ticker calling FlushOwnedWindows while
// recording is live, and one final FlushOwnedWindows on ctx cancellation. A
// regression that deletes either compiles cleanly and silently reintroduces
// the lost-mock bug, so both aspects are pinned here.
//
// These tests drive the real package-global syncMock manager (syncMock.Get(),
// the same one the fix flushes) through its public API: resolve a window, then
// AddMock a per-test mock whose request timestamp falls inside that
// already-resolved window so it lands in the buffer post-resolution — exactly
// the mock that only FlushOwnedWindows can rescue.
package proxy

import (
	"context"
	"testing"
	"time"

	syncMock "go.keploy.io/server/v3/pkg/agent/proxy/syncMock"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// seedLateOwnedMock wires the package-global syncMock manager for a proxyless
// recording session and plants a per-test mock that landed in the buffer AFTER
// its ingress window resolved — the exact post-resolution race the skipListener
// flush loop exists to drain. It returns the freshly-bound out channel; the
// planted mock carries the given connID so awaitMock can pick it out of any
// residue left by an earlier test sharing the process-global singleton.
func seedLateOwnedMock(t *testing.T, connID string) chan *models.Mock {
	t.Helper()
	mgr := syncMock.Get()

	ch := make(chan *models.Mock, 16)
	mgr.SetOutputChannel(ch)
	mgr.SetFirstRequestSignaled()

	now := time.Now()
	winStart, winEnd := now.Add(-1*time.Second), now
	// Resolve the window while the buffer holds nothing of ours, so the mock
	// added next is genuinely post-resolution (retro-bin territory, not a live
	// window match inside ResolveRange).
	mgr.ResolveRange(winStart, winEnd, "skiplistener-"+connID, true, false)

	// Untagged Mongo => LifetimePerTest, so with firstReqSeen already set the
	// mock rides the buffer and is drainable only via the owned-window
	// retro-bin in FlushOwnedWindows — never the session/connection carve-out.
	// Its request timestamp sits inside the just-resolved window.
	mgr.AddMock(&models.Mock{
		Kind:         models.Mongo,
		ConnectionID: connID,
		Spec:         models.MockSpec{ReqTimestampMock: now.Add(-500 * time.Millisecond)},
	})
	return ch
}

// awaitMock drains ch until it sees a mock tagged with connID (returning true)
// or the timeout elapses (returning false). Mocks that aren't ours — residue
// flushed from the shared singleton by an earlier test — are skipped. It
// returns as soon as our mock arrives, so a generous timeout costs nothing on
// the success path but keeps the failure path deterministic.
func awaitMock(t *testing.T, ch <-chan *models.Mock, connID string, timeout time.Duration) bool {
	t.Helper()
	deadline := time.After(timeout)
	for {
		select {
		case m := <-ch:
			if m != nil && m.ConnectionID == connID {
				return true
			}
		case <-deadline:
			return false
		}
	}
}

// TestRunSkipListenerFlush_PeriodicDrainsLateMock pins the PERIODIC drain: a
// mock that landed in the buffer after its window resolved must reach the out
// channel from a ticker tick alone, WITHOUT the session ever being cancelled.
// The helper runs with a short interval and the mock is asserted before any
// cancellation, so if the `case <-ticker.C` flush is deleted the mock never
// arrives and the test times out.
func TestRunSkipListenerFlush_PeriodicDrainsLateMock(t *testing.T) {
	ch := seedLateOwnedMock(t, "late-periodic")

	ctx, cancel := context.WithCancel(context.Background())

	// Join the flush goroutine before the test returns. runSkipListenerFlush
	// drains the process-global syncMock singleton, so a leaked ticker would
	// keep flushing into a later test and drain its freshly-seeded mock —
	// cross-test flakiness under -race. Mirror the sibling tests' done channel;
	// cancel is cleanup only and the assert below must not depend on it.
	done := make(chan struct{})
	p := &Proxy{logger: zap.NewNop()}
	go func() {
		p.runSkipListenerFlush(ctx, 5*time.Millisecond)
		close(done)
	}()
	defer func() { cancel(); <-done }()

	// Drained by a periodic tick while the session is still live. The 2 s
	// ceiling is the failure bound only; on success awaitMock returns in ~one
	// tick.
	if !awaitMock(t, ch, "late-periodic", 2*time.Second) {
		t.Fatal("periodic FlushOwnedWindows did not drain the post-resolution mock; the skipListener ticker flush is missing")
	}
}

// TestRunSkipListenerFlush_FinalFlushOnCancelDrainsLateMock pins the FINAL
// shutdown drain: with an interval long enough that no periodic tick can fire
// during the test, the buffered post-resolution mock must be flushed by the
// ctx.Done branch when the session is cancelled. If the final
// FlushOwnedWindows is deleted, cancellation returns without draining and the
// mock is lost — the test times out.
func TestRunSkipListenerFlush_FinalFlushOnCancelDrainsLateMock(t *testing.T) {
	ch := seedLateOwnedMock(t, "late-final")

	ctx, cancel := context.WithCancel(context.Background())

	// Interval far longer than the test's lifetime: the ONLY drain that can
	// fire is the ctx.Done final flush.
	done := make(chan struct{})
	p := &Proxy{logger: zap.NewNop()}
	go func() {
		p.runSkipListenerFlush(ctx, time.Hour)
		close(done)
	}()

	// Nothing should have drained our mock before cancellation.
	if awaitMock(t, ch, "late-final", 50*time.Millisecond) {
		t.Fatal("mock drained before shutdown; long interval should have suppressed any periodic tick")
	}

	cancel()

	if !awaitMock(t, ch, "late-final", 2*time.Second) {
		t.Fatal("final FlushOwnedWindows on ctx.Done did not drain the post-resolution mock; the skipListener shutdown flush is missing")
	}

	// The loop must return promptly after cancellation (guards against the
	// final flush blocking or the loop failing to observe ctx.Done).
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("runSkipListenerFlush did not return after ctx cancellation")
	}
}

// TestStartSkipListenerDrainsLateMockOnShutdown drives the REAL start() on the
// skipListener path end-to-end: it proves start() actually wires the flush loop
// (not just that the loop works in isolation). start() runs with the production
// 1 s cadence; the session is cancelled well within that window, so the drain
// can only come from the shutdown final flush. If start() stops calling
// runSkipListenerFlush, or the final flush regresses, the mock is never
// emitted.
func TestStartSkipListenerDrainsLateMockOnShutdown(t *testing.T) {
	ch := seedLateOwnedMock(t, "late-start")

	p := &Proxy{logger: zap.NewNop()}
	p.skipListener = true // no TCP listener; empty nsswitchData skips the reset.

	ctx, cancel := context.WithCancel(context.Background())
	readyChan := make(chan error, 1)
	startErr := make(chan error, 1)
	go func() { startErr <- p.start(ctx, readyChan) }()

	// start() signals readiness before entering the flush loop.
	select {
	case err := <-readyChan:
		if err != nil {
			t.Fatalf("start() reported not-ready: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("start() never signalled readiness")
	}

	// Cancel far inside the 1 s tick, so only the shutdown final flush can
	// drain the buffered mock.
	cancel()

	if !awaitMock(t, ch, "late-start", 2*time.Second) {
		t.Fatal("start() skipListener path did not drain the post-resolution mock on shutdown")
	}

	select {
	case err := <-startErr:
		if err != nil {
			t.Fatalf("start() returned error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("start() did not return after ctx cancellation")
	}
}
