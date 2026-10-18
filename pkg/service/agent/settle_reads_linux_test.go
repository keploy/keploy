//go:build linux

package agent

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.keploy.io/server/v3/config"
	coreAgent "go.keploy.io/server/v3/pkg/agent"
	proxyPkg "go.keploy.io/server/v3/pkg/agent/proxy"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

// noDest names no connection's destination: the proxy's handler of a
// connection it accepts gives the connection up.
type noDest struct{}

func (noDest) Get(context.Context, uint16) (*coreAgent.NetworkAddress, error) {
	return nil, errors.New("none")
}
func (noDest) Delete(context.Context, uint16) error { return nil }

// numberingHooks are connHooks that number each connection the proxy accepts,
// as the kernel's count does: 1, 2, ...; and pair each after the first (the
// proxy's canary, which is no application's) with an application connection
// going to a closed port, so it is an application's call, never an unpaired
// connection the proxy refuses.
type numberingHooks struct {
	*connHooks
	seq atomic.Uint64
}

func (h *numberingHooks) ProxyConnPair(syscall.RawConn) (coreAgent.ProxyConnPairing, error) {
	n := h.seq.Add(1)
	if n == 1 {
		return coreAgent.ProxyConnPairing{Seq: n}, nil
	}
	return coreAgent.ProxyConnPairing{Seq: n, AppSeq: n - 1, Dst: &coreAgent.NetworkAddress{Version: 4, IPv4Addr: 0x7f000001, Port: 1}}, nil
}

// realSettlingAgent is an agent of the given settle-reads, its real proxy
// listening as StartProxy has it listen (markProxyListener first), with hooks
// that count the connections the kernel completed to the proxy's listener.
func realSettlingAgent(t *testing.T, settles bool) (*Agent, *proxyPkg.Proxy, *connHooks) {
	t.Helper()
	h := &numberingHooks{connHooks: &connHooks{}}
	a, p := realSettlingAgentWith(t, settles, h, zap.NewNop())
	return a, p, h.connHooks
}

// realSettlingAgentWith is realSettlingAgent with the given hooks and logger.
func realSettlingAgentWith(t *testing.T, settles bool, hooks coreAgent.Hooks, logger *zap.Logger) (*Agent, *proxyPkg.Proxy) {
	t.Helper()
	cfg := config.New()
	cfg.Agent.Mode = models.MODE_TEST
	cfg.Agent.SettleReads = settles
	cfg.ProxyPort = 0
	p := proxyPkg.New(zap.NewNop(), noDest{}, cfg)
	a := &Agent{logger: logger, Proxy: p, config: cfg, Hooks: hooks}
	a.markProxyListener()
	ctx, cancel := context.WithCancel(context.Background())
	ready := make(chan error, 1)
	done := make(chan error, 1)
	go func() { done <- p.Serve(ctx, ready) }()
	require.NoError(t, <-ready)
	t.Cleanup(func() { cancel(); <-done })
	a.latchCounting() // as Hook does once the proxy is up
	return a, p
}

// With the proven switch off, an agent started for a run that learns noise
// (--settle-reads) reads a test's misses only once every call the kernel
// completed to its real proxy is in the proxy's hands: here one not yet
// accepted. With the switch off no read waited for any call.
func TestAnAgentThatSettlesReadsWaitsForTheCallsNotYetAcceptedWithTheSwitchOff(t *testing.T) {
	t.Setenv(provenswitch.Env, "")
	a, p, h := realSettlingAgent(t, true)
	require.Equal(t, 1, h.marked, "the listener of an agent that settles reads is marked at its start")
	accepted, err := p.Accepted()
	require.NoError(t, err)
	h.count(accepted)
	_, err = a.GetMockErrors(context.Background())
	require.NoError(t, err, "nothing in flight: the read settles at once")

	// The kernel completed one more connection to the proxy, not yet accepted.
	h.count(accepted + 1)
	read := make(chan error, 1)
	go func() {
		_, err := a.GetMockErrors(context.Background())
		read <- err
	}()
	select {
	case err := <-read:
		t.Fatalf("the misses were read while a call completed to the proxy was not yet in its hands: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	c, err := net.Dial("tcp", p.Listener.Addr().String())
	require.NoError(t, err)
	t.Cleanup(func() { _ = c.Close() })
	require.NoError(t, <-read)
}

// With the switch off, an agent that does not settle reads its misses at
// once, as before: its listener is not marked, and it never asks the counts.
func TestAnAgentThatDoesNotSettleReadsAtOnceWithTheSwitchOff(t *testing.T) {
	t.Setenv(provenswitch.Env, "")
	a, p, h := realSettlingAgent(t, false)
	require.Zero(t, h.marked, "the listener of an agent that does not settle is not marked")
	_, err := p.Accepted()
	require.Error(t, err, "its listener is not marked for the kernel's count")
	h.count(100) // a count no accept reaches: a read that waited on it would never end
	_, err = a.GetMockErrors(context.Background())
	require.NoError(t, err)
}

// With the switch off, an agent started settling reads where the calls cannot
// be counted from its start reads at once, as before, and says so once: hooks
// that do not count them (another platform), a mark the kernel refused, or a
// canary the kernel did not count. Refusing every read there would end noise
// learning, not make it right.
func TestAnAgentThatCannotCountReadsAtOnceAsBeforeWithTheSwitchOff(t *testing.T) {
	t.Setenv(provenswitch.Env, "")
	for _, tc := range []struct {
		name  string
		hooks coreAgent.Hooks
		// notes: the agent's Info that its hooks do not count. Where they do
		// and the count failed, the proxy says why, with what to fix.
		notes int
	}{
		{"hooks that do not count", loadedHooks{}, 1},
		{"a mark the kernel refused", &numberingHooks{connHooks: &connHooks{markErr: errors.New("refused"), established: 100}}, 0},
		{"a canary the kernel did not count", &countLosingHooks{connHooks: &connHooks{established: 100}, from: 1}, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			core, logs := observer.New(zap.InfoLevel)
			a, p := realSettlingAgentWith(t, true, tc.hooks, zap.New(core))
			require.False(t, a.settlesReads())
			require.False(t, p.CountedFromStart())
			_, err := a.GetMockErrors(context.Background())
			require.NoError(t, err, "a read this agent can never settle is read as before")
			require.Equal(t, tc.notes, logs.FilterMessageSnippet("do not count the calls waiting for its proxy").Len())
			require.Zero(t, logs.FilterLevelExact(zap.WarnLevel).Len(), "the agent warns of nothing here")
		})
	}
}

// countLosingHooks are connHooks that number the connections the proxy accepts
// until the from-th, and cannot from then on: the kernel's count lost.
type countLosingHooks struct {
	*connHooks
	from uint64
	seq  atomic.Uint64
}

func (h *countLosingHooks) ProxyConnPair(syscall.RawConn) (coreAgent.ProxyConnPairing, error) {
	if n := h.seq.Add(1); n < h.from {
		return coreAgent.ProxyConnPairing{Seq: n}, nil
	}
	return coreAgent.ProxyConnPairing{}, errors.New("an accepted connection of the proxy's marked listener has no sequence number")
}

// An agent that counted the calls from its start and then loses the count
// (a connection's number the kernel cannot give) answers every later read of
// a test's misses unread: it can no longer vouch for every call, and reading
// as before would bring back the misattribution unannounced.
func TestAnAgentThatLosesItsCountReadsUnreadWithTheSwitchOff(t *testing.T) {
	t.Setenv(provenswitch.Env, "")
	h := &countLosingHooks{connHooks: &connHooks{}, from: 2}
	a, p := realSettlingAgentWith(t, true, h, zap.NewNop())
	n, err := p.Accepted()
	require.NoError(t, err, "the canary was counted")
	require.True(t, a.settlesReads())
	h.count(n)
	_, err = a.GetMockErrors(context.Background())
	require.NoError(t, err, "nothing in flight")

	c, err := net.Dial("tcp", p.Listener.Addr().String())
	require.NoError(t, err)
	t.Cleanup(func() { _ = c.Close() })
	require.Error(t, p.WaitAccepted(context.Background(), n+1), "the next connection's number is lost")
	h.count(n + 1)
	require.True(t, a.settlesReads(), "latched at the start")
	_, err = a.GetMockErrors(context.Background())
	require.ErrorIs(t, err, models.ErrUnmatchedCallsUnread)
}

// losingProxy is a real proxy whose capture losses the test sets.
type losingProxy struct {
	*proxyPkg.Proxy
	losses atomic.Uint64
}

func (p *losingProxy) CaptureLosses() models.CaptureLosses {
	return models.CaptureLosses{{Kind: models.CaptureLossParserPanic, Count: p.losses.Load()}}
}

// A settled read with the switch off cannot vouch for every miss when the
// proxy lost calls since the last read (calls it intercepted and could not
// record or answer): it is answered unread, once; the next read, with no new
// loss, settles as before.
func TestASettledReadAfterACaptureLossIsUnreadWithTheSwitchOff(t *testing.T) {
	t.Setenv(provenswitch.Env, "")
	a, p, h := realSettlingAgent(t, true)
	lp := &losingProxy{Proxy: p}
	a.Proxy = lp
	accepted, err := p.Accepted()
	require.NoError(t, err)
	h.count(accepted)
	_, err = a.GetMockErrors(context.Background())
	require.NoError(t, err)
	lp.losses.Add(2)
	_, err = a.GetMockErrors(context.Background())
	require.ErrorIs(t, err, models.ErrUnmatchedCallsUnread)
	_, err = a.GetMockErrors(context.Background())
	require.NoError(t, err, "no loss since the last read")
}

// hookLossHooks are numberingHooks that report capture losses of their own (the
// application's connections lost before the proxy got them), and a count of
// them that can no longer be read.
type hookLossHooks struct {
	*numberingHooks
	lost   atomic.Uint64
	broken atomic.Bool
}

func (h *hookLossHooks) CaptureLosses() models.CaptureLosses {
	return models.CaptureLosses{{Kind: models.CaptureLossAppConnLost, Count: h.lost.Load()}}
}

func (h *hookLossHooks) CaptureLossesErr() error {
	if h.broken.Load() {
		return errors.New("the hooks' loss counter stopped")
	}
	return nil
}

// The hooks' losses count as the proxy's do: an application connection lost
// before the proxy got it, since the last read, leaves a settled read unread
// with the switch off; and so does a count of losses that cannot be read.
func TestASettledReadAfterAHooksLossIsUnreadWithTheSwitchOff(t *testing.T) {
	t.Setenv(provenswitch.Env, "")
	h := &hookLossHooks{numberingHooks: &numberingHooks{connHooks: &connHooks{}}}
	a, p := realSettlingAgentWith(t, true, h, zap.NewNop())
	accepted, err := p.Accepted()
	require.NoError(t, err)
	h.count(accepted)
	_, err = a.GetMockErrors(context.Background())
	require.NoError(t, err)
	h.lost.Add(1)
	_, err = a.GetMockErrors(context.Background())
	require.ErrorIs(t, err, models.ErrUnmatchedCallsUnread, "an application connection lost since the last read")
	_, err = a.GetMockErrors(context.Background())
	require.NoError(t, err)
	h.broken.Store(true)
	_, err = a.GetMockErrors(context.Background())
	require.ErrorIs(t, err, models.ErrUnmatchedCallsUnread, "losses that cannot be read")
}

// natProxy is a real proxy whose dropped count the test sets: with the switch
// off a NAT'd connection drops a counted run to not counted (CountingLost).
type natProxy struct {
	*proxyPkg.Proxy
	ch   chan struct{}
	lost atomic.Pointer[error]
}

func (p *natProxy) CountingLost() (<-chan struct{}, error) {
	if e := p.lost.Load(); e != nil {
		return p.ch, *e
	}
	return p.ch, nil
}

func (p *natProxy) drop() {
	err := fmt.Errorf("%w: a NAT'd connection", models.ErrProxyConnsUnsupported)
	p.lost.Store(&err)
	close(p.ch)
}

// With the switch off, once a NAT'd connection has dropped the run to not
// counted, no read of its misses is complete: the settle in flight then ends
// unread (the call it waits for can never settle), and every later read is
// unread at once, never read as before.
func TestAReadAfterTheRunDroppedToNotCountedIsUnreadWithTheSwitchOff(t *testing.T) {
	t.Setenv(provenswitch.Env, "")
	a, p, h := realSettlingAgent(t, true)
	np := &natProxy{Proxy: p, ch: make(chan struct{})}
	a.Proxy = np
	accepted, err := p.Accepted()
	require.NoError(t, err)
	h.count(accepted + 1) // a call the kernel completed to the proxy, never accepted
	read := make(chan error, 1)
	go func() {
		_, err := a.GetMockErrors(context.Background())
		read <- err
	}()
	select {
	case err := <-read:
		t.Fatalf("the read did not wait for the call in flight: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	np.drop()
	select {
	case err := <-read:
		require.ErrorIs(t, err, models.ErrUnmatchedCallsUnread, "the settle in flight when the run dropped to not counted")
		require.ErrorContains(t, err, "NAT'd connection", "ended by the drop, not by the stall watchdog")
	case <-time.After(30 * time.Second): // only turns a hang into a failure
		t.Fatal("the settle in flight did not end when the run dropped to not counted")
	}
	// Nothing in flight now: a read that settled would come back complete.
	h.count(accepted)
	_, err = a.GetMockErrors(context.Background())
	require.ErrorIs(t, err, models.ErrUnmatchedCallsUnread, "a read after it")
}

// The capture-loss mark advances on every read, its settle failed or not: a
// loss is the read's it happened before, so the next read, with none new and
// settled, is complete.
func TestTheLossMarkAdvancesOnAReadWhoseSettleFailed(t *testing.T) {
	t.Setenv(provenswitch.Env, "")
	h := &hookLossHooks{numberingHooks: &numberingHooks{connHooks: &connHooks{}}}
	a, p := realSettlingAgentWith(t, true, h, zap.NewNop())
	accepted, err := p.Accepted()
	require.NoError(t, err)
	h.count(accepted)
	_, err = a.GetMockErrors(context.Background())
	require.NoError(t, err)
	h.lost.Add(1)
	h.connHooks.mu.Lock()
	h.connHooks.err = models.ErrProxyConnsUnsupported // this read's settle fails
	h.connHooks.mu.Unlock()
	_, err = a.GetMockErrors(context.Background())
	require.ErrorIs(t, err, models.ErrUnmatchedCallsUnread)
	h.connHooks.mu.Lock()
	h.connHooks.err = nil
	h.connHooks.mu.Unlock()
	_, err = a.GetMockErrors(context.Background())
	require.NoError(t, err, "the loss was the failed read's; none since")
}

// droppingHooks are numberingHooks that drop the run to not counted (its
// reason set, its signal not yet closed) as the settle reads the counts: the
// settle can then finish before its abort goroutine hears of the drop.
type droppingHooks struct {
	*numberingHooks
	np    *natProxy
	armed atomic.Bool
}

func (h *droppingHooks) ProxyConnsEstablished(ctx context.Context) (uint64, error) {
	if h.armed.CompareAndSwap(true, false) {
		err := fmt.Errorf("%w: a NAT'd connection", models.ErrProxyConnsUnsupported)
		h.np.lost.Store(&err)
	}
	return h.numberingHooks.ProxyConnsEstablished(ctx)
}

// A drop while the read settles leaves the read unread even when the settle
// finished first: a call accepted after the drop is not kept, so the settle
// could not have seen it.
func TestADropDuringTheSettleLeavesTheReadUnread(t *testing.T) {
	t.Setenv(provenswitch.Env, "")
	h := &droppingHooks{numberingHooks: &numberingHooks{connHooks: &connHooks{}}}
	a, p := realSettlingAgentWith(t, true, h, zap.NewNop())
	np := &natProxy{Proxy: p, ch: make(chan struct{})}
	h.np = np
	a.Proxy = np
	accepted, err := p.Accepted()
	require.NoError(t, err)
	h.count(accepted)
	h.armed.Store(true)
	_, err = a.GetMockErrors(context.Background())
	require.ErrorIs(t, err, models.ErrUnmatchedCallsUnread, "dropped while the read settled")
	require.ErrorContains(t, err, "NAT'd connection")
}
