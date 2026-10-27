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
	coreAgent "go.keploy.io/server/v3/pkg/agent"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
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
	// drops: the packets the kernel dropped at the listener.
	drops uint64
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

func (s *settledProxy) ListenDrops() (uint64, error) {
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
			t.Cleanup(cancel)
			done := make(chan error, 1)
			go func() {
				_, err := a.settle(ctx, true)
				done <- err
			}()
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
// than settleStallLimit wake-ups in all, is no stuck wait.
func TestTheSettleWatchdogWeighsTheAcceptsTheEndWaitsFor(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	wake, woken := make(chan struct{}), make(chan struct{})
	prevWake, prevWoken, prevLimit := settleWake, settleWoken, settleStallLimit
	settleWake = func(context.Context) (<-chan struct{}, func()) { return wake, func() {} }
	settleWoken = func() { woken <- struct{}{} }
	settleStallLimit = 3
	t.Cleanup(func() { settleWake, settleWoken, settleStallLimit = prevWake, prevWoken, prevLimit })
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
// until charged.
type scopedProxy struct {
	*settleProxy
	scopePending atomic.Int32
	fmu          sync.Mutex
	scopes       int
	stuck        map[uint64]uint64
	gaveUp       []uint64
	behind       []string
	charged      int
}

func (s *scopedProxy) InFlightScope() coreAgent.InFlightScope {
	s.fmu.Lock()
	s.scopes++
	s.fmu.Unlock()
	return proxyScope{s}
}

type proxyScope struct{ s *scopedProxy }

func (f proxyScope) State(bool) (int, []string) {
	n := int(f.s.scopePending.Load())
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
			f.s.scopePending.Add(-1)
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
	wakes, err = w.run(func() error { _, err := a.settle(context.Background(), true); return err }, nil)
	require.NoError(t, err)
	require.Zero(t, wakes, "a begin with nothing to wait for needs no wake-up")
}

// Calls the application queued behind work a settle gave up on cannot be
// told answered: a begin goes on and leaves them to the next end, which is
// not verified with them and charges them, once.
func TestCallsQueuedBehindLostWorkAreTheNextEndsLoss(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	_ = manualWake(t)
	a, h, p := settleAgent()
	sp := &scopedProxy{settleProxy: p, behind: []string{"connection 7 received 20 byte(s) after keploy gave up on it"}}
	a.Proxy = sp
	h.established = 1
	p.set(1, 0)
	_, err := a.settle(context.Background(), true)
	require.ErrorIs(t, err, errHoldsUnknown, "a begin goes on, as for a connection whose held requests cannot be told")
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

// A test's begin whose watchdog gives up on a connection it found stuck
// fails that begin (that test is not judged), naming the connection and
// what it held; the give-up is per connection, so the test's end does not
// wait for it again.
func TestABeginThatGivesUpOnAConnectionFailsItAndTheEndGoesOn(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	w := stallWake(t)
	a, h, p := settleAgent()
	sp := &scopedProxy{settleProxy: p, stuck: map[uint64]uint64{7: 1}}
	sp.scopePending.Store(1)
	a.Proxy = sp
	h.established = 1
	p.set(1, 0)
	wakes, err := w.run(func() error { _, err := a.settle(context.Background(), true); return err }, nil)
	require.ErrorIs(t, err, errNoProgress)
	require.GreaterOrEqual(t, wakes, settleStallLimit, "its watchdog gave up")
	require.False(t, errors.Is(err, errHoldsUnknown), "a begin goes on only past connections whose requests cannot be told: %v", err)
	require.Contains(t, err.Error(), "connection 7 held 1 request(s) its parser had read and not answered")
	sp.fmu.Lock()
	require.Equal(t, []uint64{7}, sp.gaveUp, "the begin's watchdog gave up on the connection it found stuck")
	sp.fmu.Unlock()
	wakes, err = w.run(func() error { return a.settledSince(context.Background(), true, "TestEnd") }, nil)
	require.NoError(t, err, "the end does not wait for what the begin gave up on")
	require.Zero(t, wakes, "an end with nothing to wait for needs no wake-up")
}

// A wait still in its first part (an application connection the kernel
// counted that the proxy never paired) has no connection to give up on: its
// watchdog fails the settle as a whole, takes no scope, and the next settle
// reads fresh counts, so it waits for that connection again (a stated
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
	wakes, err = w.run(func() error { _, err := a.settle(context.Background(), true); return err }, nil)
	require.ErrorIs(t, err, errNoProgress, "the next begin waits for the unpaired connection again")
	require.GreaterOrEqual(t, wakes, settleStallLimit, "its watchdog gave up")
	sp.fmu.Lock()
	require.Zero(t, sp.scopes, "neither wait got to the connections' work")
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
