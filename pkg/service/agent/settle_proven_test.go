package agent

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	coreAgent "go.keploy.io/server/v3/pkg/agent"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
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
	_ coreAgent.ProxyConnsReporter  = &countHooks{}
	_ coreAgent.ProxyConnsReporter  = countingHooks{}
	_ coreAgent.ProxyConnsReporter  = uncountedHooks{}
	_ coreAgent.AcceptCounter       = &settledProxy{}
	_ coreAgent.InFlightReader      = &settledProxy{}
	_ coreAgent.PairedBytesWaiter   = &settledProxy{}
	_ coreAgent.InFlightStuckReader = &settledProxy{}
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
	// drops: the packets the kernel dropped at the listener.
	drops uint64
	// bytesWait, when set, is WaitPairedBytes; bytesProgress its progress.
	bytesWait     func(context.Context, uint64) error
	bytesProgress atomic.Uint64
	// inWork, when set, is closed at the settle's first look at the
	// connections' work: its wait for the counts (WaitProxySettled) is over.
	inWork   chan struct{}
	workOnce sync.Once
	// looks, when set, hears each look at the connections' work after the
	// first, without holding it up.
	looks chan struct{}
	first bool
	// stuck are the connections it holds that no reader will take, as the
	// proxy's InFlightStuck finds them: work until a look marks them, and
	// no record's work while the mark holds. A read of one ends its mark:
	// all of it (readStuck), and it is done; part (readStuckPartly), and it
	// is work again. stuckLooks counts the looks.
	stuck      []*fakeStuck
	stuckLooks atomic.Int32
	// looked, when set, hears each look.
	looked chan struct{}
	// onDrops, when set, runs once as the listener's drops are next read
	// (ProxyConns).
	onDrops func()
}

// fakeStuck is a connection settledProxy holds that no reader will take.
type fakeStuck struct {
	what string
	// marked: a look marked it; ended: a read ended the mark; done: it
	// holds no work. gen counts its marks.
	marked, ended, done bool
	gen                 int
}

// stuckHandle is a fakeStuck as InFlightStuck returns it, for its mark gen.
type stuckHandle struct {
	s   *settledProxy
	c   *fakeStuck
	gen int
}

func (h stuckHandle) Holds() bool {
	h.s.smu.Lock()
	defer h.s.smu.Unlock()
	return h.c.marked && !h.c.ended && h.c.gen == h.gen
}

func (h stuckHandle) String() string { return h.c.what }

// addStuck holds one more connection no reader will take.
func (s *settledProxy) addStuck(what string) *fakeStuck {
	c := &fakeStuck{what: what}
	s.smu.Lock()
	s.stuck = append(s.stuck, c)
	s.smu.Unlock()
	s.set(s.counts())
	return c
}

// readStuck is c's handler taking its bytes after all: its mark ends, and it
// holds no work.
func (s *settledProxy) readStuck(c *fakeStuck) {
	s.smu.Lock()
	c.ended, c.done = true, true
	s.smu.Unlock()
	s.set(s.counts())
}

// readStuckPartly is c's handler taking some of its bytes: its mark ends, and
// the rest are work again.
func (s *settledProxy) readStuckPartly(c *fakeStuck) {
	s.smu.Lock()
	c.ended = true
	s.smu.Unlock()
	s.set(s.counts())
}

func (s *settledProxy) counts() (uint64, int) {
	s.smu.Lock()
	defer s.smu.Unlock()
	return s.accepted, s.pending
}

// InFlightStuck marks each connection no reader will take that no look
// marked yet, and returns it, as the proxy's does.
func (s *settledProxy) InFlightStuck() []coreAgent.StuckConn {
	s.stuckLooks.Add(1)
	s.smu.Lock()
	var out []coreAgent.StuckConn
	for _, c := range s.stuck {
		if !c.done && (!c.marked || c.ended) {
			c.marked, c.ended = true, false
			c.gen++
			out = append(out, stuckHandle{s, c, c.gen})
		}
	}
	looked := s.looked
	s.smu.Unlock()
	if looked != nil {
		looked <- struct{}{}
	}
	return out
}

// stuckWork is the work its stuck connections hold: each not done, and, but
// in a replay's wait, whose mark does not hold.
func (s *settledProxy) stuckWork(answers bool) int {
	n := 0
	for _, c := range s.stuck {
		if !c.done && (answers || !c.marked || c.ended) {
			n++
		}
	}
	return n
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

func (s *settledProxy) ListenDrops() (uint64, error) {
	s.smu.Lock()
	hook := s.onDrops
	s.onDrops = nil
	s.smu.Unlock()
	if hook != nil {
		hook()
	}
	s.smu.Lock()
	defer s.smu.Unlock()
	return s.drops, nil
}

func (s *settledProxy) WaitAccepted(ctx context.Context, n uint64) error {
	for {
		changed := s.InFlightChanged()
		if acc, _ := s.Accepted(); acc >= n {
			return nil
		}
		select {
		case <-changed:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// WaitPairedBytes: it holds no paired connection with bytes still on their
// way, unless the test set bytesWait.
func (s *settledProxy) WaitPairedBytes(ctx context.Context, n uint64, _ models.Mode) error {
	if s.bytesWait != nil {
		return s.bytesWait(ctx, n)
	}
	return nil
}

// PairedBytesProgress is what the test set bytesProgress to.
func (s *settledProxy) PairedBytesProgress() uint64 { return s.bytesProgress.Load() }

func (s *settledProxy) InFlightPending(answers bool) int {
	s.smu.Lock()
	defer s.smu.Unlock()
	if s.inWork != nil {
		s.workOnce.Do(func() { close(s.inWork) })
	}
	if s.looks != nil && s.first {
		select {
		case s.looks <- struct{}{}:
		default:
		}
	}
	s.first = true
	return s.pending + s.stuckWork(answers)
}

func (s *settledProxy) InFlightChanged() <-chan struct{} {
	s.smu.Lock()
	defer s.smu.Unlock()
	if s.sig == nil {
		s.sig = make(chan struct{})
	}
	return s.sig
}

func (s *settledProxy) InFlightPendingProgress(answers bool) map[uint64]uint64 {
	s.smu.Lock()
	defer s.smu.Unlock()
	if s.pending+s.stuckWork(answers) == 0 {
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

// The watchdog runs what a wait looks at only when it wakes (woke): never at
// its start, which reads the counts it weighs from, and once at each
// wake-up it weighs and does not fire on.
func TestTheSettleWatchdogLooksOnlyWhenItWakes(t *testing.T) {
	wake := manualWake(t)
	a, _, _ := settleAgent()
	ctx, cancel := context.WithCancelCause(context.Background())
	t.Cleanup(func() { cancel(nil) })
	started := make(chan struct{})
	var once sync.Once
	var woke atomic.Int32
	stop := a.watchSettle(ctx, cancel, func() { woke.Add(1) }, func() map[string]uint64 {
		once.Do(func() { close(started) })
		return map[string]uint64{"w": uint64(woke.Load())}
	})
	defer stop()
	<-started
	require.Zero(t, woke.Load(), "the watchdog's start is no wake-up")
	wake()
	require.Equal(t, int32(1), woke.Load())
	wake()
	require.Equal(t, int32(2), woke.Load())
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
	stop := a.watchSettle(ctx, cancel, nil, func() map[string]uint64 {
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
