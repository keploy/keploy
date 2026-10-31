package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.keploy.io/server/v3/config"
	coreAgent "go.keploy.io/server/v3/pkg/agent"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

// countHooks are hooks that count, in the kernel, the connections completed
// to the proxy and the application's own connections to it
// (coreAgent.ProxyConnsReporter): established, appConns of which appSettled
// are paired or lost, or err.
type countHooks struct {
	// countingHooks: the capture-loss counts (none lost) the wire reads too.
	countingHooks
	mu                    sync.Mutex
	established, appConns uint64
	appSettled            uint64
	err                   error
	sig                   chan struct{}
}

func (h *countHooks) ProxyConnsEstablished(context.Context) (uint64, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.established, h.err
}

func (h *countHooks) AppConnsEstablished(context.Context) (uint64, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.appConns, h.err
}

func (h *countHooks) AppConnsSettled() (uint64, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.appSettled, h.err
}

func (h *countHooks) WaitAppConnsSettled(ctx context.Context, n uint64) error {
	for {
		h.mu.Lock()
		if h.sig == nil {
			h.sig = make(chan struct{})
		}
		changed, settled, err := h.sig, h.appSettled, h.err
		h.mu.Unlock()
		if err != nil {
			return err
		}
		if settled >= n {
			return nil
		}
		blockedHere()
		select {
		case <-changed:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// setApp changes the application's connections, as a connect() or a pairing
// would.
func (h *countHooks) setApp(conns, settled uint64) {
	h.mu.Lock()
	h.appConns, h.appSettled = conns, settled
	if h.sig != nil {
		close(h.sig)
		h.sig = nil
	}
	h.mu.Unlock()
}

// The doubles are what the agent reads: a change to the counters'
// interfaces breaks this build, not a test's count at run time.
var (
	_ coreAgent.ProxyConnsReporter = &countHooks{}
	_ coreAgent.ProxyConnsReporter = countingHooks{}
	_ coreAgent.ProxyConnsReporter = uncountedHooks{}
	_ coreAgent.AcceptCounter      = &settledProxy{}
	_ coreAgent.InFlightReader     = &settledProxy{}
	_ coreAgent.PairedBytesWaiter  = &settledProxy{}
)

// settledProxy is the part of a proxy a test's settling reads
// (coreAgent.AcceptCounter, coreAgent.InFlightReader): how many connections
// it accepted, and how many still hold the test's work. Its zero value holds
// nothing: every call is in hand and answered.
type settledProxy struct {
	smu      sync.Mutex
	accepted uint64
	pending  int
	progress uint64
	sig      chan struct{}
	// unknown: protocols whose held requests cannot be told.
	unknown []string
	// drops: the packets the kernel dropped at the listener; netDrops: its
	// network namespace's ListenDrops, and netUnread why they cannot be read.
	drops, netDrops uint64
	netUnread       string
	// bytesWait, when set, is WaitPairedBytes; bytesProgress its progress.
	bytesWait     func(context.Context, uint64) error
	bytesProgress atomic.Uint64
	// inWork, when set, is closed at the settle's first look at the
	// connections' work: its wait for the counts (WaitProxySettled) is over.
	inWork   chan struct{}
	workOnce sync.Once
}

func (s *settledProxy) InFlightState(answers bool) (int, []string) {
	return s.InFlightPending(answers), s.InFlightUnknown(answers)
}

func (s *settledProxy) InFlightUnknown(answers bool) []string {
	s.smu.Lock()
	defer s.smu.Unlock()
	if !answers {
		return nil
	}
	return append([]string(nil), s.unknown...)
}

func (s *settledProxy) Accepted() (uint64, error) {
	s.smu.Lock()
	defer s.smu.Unlock()
	return s.accepted, nil
}

func (s *settledProxy) ListenDrops() (models.ListenDrops, error) {
	s.smu.Lock()
	defer s.smu.Unlock()
	return models.ListenDrops{Listener: s.drops, Net: s.netDrops, NetKnown: s.netUnread == "", NetUnread: s.netUnread}, nil
}

func (s *settledProxy) WaitAccepted(ctx context.Context, n uint64) error {
	for {
		changed := s.InFlightChanged()
		if acc, _ := s.Accepted(); acc >= n {
			return nil
		}
		blockedHere()
		select {
		case <-changed:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// WaitPairedBytes: it holds no paired connection with bytes still on their
// way, unless the test set bytesWait.
func (s *settledProxy) WaitPairedBytes(ctx context.Context, n uint64) error {
	if s.bytesWait != nil {
		return s.bytesWait(ctx, n)
	}
	return nil
}

// PairedBytesProgress is what the test set bytesProgress to.
func (s *settledProxy) PairedBytesProgress() uint64 { return s.bytesProgress.Load() }

func (s *settledProxy) InFlightPending(bool) int {
	s.smu.Lock()
	defer s.smu.Unlock()
	if s.inWork != nil {
		s.workOnce.Do(func() { close(s.inWork) })
	}
	if s.pending > 0 {
		blockedHere()
	}
	return s.pending
}

func (s *settledProxy) InFlightChanged() <-chan struct{} {
	s.smu.Lock()
	defer s.smu.Unlock()
	if s.sig == nil {
		s.sig = make(chan struct{})
	}
	return s.sig
}

func (s *settledProxy) InFlightPendingProgress(bool) map[uint64]uint64 {
	s.smu.Lock()
	defer s.smu.Unlock()
	if s.pending == 0 {
		return map[uint64]uint64{}
	}
	// One connection holds the work; its progress is the proxy's.
	return map[uint64]uint64{1: s.progress}
}

// set changes what the proxy holds, as an accept or a read would.
func (s *settledProxy) set(accepted uint64, pending int) {
	s.smu.Lock()
	s.accepted, s.pending = accepted, pending
	s.progress++
	if s.sig != nil {
		close(s.sig)
		s.sig = nil
	}
	s.smu.Unlock()
}

// settleProxy is a proxy that holds only what settledProxy says.
type settleProxy struct {
	coreAgent.Proxy
	settledProxy
}

func settleAgent() (*Agent, *countHooks, *settleProxy) {
	h, p := &countHooks{}, &settleProxy{}
	return &Agent{logger: zap.NewNop(), Hooks: h, Proxy: p}, h, p
}

// manualWake makes the watchdog wake only when the test says: each send on
// the returned channel is one wake-up, and returns once the watchdog has
// weighed it.
func manualWake(t *testing.T) func() {
	t.Helper()
	wake := make(chan struct{})
	woken := make(chan struct{})
	prevWake, prevWoken := settleWake, settleWoken
	settleWake = func(context.Context) (<-chan struct{}, func()) {
		return wake, func() {}
	}
	settleWoken = func() { woken <- struct{}{} }
	t.Cleanup(func() { settleWake, settleWoken = prevWake, prevWoken })
	return func() {
		wake <- struct{}{}
		<-woken
	}
}

// The settling waits for the proxy to accept every connection the kernel
// completed to it, then for no connection to hold the test's work, and ends
// on those events alone.
func TestSettleWaitsForTheAcceptsThenTheWork(t *testing.T) {
	_ = manualWake(t)
	a, h, p := settleAgent()
	h.established = 3
	p.set(1, 2)
	done := make(chan error, 1)
	go func() {
		_, err := a.settle(context.Background(), true)
		done <- err
	}()
	p.set(3, 2)
	p.set(3, 1)
	select {
	case err := <-done:
		t.Fatalf("settled with work still in hand: %v", err)
	default:
	}
	p.set(3, 0)
	require.NoError(t, <-done)
}

// A count keploy cannot read is unknown, never "nothing in flight".
func TestSettleWithoutTheCountIsUnknown(t *testing.T) {
	a, h, _ := settleAgent()
	h.err = models.ErrProxyConnsUnsupported
	_, err := a.settle(context.Background(), true)
	require.ErrorIs(t, err, models.ErrProxyConnsUnsupported)
	a.Hooks = nil
	_, err = a.settle(context.Background(), true)
	require.ErrorIs(t, err, models.ErrProxyConnsUnsupported)
	a, _, _ = settleAgent()
	a.Proxy = struct{ coreAgent.Proxy }{}
	_, err = a.settle(context.Background(), true)
	require.ErrorContains(t, err, "cannot tell which calls it still holds")
}

// The watchdog fails the wait after settleStallLimit wake-ups in a row with
// no progress, and a sign of progress starts its count again: it counts
// wake-ups, never time.
func TestTheSettleWatchdogCountsWakeUpsWithoutProgress(t *testing.T) {
	wake := manualWake(t)
	prev := settleStallLimit
	settleStallLimit = 3
	t.Cleanup(func() { settleStallLimit = prev })
	a, h, p := settleAgent()
	h.established = 1
	p.set(1, 1)
	p.inWork = make(chan struct{})
	done := make(chan error, 1)
	go func() {
		_, err := a.settle(context.Background(), true)
		done <- err
	}()
	// The wake-ups below weigh the connection's work: the wait for the
	// counts, whose progress is another key, must be over first.
	<-p.inWork
	wake()
	wake()
	p.set(1, 1) // progress, still pending
	wake()      // weighs the progress: the count starts again
	wake()
	wake()
	select {
	case err := <-done:
		t.Fatalf("the watchdog fired before its count: %v", err)
	default:
	}
	wake()
	err := <-done
	require.True(t, errors.Is(err, errNoProgress), "%v", err)
}

// While the wait is for the bytes on the connections the proxy holds (the
// counts settled: nothing more to accept or resolve), their arrivals are the
// progress the watchdog weighs: a wait whose bytes keep arriving is never
// failed for want of a count that moves, and one whose bytes stop is, at
// settleStallLimit wake-ups.
func TestTheSettleWatchdogWeighsThePairedBytes(t *testing.T) {
	// The watchdog wakes when the test says, as manualWake has it, but a
	// wake-up it can no longer take (it fired, and stopped) ends the test
	// with the settle's answer instead of blocking.
	wakeCh, woken := make(chan struct{}), make(chan struct{})
	prevWake, prevWoken := settleWake, settleWoken
	settleWake = func(context.Context) (<-chan struct{}, func()) { return wakeCh, func() {} }
	settleWoken = func() { woken <- struct{}{} }
	t.Cleanup(func() { settleWake, settleWoken = prevWake, prevWoken })
	prev := settleStallLimit
	settleStallLimit = 3
	t.Cleanup(func() { settleStallLimit = prev })
	a, h, p := settleAgent()
	h.established = 1
	p.set(1, 0)
	inBytes := make(chan struct{})
	p.bytesWait = func(ctx context.Context, _ uint64) error {
		close(inBytes)
		<-ctx.Done()
		return ctx.Err()
	}
	done := make(chan error, 1)
	go func() {
		_, err := a.settle(context.Background(), true)
		done <- err
	}()
	wake := func(what string) {
		select {
		case wakeCh <- struct{}{}:
			<-woken
		case err := <-done:
			t.Fatalf("the watchdog stopped %s: the settle ended with %v", what, err)
		}
	}
	<-inBytes
	for i := 0; i < 2*settleStallLimit; i++ {
		p.bytesProgress.Add(1) // bytes arrived
		wake("while the bytes kept arriving")
	}
	for i := 0; i < settleStallLimit; i++ {
		wake("before its count")
	}
	err := <-done
	require.True(t, errors.Is(err, errNoProgress), "%v", err)
}

// The settling waits first for every application connection whose connect()
// had returned to be paired with the proxy's side of it (or lost): one whose
// final ACK has not reached the proxy is in no proxy-side count yet. Then it
// waits for the proxy to accept what the kernel completed by then.
func TestSettleWaitsForTheApplicationsConnectionsFirst(t *testing.T) {
	_ = manualWake(t)
	a, h, p := settleAgent()
	h.mu.Lock()
	h.established, h.appConns, h.appSettled = 1, 2, 1
	h.mu.Unlock()
	p.set(1, 0)
	done := make(chan error, 1)
	go func() {
		_, err := a.settle(context.Background(), true)
		done <- err
	}()
	select {
	case err := <-done:
		t.Fatalf("settled while an application connection had not reached the proxy: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	// The connection's final ACK arrives: paired, and completed on the
	// proxy's side, which has not accepted it yet.
	h.mu.Lock()
	h.established = 2
	h.mu.Unlock()
	h.setApp(2, 2)
	select {
	case err := <-done:
		t.Fatalf("settled before the proxy accepted the paired connection: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	p.set(2, 0)
	require.NoError(t, <-done)
}

// The watchdog judges each thing the wait waits on apart: a connection that
// holds work and does not move fails the wait at settleStallLimit wake-ups,
// though another one moves at every wake-up (review S1: one stuck connection
// and unrelated traffic never tripped it). One that stops being waited on
// drops out.
func TestTheSettleWatchdogFiresForTheOneThatDoesNotMove(t *testing.T) {
	wake := manualWake(t)
	prev := settleStallLimit
	settleStallLimit = 3
	t.Cleanup(func() { settleStallLimit = prev })
	a := &Agent{logger: zap.NewNop()}
	var mu sync.Mutex
	busy := uint64(0)
	waiting := map[string]bool{"stuck": true, "busy": true, "gone": true}
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	stop := a.watchSettle(ctx, cancel, func() map[string]uint64 {
		mu.Lock()
		defer mu.Unlock()
		busy++
		out := map[string]uint64{}
		if waiting["stuck"] {
			out["stuck"] = 7
		}
		if waiting["busy"] {
			out["busy"] = busy
		}
		if waiting["gone"] {
			out["gone"] = 1
		}
		return out
	})
	defer stop()
	wake()
	wake()
	mu.Lock()
	waiting["gone"] = false // no longer waited on: its count is dropped
	mu.Unlock()
	select {
	case <-ctx.Done():
		t.Fatal("the watchdog fired before the stuck connection reached its count")
	default:
	}
	wake()
	require.ErrorIs(t, context.Cause(ctx), errNoProgress, "a stuck connection beside a busy one")
}

// chargeDrops charges a window the drops that can lose a connection: those
// that raised both the listener's count and its namespace's, never one that
// raised the listener's alone (an abandoned connect, a retransmitted SYN) or
// the namespace's alone (another listener). A drop whose two rises land on
// either side of a window's end is charged to the next window, never to
// none; what a window leaves unpaired is carried one window only. With the
// namespace's count unread, or a count that went back, every rise counts.
func TestChargeDropsChargesWhatCanLoseAConnection(t *testing.T) {
	known := func(l, n uint64) models.ListenDrops { return models.ListenDrops{Listener: l, Net: n, NetKnown: true} }
	unread := func(l uint64) models.ListenDrops { return models.ListenDrops{Listener: l, NetUnread: "no"} }
	// A step is a window to its end's counts, now, and what it is charged;
	// gap says it is no test's (the gap from a test's end to the next begin).
	type step struct {
		now  models.ListenDrops
		want uint64
		gap  bool
	}
	for _, c := range []struct {
		name  string
		start models.ListenDrops
		steps []step
	}{
		{"an accept queue full at a SYN", known(3, 9), []step{{known(4, 10), 1, false}}},
		{"an accept queue full at the final ACK: two at the listener, one in the namespace", known(0, 0), []step{{known(2, 1), 1, false}}},
		{"five abandoned connects", known(0, 0), []step{{known(5, 0), 0, false}}},
		{"another listener of the namespace", known(0, 0), []step{{known(0, 3), 0, false}}},
		{"abandoned connects beside a real drop", known(0, 0), []step{{known(7, 1), 1, false}}},
		{"a real drop beside another listener's: more, never fewer", known(0, 0), []step{{known(2, 3), 2, false}}},
		{"the listener's rise first, the namespace's after the end", known(0, 0), []step{{known(1, 0), 0, false}, {known(1, 1), 1, false}}},
		{"the namespace's rise first, the listener's after the end", known(0, 0), []step{{known(0, 1), 0, false}, {known(1, 1), 1, false}}},
		{"what a test's own rises leave unpaired is carried one window only", known(0, 0), []step{{known(1, 0), 0, false}, {known(1, 0), 0, false}, {known(1, 1), 0, false}}},
		{"the listener's rise in a test, the namespace's after the next begin: the next test's", known(0, 0), []step{{known(1, 0), 0, false}, {known(1, 0), 0, true}, {known(1, 1), 1, false}}},
		{"the namespace's rise in a test, the listener's after the next begin: the next test's", known(0, 0), []step{{known(0, 1), 0, false}, {known(0, 1), 0, true}, {known(1, 1), 1, false}}},
		{"one rise in the gap, the other after the begin: the test's", known(0, 0), []step{{known(0, 1), 0, true}, {known(1, 1), 1, false}}},
		{"the listener's rise in a test, the namespace's in the gap after it: the gap's, which the next test is charged", known(0, 0), []step{{known(1, 0), 0, false}, {known(1, 1), 1, true}, {known(1, 1), 0, false}}},
		{"the namespace's rise in a test, the listener's in the gap after it: the gap's", known(0, 0), []step{{known(0, 1), 0, false}, {known(1, 1), 1, true}, {known(1, 1), 0, false}}},
		{"a drop in the gap: the gap's", known(0, 0), []step{{known(0, 0), 0, false}, {known(1, 1), 1, true}, {known(1, 1), 0, false}}},
		{"what a test left unpaired passes the gap and pairs with nothing after the next test", known(0, 0), []step{{known(1, 0), 0, false}, {known(1, 0), 0, true}, {known(1, 0), 0, false}, {known(1, 0), 0, true}, {known(1, 1), 0, false}}},
		{"the gap passes on what it was carried and its own, whole", known(0, 0), []step{{known(2, 0), 0, false}, {known(3, 0), 0, true}, {known(3, 3), 3, false}}},
		{"a charged drop leaves nothing to pair again", known(0, 0), []step{{known(1, 1), 1, false}, {known(1, 1), 0, false}}},
		{"the namespace unread at the start", unread(0), []step{{known(1, 0), 1, false}}},
		{"the namespace unread at the end", known(0, 0), []step{{unread(2), 2, false}}},
		{"the namespace unread after a window left the listener's rise unpaired", known(0, 0), []step{{known(1, 0), 0, false}, {unread(1), 1, false}}},
		{"the listener's count went back", known(5, 5), []step{{known(0, 5), 1, false}}},
		{"the namespace's count went back", known(5, 5), []step{{known(5, 0), 1, false}}},
		{"the listener's count went back, the namespace unread", unread(5), []step{{unread(2), 2, false}}},
		{"the listener's count went back to none, the namespace unread: at least one", unread(5), []step{{unread(0), 1, false}}},
	} {
		t.Run(c.name, func(t *testing.T) {
			was, carry := c.start, dropCarry{}
			for i, s := range c.steps {
				var got uint64
				got, carry = chargeDrops(was, s.now, carry, s.gap)
				if got != s.want {
					t.Fatalf("window %d, %+v to %+v: charged %d, want %d", i+1, was, s.now, got, s.want)
				}
				was = s.now
			}
		})
	}
}

// A test's end waits for what the application had sent before it began: an
// application connection that never reaches the proxy, or bytes written to
// one that never arrive, hold the wait, and the watchdog fails it at
// settleStallLimit wake-ups however many connections the application opens
// after the end began (an app whose timers make an unrecorded call, take its
// miss, have the connection reset and connect again, every few seconds, as
// api-server's do). Those connections are no part of the wait: the kernel
// numbers each one after the ones the end read, pairs it, and the proxy
// accepts it. Before, their pairings and accepts were the wait's progress, and
// the end waited for as long as the application kept connecting: 31 minutes,
// until go test's -timeout ended the run.
//
// With the proven switch off the watchdog counts as it always did: every
// accept and application connection is the wait's progress.
func TestTheSettleWatchdogIsNotFedByConnectionsOpenedAfterTheEndBegan(t *testing.T) {
	for _, proven := range []bool{true, false} {
		t.Run(map[bool]string{true: "switch on", false: "switch off"}[proven], func(t *testing.T) {
			testSettleWatchdogNotFed(t, proven)
		})
	}
}

func testSettleWatchdogNotFed(t *testing.T, proven bool) {
	t.Setenv(provenswitch.Env, map[bool]string{true: "1", false: ""}[proven])
	for _, tc := range []struct {
		name string
		// connStuck: the end's second application connection never
		// reaches the proxy; otherwise the bytes written to its first never
		// arrive.
		connStuck bool
	}{
		{"an application connection that never reaches the proxy", true},
		{"bytes on a pooled connection that never arrive", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			wake, woken := make(chan struct{}), make(chan struct{})
			prevWake, prevWoken, prevLimit := settleWake, settleWoken, settleStallLimit
			settleWake = func(context.Context) (<-chan struct{}, func()) { return wake, func() {} }
			settleWoken = func() { woken <- struct{}{} }
			settleStallLimit = 3
			t.Cleanup(func() { settleWake, settleWoken, settleStallLimit = prevWake, prevWoken, prevLimit })
			a, h, p := settleAgent()
			h.mu.Lock()
			if tc.connStuck {
				h.established, h.appConns, h.appSettled = 1, 2, 1
			} else {
				h.established, h.appConns, h.appSettled = 1, 1, 1
				p.bytesWait = func(ctx context.Context, _ uint64) error {
					<-ctx.Done()
					return ctx.Err()
				}
			}
			h.mu.Unlock()
			p.set(1, 0)
			ctx, cancel := context.WithCancel(context.Background())
			done, exited := make(chan error, 1), make(chan struct{})
			go func() {
				defer close(exited)
				_, err := a.settle(ctx, true)
				done <- err
			}()
			// The settle still waiting as the row ends (with the switch
			// off, a wait that moves) is ended and waited for before the
			// watchdog's hooks are put back: none outlives the row.
			t.Cleanup(func() {
				cancel()
				<-exited
			})
			for i := 0; i < 10*settleStallLimit; i++ {
				select {
				case err := <-done:
					require.True(t, proven, "with the switch off the watchdog failed the wait while connections moved: %v", err)
					require.ErrorIs(t, err, errNoProgress, "what the end waits for fails the wait, at the watchdog's count")
					require.GreaterOrEqual(t, i, settleStallLimit, "the watchdog failed the wait before its count")
					return
				case wake <- struct{}{}:
					<-woken
				}
				// Between two wake-ups the application opens one more
				// connection, after the end began: the kernel completes
				// it to the proxy and pairs it, and the proxy accepts
				// it. A stuck application connection keeps the settled
				// count where it was (every connection numbered up to
				// it).
				h.mu.Lock()
				h.established++
				h.appConns++
				if !tc.connStuck {
					h.appSettled++
				}
				est := h.established
				h.mu.Unlock()
				p.set(est, 0)
			}
			if !proven {
				return // as before: they are the wait's progress
			}
			t.Fatalf("the end still waited after %d wake-ups at which only connections opened after it began moved: the watchdog counted them as the wait's progress", 10*settleStallLimit)
		})
	}
}

// The accepts the end waits for are the wait's progress: the watchdog counts
// each one, so a proxy that accepts the end's connections one by one, slower
// than settleStallLimit wake-ups in all, is no stuck wait. Each wake-up is
// sent once the settle is blocked waiting for the next accept (stallWake),
// so it is weighed against the accepts the wait waits for.
func TestTheSettleWatchdogWeighsTheAcceptsTheEndWaitsFor(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	w := stallWake(t)
	wake, woken := w.wake, w.woken
	a, h, p := settleAgent()
	// The kernel completed 5 connections to the proxy; it has accepted 1.
	h.mu.Lock()
	h.established = 5
	h.mu.Unlock()
	p.set(1, 0)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	done := make(chan error, 1)
	go func() {
		_, err := a.settle(ctx, true)
		done <- err
	}()
	for acc := uint64(2); acc <= 5; acc++ {
		select {
		case err := <-done:
			require.NoError(t, err, "the proxy accepted one more of the end's connections at every wake-up")
			t.Fatalf("the end settled with %d of the 5 connections it waited for accepted", acc-1)
		case <-w.blocked:
		}
		select {
		case err := <-done:
			require.NoError(t, err, "the proxy accepted one more of the end's connections at every wake-up")
			t.Fatalf("the end settled with %d of the 5 connections it waited for accepted", acc-1)
		case wake <- struct{}{}:
			<-woken
		}
		p.set(acc, 0)
	}
	require.NoError(t, <-done)
}

// scopedProxy is a settleProxy that can keep a settle to the connections it
// began with (coreAgent.InFlightScoper): its scope holds what scopePending
// says, whatever the proxy's whole count (settledProxy.pending) is.
// stuck are the connections its scope's PendingProgress names, each with its
// progress, until GiveUp gives up on them; behind is what its Behind says,
// until charged. With ownCuts each scope holds what scopePending said when
// it was cut (cuts, in order), not what it says later.
type scopedProxy struct {
	*settleProxy
	scopePending atomic.Int32
	fmu          sync.Mutex
	scopes       int
	stuck        map[uint64]uint64
	gaveUp       []uint64
	behind       []string
	charged      int
	ownCuts      bool
	cuts         []*atomic.Int32
}

func (s *scopedProxy) InFlightScope() coreAgent.InFlightScope {
	s.fmu.Lock()
	defer s.fmu.Unlock()
	s.scopes++
	n := &s.scopePending
	if s.ownCuts {
		n = new(atomic.Int32)
		n.Store(s.scopePending.Load())
		s.cuts = append(s.cuts, n)
	}
	return proxyScope{s: s, n: n}
}

// cut is the i-th scope's pending work (ownCuts).
func (s *scopedProxy) cut(i int) *atomic.Int32 {
	s.fmu.Lock()
	defer s.fmu.Unlock()
	return s.cuts[i]
}

type proxyScope struct {
	s *scopedProxy
	n *atomic.Int32
}

func (f proxyScope) State(bool) (int, []string) {
	n := int(f.n.Load())
	if n > 0 {
		blockedHere()
	}
	return n, nil
}

func (f proxyScope) PendingProgress(bool) map[uint64]uint64 {
	f.s.fmu.Lock()
	defer f.s.fmu.Unlock()
	out := map[uint64]uint64{}
	for id, n := range f.s.stuck {
		out[id] = n
	}
	return out
}

func (f proxyScope) GiveUp(ids []uint64, _ bool) []string {
	f.s.fmu.Lock()
	defer f.s.fmu.Unlock()
	var out []string
	for _, id := range ids {
		if _, ok := f.s.stuck[id]; ok {
			f.s.gaveUp = append(f.s.gaveUp, id)
			delete(f.s.stuck, id)
			f.n.Add(-1)
			out = append(out, fmt.Sprintf("connection %d held 1 request(s) its parser had read and not answered", id))
		}
	}
	return out
}

func (f proxyScope) Behind() ([]string, func()) {
	f.s.fmu.Lock()
	defer f.s.fmu.Unlock()
	conns := append([]string(nil), f.s.behind...)
	return conns, func() {
		f.s.fmu.Lock()
		f.s.charged++
		f.s.behind = nil
		f.s.fmu.Unlock()
	}
}

// Once the counts are settled, the settle waits for the connections it began
// with only (InFlightScope): one the application keeps busy after (a timer's
// calls) never holds it, and one of its own still holds it.
func TestTheSettleWaitsOnlyForTheConnectionsItBeganWith(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	_ = manualWake(t)
	a, h, p := settleAgent()
	sp := &scopedProxy{settleProxy: p}
	a.Proxy = sp
	h.established = 1
	sp.scopePending.Store(1)
	p.set(1, 2)
	done := make(chan error, 1)
	go func() {
		_, err := a.settle(context.Background(), true)
		done <- err
	}()
	p.set(1, 2)
	select {
	case err := <-done:
		t.Fatalf("settled while one of its own connections held work: %v", err)
	default:
	}
	sp.scopePending.Store(0)
	p.set(1, 1)
	require.NoError(t, <-done)
}

// With the proven switch off a settle (a read's, for an agent that settles
// its reads) waits for every connection the proxy keeps, as it always did:
// it never keeps to a scope.
func TestWithTheSwitchOffASettleWaitsForEveryConnection(t *testing.T) {
	t.Setenv(provenswitch.Env, "")
	_ = manualWake(t)
	a, h, p := settleAgent()
	sp := &scopedProxy{settleProxy: p}
	a.Proxy = sp
	h.established = 1
	p.set(1, 1)
	done := make(chan error, 1)
	go func() {
		_, err := a.settle(context.Background(), true)
		done <- err
	}()
	p.set(1, 1)
	select {
	case err := <-done:
		t.Fatalf("settled while a connection the proxy keeps held work: %v", err)
	default:
	}
	p.set(1, 0)
	require.NoError(t, <-done)
	sp.fmu.Lock()
	defer sp.fmu.Unlock()
	require.Zero(t, sp.scopes, "with the switch off a settle keeps to no scope")
}

// A test's end whose watchdog finds one of its connections stuck gives up on
// it: the test carries the loss, its reason naming the connection and what
// it held, and the next test's begin does not wait for it again.
func TestAnEndThatGivesUpOnAConnectionCarriesItAndTheNextBeginGoesOn(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	w := stallWake(t)
	a, h, p := settleAgent()
	sp := &scopedProxy{settleProxy: p, stuck: map[uint64]uint64{7: 1}}
	sp.scopePending.Store(1)
	a.Proxy = sp
	h.established = 1
	p.set(1, 0)
	wakes, err := w.run(func() error { return a.settledSince(context.Background(), true, "TestEnd") }, nil)
	require.ErrorIs(t, err, errNoProgress)
	require.GreaterOrEqual(t, wakes, settleStallLimit, "its watchdog gave up")
	require.Contains(t, err.Error(), "connection 7 held 1 request(s) its parser had read and not answered", "the test's reason names the connection and what it held")
	sp.fmu.Lock()
	require.Equal(t, []uint64{7}, sp.gaveUp, "the watchdog gave up on the connection it found stuck")
	sp.fmu.Unlock()
	// The next test's begin: the connection given up on holds nothing.
	wakes, err = w.run(func() error { _, err := a.settleBegin(context.Background(), true); return err }, nil)
	require.NoError(t, err)
	require.Zero(t, wakes, "a begin with nothing to wait for needs no wake-up")
}

// A test's end that gives up on a stuck connection says so at WARN in the
// agent's own log, naming the test, the connection and what it held: its
// caller tells only the test's failing, and a run's kept agent log is where
// the lines of the calls on that connection are.
func TestAnEndThatGivesUpSaysWhichConnectionsAtWarn(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	w := stallWake(t)
	a, h, p := settleAgent()
	core, logs := observer.New(zap.WarnLevel)
	a.logger = zap.New(core)
	sp := &scopedProxy{settleProxy: p, stuck: map[uint64]uint64{7: 1}}
	sp.scopePending.Store(1)
	a.Proxy = sp
	h.established = 1
	p.set(1, 0)
	_, err := w.run(func() error { return a.settledSince(context.Background(), true, "TestEnd") }, nil)
	require.ErrorIs(t, err, errNoProgress)
	warns := logs.FilterMessageSnippet("settling the test's calls at its end").All()
	require.Len(t, warns, 1, "the give-up is said once at WARN: %v", logs.All())
	require.Equal(t, "TestEnd", warns[0].ContextMap()["test"])
	require.Equal(t, []interface{}{"connection 7 held 1 request(s) its parser had read and not answered"}, warns[0].ContextMap()["connections"], "it names the connection and what it held")
}

// missesProxy is scopedProxy with a read of the misses that finds none.
type missesProxy struct{ *scopedProxy }

func (missesProxy) GetMockErrors(context.Context) ([]models.UnmatchedCall, error) {
	return nil, nil
}

// A read of the misses (GetMockErrors: keploy test's after a test case, the
// run's last) settles the application's calls as a test's end does. One
// whose watchdog gives up on a stuck connection cannot vouch for every miss,
// and says at WARN which connections it gave up on and what each held: its
// caller reports the misses unknown and points to the agent's log for why.
func TestAMissesReadThatGivesUpSaysWhichConnectionsAtWarn(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	w := stallWake(t)
	a, h, p := settleAgent()
	core, logs := observer.New(zap.WarnLevel)
	a.logger = zap.New(core)
	a.config = config.New()
	a.config.Agent.Mode = models.MODE_TEST
	sp := &scopedProxy{settleProxy: p, stuck: map[uint64]uint64{7: 1}}
	sp.scopePending.Store(1)
	a.Proxy = missesProxy{sp}
	h.established = 1
	p.set(1, 0)
	wakes, err := w.run(func() error { _, err := a.GetMockErrors(context.Background()); return err }, nil)
	require.ErrorIs(t, err, models.ErrUnmatchedCallsUnread, "a read that gave up cannot vouch for every miss")
	require.GreaterOrEqual(t, wakes, settleStallLimit, "its watchdog gave up")
	warns := logs.FilterMessageSnippet("before reading the misses").All()
	require.Len(t, warns, 1, "the give-up is said once at WARN: %v", logs.All())
	require.Equal(t, []interface{}{"connection 7 held 1 request(s) its parser had read and not answered"}, warns[0].ContextMap()["connections"], "it names the connection and what it held")
}

// Calls the application queued behind work a settle gave up on cannot be
// told answered: a begin does not wait for them and leaves them to the next
// end, which is not verified with them and charges them, once.
func TestCallsQueuedBehindLostWorkAreTheNextEndsLoss(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	_ = manualWake(t)
	a, h, p := settleAgent()
	sp := &scopedProxy{settleProxy: p, behind: []string{"connection 7 received 20 byte(s) after keploy gave up on it"}}
	a.Proxy = sp
	h.established = 1
	p.set(1, 0)
	_, err := a.settleBegin(context.Background(), true)
	require.NoError(t, err, "a begin goes on")
	sp.fmu.Lock()
	require.Zero(t, sp.charged, "a begin charges nobody")
	sp.fmu.Unlock()
	err = a.settledSince(context.Background(), true, "TestNext")
	require.Error(t, err)
	require.True(t, strings.Contains(err.Error(), "connection 7 received 20 byte(s)"), "%v", err)
	sp.fmu.Lock()
	require.Equal(t, 1, sp.charged, "the end charges them")
	sp.fmu.Unlock()
	require.NoError(t, a.settledSince(context.Background(), true, "TestAfter"), "charged once")
}

// An end whose settle found calls queued behind lost work (behindLost) is
// not verified, but it read the listener's drops when it settled, and that
// read closes the test's drops' window: the next window opens at it, so
// abandoned connects (the listener's count alone) after it charge nothing.
func TestAnEndWithCallsBehindLostWorkStillClosesItsDropsWindow(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	_ = manualWake(t)
	a, h, p := settleAgent()
	sp := &scopedProxy{settleProxy: p, behind: []string{"connection 7 received 20 byte(s) after keploy gave up on it"}}
	a.Proxy = sp
	h.established = 1
	p.set(1, 0)
	p.smu.Lock()
	p.drops = 5 // abandoned connects
	p.smu.Unlock()
	d, err := a.settleBegin(context.Background(), true)
	require.NoError(t, err)
	a.windowDrops(d, true, true)
	err = a.settledSince(context.Background(), true, "TestNext")
	require.Error(t, err, "the end is not verified")
	a.provenMu.Lock()
	got := a.proven.drops
	a.provenMu.Unlock()
	require.Equal(t, models.ListenDrops{Listener: 5, NetKnown: true}, got, "the end's read closes the window")
	n, _, _ := a.windowDrops(models.ListenDrops{Listener: 8, NetKnown: true}, true, true)
	require.Zero(t, n, "abandoned connects after it charge nothing")
}

// settleBlocked, when set, is told by the doubles that a settle is blocked
// at a step only an event or its watchdog can end (blockedHere).
var settleBlocked func()

// blockedHere is a double's wait saying, as it begins to block, that the
// settle cannot return until something moves: an application connection
// not yet paired, an accept not yet made, or work still held.
func blockedHere() {
	if settleBlocked != nil {
		settleBlocked()
	}
}

// settleWaker drives the settle watchdog by hand (stallWake).
type settleWaker struct {
	t                    *testing.T
	wake, woken, blocked chan struct{}
}

// stallWake has the settle watchdog wake only when a run says
// (settleWaker.run), with settleStallLimit 3.
func stallWake(t *testing.T) *settleWaker {
	t.Helper()
	w := &settleWaker{t: t, wake: make(chan struct{}), woken: make(chan struct{}), blocked: make(chan struct{}, 1)}
	prevWake, prevWoken, prevLimit, prevBlocked := settleWake, settleWoken, settleStallLimit, settleBlocked
	settleWake = func(context.Context) (<-chan struct{}, func()) { return w.wake, func() {} }
	settleWoken = func() { w.woken <- struct{}{} }
	settleStallLimit = 3
	settleBlocked = func() {
		select {
		case w.blocked <- struct{}{}:
		default:
		}
	}
	t.Cleanup(func() {
		settleWake, settleWoken, settleStallLimit, settleBlocked = prevWake, prevWoken, prevLimit, prevBlocked
	})
	return w
}

// run runs fn's settle and returns its error and the wake-ups it sent. It
// sends none until the settle is blocked (blockedHere), so a settle that
// needs none gets none; from then on, until the settle returns, it runs
// moved, when set (what moves between wake-ups), and wakes the watchdog,
// failing the test if the settle outlives the watchdog's limit.
func (w *settleWaker) run(fn func() error, moved func()) (wakes int, err error) {
	w.t.Helper()
	select {
	case <-w.blocked: // the last run's
	default:
	}
	done := make(chan error, 1)
	go func() { done <- fn() }()
	select {
	case err := <-done:
		return 0, err
	case <-w.blocked:
	}
	for {
		if moved != nil {
			moved()
		}
		select {
		case err := <-done:
			return wakes, err
		case w.wake <- struct{}{}:
			<-w.woken
			wakes++
			require.LessOrEqual(w.t, wakes, settleStallLimit+1, "the settle outlived its watchdog")
		}
	}
}

// cancelledEnd runs a test's end until it blocks (blockedHere) and then
// cancels its request, as a test process that gives up on it does; it
// returns the end's error.
func cancelledEnd(w *settleWaker, a *Agent) error {
	select {
	case <-w.blocked: // an earlier settle's
	default:
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- a.settledSince(ctx, true, "TestEnd") }()
	<-w.blocked
	cancel()
	err := <-done
	select {
	case <-w.blocked: // the end's own, before it saw the cancel
	default:
	}
	return err
}

// A test's end that returned before its connections settled (its request
// was cancelled) leaves them to the next begin, which waits for them: they
// are that test's. One the begin's watchdog finds stuck is given up on, and
// the begin goes on (the test whose end failed carried it); the end after
// does not wait for it again.
func TestABeginThatGivesUpOnThePreviousTestsWorkGoesOn(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	w := stallWake(t)
	a, h, p := settleAgent()
	sp := &scopedProxy{settleProxy: p, stuck: map[uint64]uint64{7: 1}, ownCuts: true}
	sp.scopePending.Store(1)
	a.Proxy = sp
	h.established = 1
	p.set(1, 0)
	require.ErrorIs(t, cancelledEnd(w, a), context.Canceled)
	sp.scopePending.Store(0)
	wakes, err := w.run(func() error { _, err := a.settleBegin(context.Background(), true); return err }, nil)
	require.NoError(t, err, "the begin goes on past the previous test's stuck work")
	require.GreaterOrEqual(t, wakes, settleStallLimit, "its watchdog gave up")
	sp.fmu.Lock()
	require.Equal(t, []uint64{7}, sp.gaveUp, "the begin's watchdog gave up on the connection it found stuck")
	require.Equal(t, 1, sp.scopes, "the begin cut no scope of its own")
	sp.fmu.Unlock()
	wakes, err = w.run(func() error { return a.settledSince(context.Background(), true, "TestNext") }, nil)
	require.NoError(t, err, "the end does not wait for what the begin gave up on")
	require.Zero(t, wakes, "an end with nothing to wait for needs no wake-up")
}

// A wait still in its first part (an application connection the kernel
// counted that the proxy never paired) has no connection to give up on: its
// watchdog fails the settle as a whole, and the end cuts as it fails (here
// nothing is held). The next begin waits for that cut only, and the next
// end reads fresh counts, so it waits for that connection again (a stated
// limit) until it is paired.
func TestAGiveUpBeforeTheCountsSettleIsNotPerConnection(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	w := stallWake(t)
	a, h, p := settleAgent()
	sp := &scopedProxy{settleProxy: p}
	a.Proxy = sp
	h.established = 1
	h.setApp(2, 1)
	p.set(1, 0)
	end := func() error { return a.settledSince(context.Background(), true, "TestEnd") }
	wakes, err := w.run(end, nil)
	require.ErrorIs(t, err, errNoProgress)
	require.GreaterOrEqual(t, wakes, settleStallLimit, "its watchdog gave up")
	require.NotContains(t, err.Error(), "gave up on", "no connection to give up on before the counts settle")
	wakes, err = w.run(func() error { _, err := a.settleBegin(context.Background(), true); return err }, nil)
	require.NoError(t, err, "the next begin waits for nothing")
	require.Zero(t, wakes, "a begin with nothing to wait for needs no wake-up")
	wakes, err = w.run(end, nil)
	require.ErrorIs(t, err, errNoProgress, "the next end waits for the unpaired connection again")
	require.GreaterOrEqual(t, wakes, settleStallLimit, "its watchdog gave up")
	sp.fmu.Lock()
	require.Equal(t, 2, sp.scopes, "each end cut as it failed, and the begin cut none")
	sp.fmu.Unlock()
	h.setApp(2, 2)
	wakes, err = w.run(end, nil)
	require.NoError(t, err, "once paired, the next settle goes on")
	require.Zero(t, wakes, "a settle with nothing to wait for needs no wake-up")
}

// A settle's watchdog gives up only on the connections that did not move:
// one that moved at every wake-up is kept (the end's reason does not name
// it, and later waits still wait for it).
func TestAGiveUpKeepsTheConnectionsThatMoved(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	w := stallWake(t)
	a, h, p := settleAgent()
	sp := &scopedProxy{settleProxy: p, stuck: map[uint64]uint64{7: 1, 8: 1}}
	sp.scopePending.Store(2)
	a.Proxy = sp
	h.established = 1
	p.set(1, 0)
	wakes, err := w.run(func() error { return a.settledSince(context.Background(), true, "TestEnd") }, func() {
		sp.fmu.Lock()
		sp.stuck[8]++
		sp.fmu.Unlock()
	})
	require.ErrorIs(t, err, errNoProgress)
	require.GreaterOrEqual(t, wakes, settleStallLimit, "its watchdog gave up")
	require.Contains(t, err.Error(), "connection 7 held")
	require.NotContains(t, err.Error(), "connection 8", "the connection that moved is not given up on")
	sp.fmu.Lock()
	defer sp.fmu.Unlock()
	require.Equal(t, []uint64{7}, sp.gaveUp, "the watchdog gave up on the connection that did not move only")
	require.Contains(t, sp.stuck, uint64(8), "the connection that moved is kept")
}

// A test's begin waits only for what the previous test's end cut: an
// application that keeps calling after it (a timer whose calls are held,
// on new connections or on ones the end kept, one not yet paired) never
// holds a begin, nor does one before the first test; the ending test's own
// last call still holds its end.
func TestABeginDoesNotWaitForCallsMadeAfterThePreviousEndsCut(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	w := stallWake(t)
	a, h, p := settleAgent()
	sp := &scopedProxy{settleProxy: p, ownCuts: true}
	a.Proxy = sp
	background := func(n uint64) {
		h.mu.Lock()
		h.established = n + 1
		h.mu.Unlock()
		h.setApp(n+1, n)
		sp.scopePending.Store(1)
		p.set(n, 1)
	}
	// Before the first test the application's timer already calls.
	background(1)
	begin := func() error { _, err := a.settleBegin(context.Background(), true); return err }
	wakes, err := w.run(begin, nil)
	require.NoError(t, err)
	require.Zero(t, wakes, "the first begin waits for nothing")
	// The test's own last call is in flight as it ends: the end waits for
	// it (its timer's calls paired by then, so the cut holds it alone).
	h.mu.Lock()
	h.established = 2
	h.mu.Unlock()
	h.setApp(2, 2)
	p.set(2, 1)
	ended := make(chan error, 1)
	go func() { ended <- a.settledSince(context.Background(), true, "TestEnd") }()
	<-w.blocked
	select {
	case err := <-ended:
		t.Fatalf("the end went on while the test's own call was held: %v", err)
	default:
	}
	sp.cut(0).Store(0)
	p.set(2, 0)
	require.NoError(t, <-ended, "the end settles the test's own call")
	// The timer keeps calling after the end's cut.
	background(3)
	wakes, err = w.run(begin, nil)
	require.NoError(t, err)
	require.Zero(t, wakes, "the next begin does not wait for the timer's calls")
	sp.fmu.Lock()
	defer sp.fmu.Unlock()
	require.Equal(t, 1, sp.scopes, "a begin cuts no scope")
	require.Empty(t, sp.gaveUp, "nothing was given up on")
}

// A test's end that returned before its connections settled (its request
// was cancelled) leaves them to the next begin, which waits until they
// settle.
func TestABeginWaitsForWhatThePreviousEndLeftHeld(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	w := stallWake(t)
	a, h, p := settleAgent()
	sp := &scopedProxy{settleProxy: p, ownCuts: true}
	sp.scopePending.Store(1)
	a.Proxy = sp
	h.established = 1
	p.set(1, 1)
	require.ErrorIs(t, cancelledEnd(w, a), context.Canceled)
	begun := make(chan error, 1)
	go func() {
		_, err := a.settleBegin(context.Background(), true)
		begun <- err
	}()
	<-w.blocked
	select {
	case err := <-begun:
		t.Fatalf("the begin went on while the previous test's call was held: %v", err)
	default:
	}
	sp.cut(0).Store(0)
	p.set(1, 0)
	require.NoError(t, <-begun)
	sp.fmu.Lock()
	defer sp.fmu.Unlock()
	require.Equal(t, 1, sp.scopes, "the begin cut no scope of its own")
}

// A test's end that fails before its cut (its request cancelled while an
// application connection was still on its way to the proxy) cuts as it
// fails: the next begin waits for the work its connections then held.
func TestABeginWaitsForWhatAnEndThatFailedBeforeItsCutLeft(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	w := stallWake(t)
	a, h, p := settleAgent()
	sp := &scopedProxy{settleProxy: p, ownCuts: true}
	sp.scopePending.Store(1)
	a.Proxy = sp
	h.established = 1
	h.setApp(2, 1)
	p.set(1, 1)
	require.ErrorIs(t, cancelledEnd(w, a), context.Canceled)
	begun := make(chan error, 1)
	go func() {
		_, err := a.settleBegin(context.Background(), true)
		begun <- err
	}()
	<-w.blocked
	select {
	case err := <-begun:
		t.Fatalf("the begin went on while the previous test's call was held: %v", err)
	default:
	}
	sp.cut(0).Store(0)
	p.set(1, 0)
	require.NoError(t, <-begun)
	sp.fmu.Lock()
	defer sp.fmu.Unlock()
	require.Equal(t, 1, sp.scopes, "the end cut as it failed, and the begin cut none")
}

// With the proven switch off a begin settles as it always did: it waits for
// every connection the proxy keeps, and keeps to no scope.
func TestWithTheSwitchOffABeginWaitsForEveryConnection(t *testing.T) {
	t.Setenv(provenswitch.Env, "")
	w := stallWake(t)
	a, h, p := settleAgent()
	sp := &scopedProxy{settleProxy: p}
	a.Proxy = sp
	h.established = 1
	p.set(1, 0)
	require.NoError(t, a.settledSince(context.Background(), true, "TestEnd"))
	p.set(1, 1)
	begun := make(chan error, 1)
	go func() {
		_, err := a.settleBegin(context.Background(), true)
		begun <- err
	}()
	<-w.blocked
	select {
	case err := <-begun:
		t.Fatalf("the begin went on while a connection the proxy keeps held work: %v", err)
	default:
	}
	p.set(1, 0)
	require.NoError(t, <-begun)
	sp.fmu.Lock()
	defer sp.fmu.Unlock()
	require.Zero(t, sp.scopes, "with the switch off a settle keeps to no scope")
}

// fakeCaptureSettler is a CaptureSettler that counts its settles, returns
// err, and, while block is set, waits for it (or the settle's end); progress
// is what its watchdog reads.
type fakeCaptureSettler struct {
	settles  atomic.Int32
	err      error
	block    chan struct{}
	progress atomic.Uint64
	// entered is closed at the first settle, when set.
	entered     chan struct{}
	enteredOnce sync.Once
}

func (s *fakeCaptureSettler) SettleCapture(ctx context.Context) error {
	s.settles.Add(1)
	if s.entered != nil {
		s.enteredOnce.Do(func() { close(s.entered) })
	}
	if s.block != nil {
		select {
		case <-s.block:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return s.err
}

func (s *fakeCaptureSettler) SettleProgress() map[string]uint64 {
	return map[string]uint64{"capture": s.progress.Load()}
}

func withCaptureSettler(t *testing.T, s CaptureSettler) {
	t.Helper()
	prev := captureSettler
	RegisterCaptureSettler(s)
	t.Cleanup(func() { RegisterCaptureSettler(prev) })
}

// Under the proven switch a record whose capture settles itself (a
// low-latency capture: no proxy in the application's path) settles each
// test's end through it and never asks for the proxy's counts, which such an
// agent cannot give; its begin waits for nothing.
func TestARecordsEndSettlesThroughTheCaptureWhenOneIsRegistered(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	a, h, _ := settleAgent()
	h.err = models.ErrProxyConnsUnsupported
	cs := &fakeCaptureSettler{}
	withCaptureSettler(t, cs)
	_, err := a.settleBegin(context.Background(), false)
	require.NoError(t, err, "a begin asks the proxy for nothing")
	require.Zero(t, cs.settles.Load(), "a begin waits for nothing")
	require.NoError(t, a.settledSince(context.Background(), false, "TestEnd"))
	require.EqualValues(t, 1, cs.settles.Load(), "the end settled through the capture")
}

// What the capture cannot tell fails the end: the test is not judged.
func TestAnEndTheCaptureCannotSettleIsNotJudged(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	a, _, _ := settleAgent()
	lost := errors.New("the capture lost 3 events")
	withCaptureSettler(t, &fakeCaptureSettler{err: lost})
	err := a.settledSince(context.Background(), false, "TestEnd")
	require.ErrorIs(t, err, lost)
	require.ErrorContains(t, err, "cannot tell that the capture holds no call of the test")
}

// A replay serves its mocks through keploy's proxy, and settles there; with
// the switch off nothing settles through the capture either: both still need
// the proxy's counts.
func TestOnlyAProvenRecordSettlesThroughTheCapture(t *testing.T) {
	cs := &fakeCaptureSettler{}
	withCaptureSettler(t, cs)
	for _, c := range []struct {
		proven  string
		answers bool
	}{{"1", true}, {"", false}, {"", true}} {
		t.Setenv(provenswitch.Env, c.proven)
		a, h, _ := settleAgent()
		h.err = models.ErrProxyConnsUnsupported
		_, err := a.settleBegin(context.Background(), c.answers)
		require.ErrorIs(t, err, models.ErrProxyConnsUnsupported, "proven=%q answers=%v", c.proven, c.answers)
		err = a.settledSince(context.Background(), c.answers, "TestEnd")
		require.ErrorIs(t, err, models.ErrProxyConnsUnsupported, "proven=%q answers=%v", c.proven, c.answers)
	}
	require.Zero(t, cs.settles.Load())
}

// The capture's settle runs under the settle's watchdog: one whose counts
// stop moving fails at settleStallLimit wake-ups, and one whose counts move
// is never failed for it. It counts wake-ups, never time.
func TestTheCapturesSettleRunsUnderTheWatchdog(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	wake := manualWake(t)
	prev := settleStallLimit
	settleStallLimit = 3
	t.Cleanup(func() { settleStallLimit = prev })
	a, _, _ := settleAgent()
	cs := &fakeCaptureSettler{block: make(chan struct{}), entered: make(chan struct{})}
	withCaptureSettler(t, cs)
	done := make(chan error, 1)
	go func() { done <- a.settledSince(context.Background(), false, "TestEnd") }()
	select {
	case <-cs.entered:
	case err := <-done:
		t.Fatalf("the end did not settle through the capture: %v", err)
	}
	wake()
	wake()
	cs.progress.Add(1) // the capture got further
	wake()
	wake()
	wake()
	select {
	case err := <-done:
		t.Fatalf("the watchdog fired before its count: %v", err)
	default:
	}
	wake()
	err := <-done
	require.ErrorIs(t, err, errNoProgress)
}
