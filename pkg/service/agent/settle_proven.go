package agent

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	coreAgent "go.keploy.io/server/v3/pkg/agent"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.uber.org/zap"
)

// A test's end count (design v14 §5.2; release1-cut §5.1 item 5; verify-v14
// M1 and M2), under the proven switch; with it off, the same settle before a
// read of a test's misses, for an agent started settling reads whose calls
// are counted (Agent.settlesReads). Before a test is judged at its end, and,
// with the switch off, before one opens at its begin, every call the
// application made must be in the proxy's hands, and in a replay answered
// (under the switch a begin waits only for what the previous end cut:
// settleBegin):
//
//  1. every connection the application had made is in the proxy's hands:
//     each of its own connections numbered up to the AppConns read is paired
//     with the proxy's side or lost, and then every connection the kernel
//     completed to the proxy has been accepted (ProxyConns, then
//     WaitProxySettled on the counts it read);
//  2. no accepted connection holds bytes the proxy has not read;
//  3. in a replay, no accepted connection holds a request the proxy read and
//     has not answered: its handler is blocked reading it, or waits out an
//     answer the dependency held idle (integrations.MarkIdle: a long poll
//     that has caught up with the recording), its parser holds none it read
//     ahead (integrations.HoldRequest), and its handler is done with it if
//     it closed. A record skips it: a relayed request waits for its real
//     server, which the end never waits on.
//
// Each step waits on an event: an accept, a read beginning or returning, a
// request held or released, an idle wait marked or released, a connection
// closing; and, for a connection the proxy closed whose orphaned socket the
// application's FIN has not reached yet, which no event tells, the orphan's
// state, looked at again until it takes the FIN or is gone
// (proxy.WaitPairedBytes). No wall clock decides: only a hang budget measured
// in time the agent could run ends that look as unknown. A test
// whose calls keploy cannot count (models.ErrProxyConnsUnsupported, or hooks
// not loaded) is not settled: "unknown" is never "none in flight". Nor is one
// with a connection whose parser reads ahead and does not say what it holds,
// or says that for now it cannot tell (holdsUnknown): its end leaves it not
// verified. A test during which the kernel dropped, at the proxy's listener,
// what can lose a connection (chargeDrops) is charged that as a capture loss
// of its own (models.CaptureLossListenerDropped), so it alone is not judged,
// and the tests around it are; one dropped between two tests is the next
// test's, as a miss there is (markDrops); one before a test process's claim,
// or after the run's last test, is the run's, where every loss there is
// (closeDropsGapHeld, closeDropsGap). An agent that counts no
// capture losses leaves the end of the test charged not verified instead
// (dropped). The run's read of its misses
// settles the same way, so a call after the last test's end is read, never
// filed after it.
//
// The wait has a minimal watchdog (design v14 §8.4): it wakes once a second
// and counts, for each thing the wait waits on, the wake-ups in a row at which
// it did not move (the accept and application connection counts; then each
// connection that still holds work: its bytes, its held requests, its close).
// At settleStallLimit it fails the wait, and the test is not judged. It counts its own wake-ups, not seconds: a
// starved agent starves its watchdog equally, so load alone cannot trip it,
// and a deadlock can. Under the proven switch, a connection it gives up on
// is charged to the settle that gave up, named with what it held (gaveUp),
// and no later settle waits for it while its handler has not passed that
// (coreAgent.InFlightScope's GiveUp): the next test's begin goes on, and a
// later test's end is not verified only when the application queued calls
// behind the lost work in its window (behindLost).

// settleStallLimit is the watchdog's count of wake-ups in a row with no
// progress (design v14's lead decision: 600).
var settleStallLimit = 600

// settleWake is how the watchdog wakes; a test replaces it.
var settleWake = func(ctx context.Context) (<-chan struct{}, func()) {
	t := time.NewTicker(time.Second)
	ch := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(ch)
		for {
			select {
			case <-t.C:
				select {
				case ch <- struct{}{}:
				case <-ctx.Done():
					return
				case <-done:
					return
				}
			case <-ctx.Done():
				return
			case <-done:
				return
			}
		}
	}()
	return ch, func() { t.Stop(); close(done) }
}

// settleWoken, when set, runs after the watchdog has weighed a wake-up; a
// test waits on it.
var settleWoken func()

// errNoProgress is the watchdog's: the wait made no progress at all.
var errNoProgress = errors.New("keploy's agent made no progress settling the test's calls")

// noProgress is errNoProgress naming what the wait found stuck: the keys of
// the watchdog's progress that did not move.
type noProgress struct{ stalled []string }

func (n noProgress) Error() string { return errNoProgress.Error() }

func (n noProgress) Is(target error) bool { return target == errNoProgress }

// gaveUp is errNoProgress for a settle that gave up on the connections it
// found stuck (InFlightScope.GiveUp): held says what each held.
type gaveUp struct{ held []string }

func (g gaveUp) Error() string {
	return errNoProgress.Error() + "; it gave up on " + strings.Join(g.held, "; ") + " (no later wait waits for that work)"
}

func (g gaveUp) Is(target error) bool { return target == errNoProgress }

// behindLost: connections a settle left out, as an earlier one gave up on
// them, received bytes since they were last charged, queued behind the lost
// work (InFlightScope.Behind). Like a connection whose held requests cannot
// be told (it Is errHoldsUnknown), it does not stop a begin; a test's end
// that finds it is not verified, and charges them (settledSince).
type behindLost struct {
	conns  []string
	charge func()
}

func (b behindLost) Error() string { return strings.Join(b.conns, "; ") }

func (b behindLost) Is(target error) bool { return target == errHoldsUnknown }

// errHoldsUnknown: a connection is served by a parser that reads ahead of
// its answers and does not say which requests it holds
// (integrations.RequestHolding), or that says for now it cannot tell
// (integrations.MarkUnknown), so whether it still holds one of the test's
// calls cannot be told. A test's end that finds one is not verified; a begin
// goes on (the test's end, or the run's last read, says it).
var errHoldsUnknown = errors.New("a connection's held requests are unknown")

// holdsUnknown is errHoldsUnknown for the protocols it names.
type holdsUnknown []string

func (h holdsUnknown) Error() string {
	return "keploy cannot tell whether " + strings.Join(h, ", ") + " on a connection still holds a request: its parser reads ahead of its answers and does not say which requests it holds, or says that for now it cannot tell"
}

func (h holdsUnknown) Is(target error) bool { return target == errHoldsUnknown }

// settle waits until the test's calls are all in the proxy's hands, and, in a
// replay (answers), answered. It returns the proxy listener's drops read once
// settled (models.ProxyConnCounts.ListenDrops), which a test's end compares
// with its begin's (windowDrops). An error says keploy cannot tell: the test
// is not judged. holdsUnknown comes with the drops.
func (a *Agent) settle(ctx context.Context, answers bool) (models.ListenDrops, error) {
	return a.settleKeeping(ctx, answers, nil)
}

// settleKeeping is settle; keep, when set, is given the scope the settle
// cuts (coreAgent.InFlightScope) as it cuts it: what a window's end owned
// (settledSince), which the next begin waits for (settleBegin). Under the
// proven switch a settle that fails before its cut cuts as it fails: the
// work its connections then held is still the window's, and a call of it
// answered later is never the next test's.
func (a *Agent) settleKeeping(ctx context.Context, answers bool, keep func(coreAgent.InFlightScope)) (models.ListenDrops, error) {
	none := models.ListenDrops{}
	// A capture beside the application's path has no proxy to count: it
	// settles itself, and cuts no scope (settlesByCapture).
	if cs, ok := settlesByCapture(answers); ok {
		return none, a.settleCapture(ctx, cs)
	}
	cut := false
	if keep != nil && provenswitch.On() {
		defer func() {
			if sr, ok := a.Proxy.(coreAgent.InFlightScoper); ok && !cut {
				keep(sr.InFlightScope())
			}
		}()
	}
	ir, ok := a.Proxy.(coreAgent.InFlightReader)
	if !ok || a.Proxy == nil {
		return none, errors.New("this keploy agent's proxy cannot tell which calls it still holds")
	}
	counts, err := a.ProxyConns(ctx)
	if err != nil {
		return none, fmt.Errorf("keploy cannot count the calls still waiting for its proxy: %w", err)
	}
	// The scope, the give-up and the watchdog's own counts are the proven
	// switch's: with it off, a settle (a read's, for an agent that settles
	// its reads) waits as it always did.
	proven := provenswitch.On()
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	// With the switch off a NAT'd connection that drops the run to not
	// counted ends a settle in flight: the call it waits for can never
	// settle, and the read it is for cannot vouch for every miss.
	if lost := a.countingLostSignal(); lost != nil {
		go func() {
			select {
			case <-lost:
				cancel(a.countingLost())
			case <-ctx.Done():
			}
		}()
	}
	// The watchdog reads the counts at every wake-up: that read is also what
	// ends an application connection's wait when the kernel dropped its
	// event (WaitAppConnsSettled). While the wait is for the counts, what it
	// waits for must move: the application's connections numbered up to the
	// count read (counts.AppConns), the accepts it then waits for (accepts),
	// and the bytes on the connections paired with those. A connection the
	// application opens after the wait began is numbered after them and
	// moves none of it: an application that keeps connecting (a timer's
	// call, its miss, a reset, a new connection) does not feed the watchdog
	// while one of the wait's own connections never reaches the proxy. Once
	// the wait is for the connections' work, each connection that still
	// holds some must move.
	var working atomic.Bool
	var accepts atomic.Uint64
	// view is what the wait reads once it is for the connections' work:
	// the connections kept when it got there, with the bytes each had
	// received (InFlightScope), set before working, else every connection
	// the proxy keeps.
	var view inFlightView = ir
	var scoped atomic.Pointer[coreAgent.InFlightScope]
	stop := a.watchSettle(ctx, cancel, func() map[string]uint64 {
		c, err := a.ProxyConns(ctx)
		if !working.Load() {
			if err != nil {
				return nil
			}
			v := c.Accepted + c.AppSettled
			if proven {
				v = min(c.AppSettled, counts.AppConns) + min(c.Accepted, accepts.Load())
			}
			if pb, ok := a.Proxy.(coreAgent.PairedBytesWaiter); ok {
				v += pb.PairedBytesProgress()
			}
			return map[string]uint64{"counts": v}
		}
		pp := ir.InFlightPendingProgress
		if sc := scoped.Load(); sc != nil {
			pp = (*sc).PendingProgress
		}
		out := map[string]uint64{}
		for id, n := range pp(answers) {
			out[strconv.FormatUint(id, 10)] = n
		}
		return out
	})
	defer stop()
	if err := a.waitProxySettled(ctx, counts, accepts.Store); err != nil {
		if cause := context.Cause(ctx); cause != nil {
			return none, cause
		}
		return none, err
	}
	// Every connection and byte the wait waits for is in the proxy's hands:
	// what can hold them is the bytes each connection kept now has
	// received, until its handler has taken them and, in a replay, answered
	// them. A connection accepted from here on, or bytes one receives from
	// here on, are not this wait's: an application that keeps calling (a
	// timer's call, its miss, a new connection, the next call on a pooled
	// one) cannot hold it.
	if sr, ok := a.Proxy.(coreAgent.InFlightScoper); ok && proven {
		sc := sr.InFlightScope()
		scoped.Store(&sc)
		view = scopeView{sc}
		if keep != nil {
			keep(sc)
		}
		cut = true
	}
	working.Store(true)
	for {
		changed := ir.InFlightChanged()
		// One view of the connections (InFlightState): read apart, a parser
		// that ended a state it could not tell between the two reads would be
		// seen neither busy nor unknown.
		pending, unknown := view.InFlightState(answers)
		if pending == 0 {
			c, err := a.ProxyConns(ctx)
			if err != nil {
				return none, fmt.Errorf("keploy cannot count the calls still waiting for its proxy: %w", err)
			}
			// Settled, but for a connection whose parser reads ahead and does
			// not say which requests it holds, or says that for now it cannot
			// tell: it may hold one.
			if len(unknown) > 0 {
				err = holdsUnknown(unknown)
			}
			// Calls the application queued behind work an earlier settle
			// gave up on: whether they were answered cannot be told.
			if sc := scoped.Load(); sc != nil {
				if conns, charge := (*sc).Behind(); len(conns) > 0 {
					bl := behindLost{conns: conns, charge: charge}
					if err == nil {
						return c.ListenDrops, bl
					}
					return c.ListenDrops, fmt.Errorf("%w; %w", err, bl)
				}
			}
			return c.ListenDrops, err
		}
		select {
		case <-changed:
		case <-ctx.Done():
			// The watchdog gave up on the connections it found stuck: their
			// work is this settle's loss, and no later settle waits for it.
			cause := context.Cause(ctx)
			var np noProgress
			if sc := scoped.Load(); sc != nil && errors.As(cause, &np) {
				ids := make([]uint64, 0, len(np.stalled))
				for _, k := range np.stalled {
					if id, err := strconv.ParseUint(k, 10, 64); err == nil {
						ids = append(ids, id)
					}
				}
				if held := (*sc).GiveUp(ids, answers); len(held) > 0 {
					return none, gaveUp{held: held}
				}
			}
			return none, cause
		}
	}
}

// inFlightView is what a settle reads of the connections' work.
type inFlightView interface {
	InFlightState(answers bool) (pending int, unknown []string)
}

// scopeView reads a settle's scope (coreAgent.InFlightScope).
type scopeView struct{ coreAgent.InFlightScope }

func (v scopeView) InFlightState(answers bool) (int, []string) { return v.State(answers) }

// dropCarry is what the windows before left unpaired of their two drop
// counts (chargeDrops).
type dropCarry struct{ listener, net uint64 }

// chargeDrops is how many drops that can lose a connection the kernel made
// at the proxy's listener in a window, from its counts at the start, was, to
// those at its end, now (models.ListenDrops), with carry what the windows
// before left unpaired, and what this one leaves unpaired for the next. gap
// says the window is no test's: the gap from a test's end, or the session's
// start, to the next begin, whose charge is counted as the gap closes
// (countDrops: the next test's, as a miss in the gap is; at a test
// process's claim or the run's last read, the run's).
//
//   - Such a drop raises both counts; one that loses nothing (a client's
//     reset of a connect it gave up, a retransmitted SYN) raises the
//     listener's alone, and a drop at another listener of the namespace
//     raises the namespace's alone. The window is charged the smaller rise:
//     never fewer than the drops that can lose a connection, though a drop
//     at another listener in the same window can make it more (the test is
//     then not verified, never wrongly verified).
//   - The two counts are read one after the other and the kernel raises
//     them one after the other, so one drop can land its two rises on either
//     side of a window's end, or, its second rise late, on either side of the
//     whole gap. What a window leaves unpaired is carried into the next one,
//     and pairs there: the drop is charged to the window in which both its
//     rises are seen. A test's window carries its own rises' unpaired part
//     one window, the gap after it; what it was carried and could not pair
//     pairs with nothing later. The gap passes what it leaves unpaired on
//     whole, what it was carried included, so a drop whose rises land on
//     either side of the gap is charged to the next test's window, never to
//     none.
//   - With the namespace's count unread at either end, every rise of the
//     listener's is charged, as keploy did before it read both.
//   - A count that went back (another listener, or a count lost) charges at
//     least one: keploy cannot tell what it missed.
func chargeDrops(was, now models.ListenDrops, carry dropCarry, gap bool) (uint64, dropCarry) {
	own, ownBack := rise(was.Listener, now.Listener)
	if !was.NetKnown || !now.NetKnown {
		if ownBack {
			return max(own, 1), dropCarry{}
		}
		return own + carry.listener, dropCarry{}
	}
	net, netBack := rise(was.Net, now.Net)
	if ownBack || netBack {
		return max(own, net, 1), dropCarry{}
	}
	l, n := own+carry.listener, net+carry.net
	charged := min(l, n)
	if gap {
		return charged, dropCarry{listener: l - charged, net: n - charged}
	}
	return charged, dropCarry{listener: min(l-charged, own), net: min(n-charged, net)}
}

// rise is how much a count grew from was to now; back says it went back, and
// the rise is then now's count.
func rise(was, now uint64) (n uint64, back bool) {
	if now < was {
		return now, true
	}
	return now - was, false
}

// windowDrops ends the drops' window that the read now closes, and opens the
// next one at now: it returns what chargeDrops charges the window, and
// whether a window was open (its start read). The start is a test's begin,
// keploy test's open of a test case, the session's start, or the end of the
// window before; gap says the window closing is no test's (a begin closes
// it, or the run's last read), test that the one it opens is a test's.
func (a *Agent) windowDrops(now models.ListenDrops, gap, test bool) (charged uint64, why string, open bool) {
	a.provenMu.Lock()
	defer a.provenMu.Unlock()
	was, open := a.proven.drops, a.proven.dropsKnown
	carry := dropCarry{}
	if open {
		charged, carry = chargeDrops(was, now, a.proven.dropCarry, gap)
	}
	a.proven.drops, a.proven.dropsKnown, a.proven.dropCarry = now, true, carry
	a.proven.dropsInTest = test
	switch {
	case !was.NetKnown && open:
		why = was.NetUnread
	case !now.NetKnown:
		why = now.NetUnread
	}
	return charged, why, open
}

// errDropped: the kernel dropped, at the proxy's listener, what can lose a
// connection while a window was open (chargeDrops).
var errDropped = errors.New("connection attempts were dropped at keploy's proxy listener")

// dropped is errDropped for one window: n drops while what ran. A SYN or
// final ACK the kernel dropped there is a connection in neither of the
// proxy's counts until the client retransmits it, so a call of the window
// may not have reached keploy. netUnread, when set, is why keploy could not
// read its network namespace's count, so that every drop at the listener
// counted.
type dropped struct {
	what      string
	n         uint64
	netUnread string
	// gap says n includes the gap's before the test (countDrops).
	gap bool
}

func (d dropped) Error() string {
	when := "while " + d.what + " ran"
	if d.gap {
		when += ", or before it while no test was open"
	}
	if d.netUnread != "" {
		return fmt.Sprintf("the kernel dropped %d packet(s) at keploy's proxy listener %s (a full SYN or accept queue, or a client that gave up its connect: keploy could not read its network namespace's ListenDrops, which tell those apart, so every drop counts: %s), so a call may not have reached keploy", d.n, when, d.netUnread)
	}
	return fmt.Sprintf("the kernel dropped %d connection attempt(s) at keploy's proxy listener %s (its SYN or accept queue full: the listener's drops and its network namespace's ListenDrops both rose), so a call may not have reached keploy", d.n, when)
}

func (d dropped) Is(target error) bool { return target == errDropped }

// why is what the drops' capture loss says of them
// (models.CaptureLoss.What): that every drop at the listener counted, and
// why, when keploy could not read its network namespace's count; "" when it
// could.
func (d dropped) why() string {
	if d.netUnread == "" {
		return ""
	}
	return "every packet the listener dropped counted: keploy could not read its network namespace's ListenDrops, which tell a drop that can lose a connection from a client that gave up its connect: " + d.netUnread
}

// watchSettle is the settle wait's watchdog. progress gives, at each
// wake-up, a count for each thing the wait is waiting on (the counts of the
// accepts and application connections while it waits for those; then each
// connection that still holds work). It cancels ctx with errNoProgress once
// one of them has not moved for settleStallLimit wake-ups in a row: a wait is
// stuck when what it waits on does not move, whatever moves elsewhere. One
// that stops being waited on drops out; a new one starts its count.
func (a *Agent) watchSettle(ctx context.Context, cancel context.CancelCauseFunc, progress func() map[string]uint64) (stop func()) {
	wake, stopWake := settleWake(ctx)
	quit, done := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		last, stalls := progress(), map[string]int{}
		for {
			select {
			case <-quit:
				return
			case _, ok := <-wake:
				if !ok {
					return
				}
			}
			now := progress()
			var stalled []string
			for k, v := range now {
				if was, ok := last[k]; ok && was == v {
					stalls[k]++
				} else {
					stalls[k] = 0
				}
				if stalls[k] >= settleStallLimit {
					stalled = append(stalled, k)
				}
			}
			fire := len(stalled) > 0
			for k := range stalls {
				if _, ok := now[k]; !ok {
					delete(stalls, k)
				}
			}
			last = now
			if fire {
				sort.Strings(stalled)
				cancel(noProgress{stalled: stalled})
			}
			if settleWoken != nil {
				settleWoken()
			}
			if fire {
				return
			}
		}
	}()
	return func() {
		close(quit)
		stopWake()
		<-done
	}
}

// markDrops opens the drops' window at the proxy listener's drops read as a
// test, or keploy test's test case, begins, and closes the gap before it:
// what can lose a connection that the kernel dropped there is counted now
// (countDrops), so the test beginning is charged it at its end, as it is a
// miss of the gap. The gap starts at the test before it's end, its process's
// claim (which counts what came before it as the run's: closeDropsGapHeld),
// or the session's start (openDrops). What the gap left unpaired, what the
// test before it left included, is carried on whole (chargeDrops).
func (a *Agent) markDrops(ctx context.Context, drops models.ListenDrops) {
	n, why, _ := a.windowDrops(drops, true, true)
	a.countDrops(ctx, n, why)
}

// countDrops counts n drops that can lose a connection, the charge of a
// window closing now (chargeDrops), as the agent's capture loss
// (models.CaptureLossListenerDropped): it is then where every capture loss
// counted now is, charged by the next read of the losses that charges one
// (a test's end, keploy test's read of a test case's losses after its end:
// the window's own test, or, for the gap, the test that begins). why is
// windowDrops'. An agent whose capture losses cannot be read
// (countsCaptureLosses) has no such read: there a gap's count is kept for
// the next test's end, which fails with it (settledSince).
func (a *Agent) countDrops(ctx context.Context, n uint64, why string) {
	if n == 0 {
		return
	}
	d := dropped{n: n, netUnread: why}
	if a.countsCaptureLosses(ctx) {
		a.listenerDrops.AddN(models.CaptureLossListenerDropped, n, d.why())
		return
	}
	a.provenMu.Lock()
	defer a.provenMu.Unlock()
	a.proven.dropsUncounted += n
	if a.proven.dropsUncountedWhy == "" {
		a.proven.dropsUncountedWhy = why
	}
}

// closeDropsGap closes the drops' window no test holds -- from the last
// test's end, or the session's start -- at the run's last read
// (UnchargedCaptureLosses: a record's, and mock record's and mock test's
// read just before their read of the run's losses; GetMockErrors, keploy
// test's, which it makes after each test case and test set, after its settle
// and just before it reads the misses): what can lose a connection that the
// kernel dropped there is counted now (countDrops), as every capture loss
// after the last test is, so that read and the next take it where they take
// those. That is the one place the drops after a run's last test go: the
// losses charged to no test (UnchargedCaptureLosses) and the run's own count
// of its losses, read after it, which are what the run is judged by (a kind
// that leaves its tests not judged leaves the run not judged, as a TLS
// handshake abandoned there does); keploy test's next test set counts its
// losses from after them, and its next test case from them, as it does any
// loss. A test process's claim closes the gap too (closeDropsGapHeld): a
// drop before it is the run's. A test's window still open (a test that has
// not ended) is not closed: its end charges it. An agent that counts no
// capture losses keeps them for a next test's end, and with none they are
// counted by nothing, as every loss of such an agent is. The wire is taken,
// so a begin or an end does not read the drops between this read and its
// close.
func (a *Agent) closeDropsGap(ctx context.Context) {
	if !a.dropsGapOpen() {
		return
	}
	if err := a.wire.lock(ctx); err != nil {
		return
	}
	defer a.wire.unlock()
	a.closeDropsGapHeld(ctx)
}

// closeDropsGapHeld is closeDropsGap for a caller that holds the wire
// already: a test process's claim (ClaimInstance), which counts the drops
// before it as the run's, as it does a failed TLS handshake or a miss there,
// before it marks the losses its first test counts from.
func (a *Agent) closeDropsGapHeld(ctx context.Context) {
	if !a.dropsGapOpen() {
		return
	}
	_, p, err := a.proxyConnSides()
	if err != nil {
		return
	}
	now, err := p.ListenDrops()
	if err != nil {
		// Not read, the gap stays open: the next begin, or read, closes it.
		return
	}
	n, why, _ := a.windowDrops(now, true, false)
	a.countDrops(ctx, n, why)
}

// dropsGapOpen says the drops' window open is the gap no test holds.
func (a *Agent) dropsGapOpen() bool {
	a.provenMu.Lock()
	defer a.provenMu.Unlock()
	return a.proven.dropsKnown && !a.proven.dropsInTest
}

// openDrops opens the session's first drops' window, a gap, at its start.
// On the scope API a drop before a test process's claim is the run's: the
// claim counts it (closeDropsGapHeld). One after the claim and before its
// first begin is its first test's, as a miss there is; in keploy test, one
// before a set's first test case is that test case's, as a failed TLS
// handshake there is (a miss there is the set's). When the count cannot be
// read here, no window is open, so nothing counts a drop before the first
// begin, as before keploy kept this count: the first begin's read opens the
// first window.
func (a *Agent) openDrops() {
	_, p, err := a.proxyConnSides()
	if err != nil {
		return
	}
	now, err := p.ListenDrops()
	if err != nil {
		return
	}
	a.windowDrops(now, true, false)
}

// settledSince settles (settle) the window what ends, and charges it what
// the kernel dropped, at the proxy's listener, that can lose a connection
// while it was open (chargeDrops). The drops read now begin the next window.
//
// The drops are the window's own capture loss
// (models.CaptureLossListenerDropped), counted in the agent's capture losses
// before the end's read of them (chargeCaptureLosses; keploy test's read of a
// test case's losses after its end): that read charges them to this test
// alone, which is not judged, and leaves every other test judged. An agent
// whose capture losses cannot be read (countsCaptureLosses) would have no
// reader take them, so there the window fails instead, with errDropped: not
// verified, never charged to nothing.
func (a *Agent) settledSince(ctx context.Context, answers bool, what string) error {
	// What the window owned at its cut is what the next begin waits for
	// (settleBegin).
	var owned coreAgent.InFlightScope
	drops, err := a.settleKeeping(ctx, answers, func(sc coreAgent.InFlightScope) { owned = sc })
	a.provenMu.Lock()
	a.proven.owned = owned
	a.provenMu.Unlock()
	if err != nil && !errors.Is(err, errHoldsUnknown) {
		// Said in the agent's own log too, where the lines of the calls it
		// gave up on are: the caller tells only the test's failing.
		var g gaveUp
		if errors.As(err, &g) {
			a.logger.Warn("keploy's agent made no progress settling the test's calls at its end; it gave up on them, so the test is not judged", zap.String("test", what), zap.Strings("connections", g.held))
		}
		// Not read: the test's window goes on as the gap's, which the next
		// begin, or the run's last read, closes. The test is not judged.
		a.provenMu.Lock()
		a.proven.dropsInTest = false
		a.provenMu.Unlock()
		return err
	}
	// A window's end charges it the calls queued behind lost work.
	var bl behindLost
	if errors.As(err, &bl) {
		bl.charge()
	}
	n, why, open := a.windowDrops(drops, false, false)
	if !open {
		return err
	}
	if a.countsCaptureLosses(ctx) {
		a.countDrops(ctx, n, why)
		return err
	}
	// No read of the losses would take them: the window, and the gaps
	// before it (countDrops), fail.
	a.provenMu.Lock()
	gap := a.proven.dropsUncounted
	n += gap
	if why == "" {
		why = a.proven.dropsUncountedWhy
	}
	a.proven.dropsUncounted, a.proven.dropsUncountedWhy = 0, ""
	a.provenMu.Unlock()
	if n == 0 || err != nil {
		return err
	}
	return dropped{what: what, n: n, netUnread: why, gap: gap > 0}
}

// settleBegin settles, before a test's begin (or a test case's open) opens
// its window, what the test before it owned. Under the proven switch that
// is the work the previous end cut (settledSince) that its connections
// still hold, which only an end that returned unsettled leaves (its request
// was cancelled, it failed before its cut and cut as it failed, or its
// watchdog gave up on others), and nothing else: a
// call the application made after that cut is the new test's, or
// background work, so an application that keeps calling (a timer whose
// calls a slow handler holds) never holds a begin, and neither does a call
// made before the first test. A connection of that work the begin's own
// watchdog finds stuck is given up on (InFlightScope.GiveUp: no later wait
// waits for it) and logged, and the begin goes on: it was the previous
// test's, whose end has already failed. With the switch off a begin
// settles as it always did (settle). It returns the proxy listener's drops,
// read once settled, which open the test's drops' window (markDrops).
func (a *Agent) settleBegin(ctx context.Context, answers bool) (models.ListenDrops, error) {
	if !provenswitch.On() {
		return a.settle(ctx, answers)
	}
	// A capture that settles itself leaves a begin nothing to wait for: its
	// ends cut no scope that a begin would settle, and what it lost between
	// two tests is the next end's. It is told of the begin, so that what it
	// lost before the first one is not (CaptureSettler.NoteTestBegin).
	if cs, ok := settlesByCapture(answers); ok {
		cs.NoteTestBegin()
		return models.ListenDrops{}, nil
	}
	ir, ok := a.Proxy.(coreAgent.InFlightReader)
	if !ok || a.Proxy == nil {
		return models.ListenDrops{}, errors.New("this keploy agent's proxy cannot tell which calls it still holds")
	}
	a.provenMu.Lock()
	owned := a.proven.owned
	a.provenMu.Unlock()
	if owned != nil {
		if err := a.settleOwned(ctx, ir, owned, answers); err != nil {
			return models.ListenDrops{}, err
		}
	}
	c, err := a.ProxyConns(ctx)
	if err != nil {
		return models.ListenDrops{}, fmt.Errorf("keploy cannot count the calls still waiting for its proxy: %w", err)
	}
	return c.ListenDrops, nil
}

// settlesByCapture returns the registered CaptureSettler when a settle goes
// through it: under the proven switch, in a record. A replay (answers) serves
// its mocks through keploy's proxy whatever recorded them, and settles there;
// with the switch off nothing changes.
func settlesByCapture(answers bool) (CaptureSettler, bool) {
	cs := captureSettler
	return cs, cs != nil && !answers && provenswitch.On()
}

// StartupCaptureLoss is what the capture a record's tests settle through
// (settlesByCapture) lost before the run's first test began, which no test
// is charged with: the record keeps it for its summary
// (models.StartupCaptureLoss). Empty when the run's tests settle through
// keploy's proxy, whose start-up losses are the run's
// (UnchargedCaptureLosses), and when the capture does not say
// (StartupLossReporter).
func (a *Agent) StartupCaptureLoss(context.Context) (models.StartupCaptureLoss, error) {
	cs, ok := settlesByCapture(a.replaying())
	if !ok {
		return models.StartupCaptureLoss{}, nil
	}
	r, ok := cs.(StartupLossReporter)
	if !ok {
		return models.StartupCaptureLoss{}, nil
	}
	return models.StartupCaptureLoss{Lost: r.StartupCaptureLoss()}, nil
}

// settleCapture settles a record's test end through cs, under the settle's
// watchdog (watchSettle on cs.SettleProgress): a wait none of whose counts
// moves for settleStallLimit wake-ups fails, as a proxy's does. An error says
// keploy cannot tell: the test is not judged.
func (a *Agent) settleCapture(ctx context.Context, cs CaptureSettler) error {
	if err := a.watchedSettle(ctx, cs, cs.SettleCapture); err != nil {
		return fmt.Errorf("keploy cannot tell that the capture holds no call of the test: %w", err)
	}
	return nil
}

// watchedSettle runs settle, a wait on cs, under the settle's watchdog
// (watchSettle on cs.SettleProgress): when it fires, or ctx ends, the error
// is why.
func (a *Agent) watchedSettle(ctx context.Context, cs CaptureSettler, settle func(context.Context) error) error {
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	stop := a.watchSettle(ctx, cancel, cs.SettleProgress)
	err := settle(ctx)
	stop()
	if err == nil {
		return nil
	}
	if cause := context.Cause(ctx); cause != nil {
		return cause
	}
	return err
}

// lostSinceLastEnd is what the capture a record's tests settle through
// (settlesByCapture) lost since the run's last test ended
// (LastEndLossReporter), read under the settle's watchdog as an end's settle
// is. Nothing when the run's tests settle through keploy's proxy, whose
// losses after the last test are the agent's own counts
// (UnchargedCaptureLosses), and when the capture does not say. An error says
// keploy cannot tell what it lost.
func (a *Agent) lostSinceLastEnd(ctx context.Context) (models.CaptureLosses, error) {
	cs, ok := settlesByCapture(a.replaying())
	if !ok {
		return nil, nil
	}
	r, ok := cs.(LastEndLossReporter)
	if !ok {
		return nil, nil
	}
	var lost models.CaptureLosses
	err := a.watchedSettle(ctx, cs, func(ctx context.Context) error {
		var err error
		lost, err = r.CaptureLossesSinceLastEnd(ctx)
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("keploy cannot tell what the capture lost after the last test: %w", err)
	}
	return lost, nil
}

// beginWindow, under the proven switch, cuts where a test's window begins on
// the proxy's connections (coreAgent.InFlightWindower), once the calls
// before it are settled: the test's end then waits only for the requests
// opened since (an HTTP/2 or gRPC stream opened before the test began, a
// long-lived one that keeps sending, is not the test's and never holds its
// end). With the switch off it cuts nothing.
func (a *Agent) beginWindow() {
	if !provenswitch.On() {
		return
	}
	if w, ok := a.Proxy.(coreAgent.InFlightWindower); ok {
		w.InFlightBeginWindow()
	}
}

// settleOwned waits until sc, a previous end's scope, holds no work, its
// watchdog weighing each connection that still holds some. One the watchdog
// finds stuck is given up on, and the wait goes on for the others.
func (a *Agent) settleOwned(ctx context.Context, ir coreAgent.InFlightReader, sc coreAgent.InFlightScope, answers bool) error {
	for {
		settled, stalled, err := a.settleOwnedOnce(ctx, ir, sc, answers)
		if settled || err != nil {
			return err
		}
		ids := make([]uint64, 0, len(stalled))
		for _, k := range stalled {
			if id, err := strconv.ParseUint(k, 10, 64); err == nil {
				ids = append(ids, id)
			}
		}
		if held := sc.GiveUp(ids, answers); len(held) > 0 {
			a.logger.Warn("keploy's agent made no progress settling the previous test's calls before the next test began; it gave up on them (that test's end had already failed)", zap.Strings("connections", held))
		}
	}
}

// settleOwnedOnce is one watched wait of settleOwned: settled once sc holds
// no work, else the keys its watchdog found stuck. Work already settled
// needs no watchdog.
func (a *Agent) settleOwnedOnce(ctx context.Context, ir coreAgent.InFlightReader, sc coreAgent.InFlightScope, answers bool) (settled bool, stalled []string, err error) {
	if pending, _ := sc.State(answers); pending == 0 {
		return true, nil, nil
	}
	wctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	stop := a.watchSettle(wctx, cancel, func() map[string]uint64 {
		out := map[string]uint64{}
		for id, n := range sc.PendingProgress(answers) {
			out[strconv.FormatUint(id, 10)] = n
		}
		return out
	})
	defer stop()
	for {
		changed := ir.InFlightChanged()
		if pending, _ := sc.State(answers); pending == 0 {
			return true, nil, nil
		}
		select {
		case <-changed:
		case <-wctx.Done():
			var np noProgress
			if cause := context.Cause(wctx); errors.As(cause, &np) {
				return false, np.stalled, nil
			}
			return false, nil, context.Cause(wctx)
		}
	}
}
