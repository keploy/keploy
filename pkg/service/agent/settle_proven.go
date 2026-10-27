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
// or says that for now it cannot tell (holdsUnknown), or one during which
// the kernel dropped packets at the proxy's listener (dropped): its end
// leaves it not verified. The run's read
// of its misses settles the same way, so a call after the last test's end is
// read, never filed after it.
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
// with its begin's (settledDrops). An error says keploy cannot tell: the test
// is not judged. holdsUnknown comes with the drops.
func (a *Agent) settle(ctx context.Context, answers bool) (uint64, error) {
	return a.settleKeeping(ctx, answers, nil)
}

// settleKeeping is settle; keep, when set, is given the scope the settle
// cuts (coreAgent.InFlightScope) as it cuts it: what a window's end owned
// (settledSince), which the next begin waits for (settleBegin). Under the
// proven switch a settle that fails before its cut cuts as it fails: the
// work its connections then held is still the window's, and a call of it
// answered later is never the next test's.
func (a *Agent) settleKeeping(ctx context.Context, answers bool, keep func(coreAgent.InFlightScope)) (uint64, error) {
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
		return 0, errors.New("this keploy agent's proxy cannot tell which calls it still holds")
	}
	counts, err := a.ProxyConns(ctx)
	if err != nil {
		return 0, fmt.Errorf("keploy cannot count the calls still waiting for its proxy: %w", err)
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
			return 0, cause
		}
		return 0, err
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
				return 0, fmt.Errorf("keploy cannot count the calls still waiting for its proxy: %w", err)
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
					return 0, gaveUp{held: held}
				}
			}
			return 0, cause
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

// droppedWhile is the error for a test (or a run's tail) during which the
// kernel dropped packets at the proxy's listener: from its drops at the start,
// was, to those at its end, now. A SYN or final ACK it dropped is a
// connection in neither of the proxy's counts until the client retransmits
// it, so a call of the test may not have reached keploy; nil when none was.
func droppedWhile(what string, was, now uint64) error {
	if now == was {
		return nil
	}
	return dropped{what: what, n: now - was}
}

// errDropped: packets were dropped at the proxy's listener while a window
// was open (droppedWhile).
var errDropped = errors.New("packets were dropped at keploy's proxy listener")

// dropped is errDropped for one window.
type dropped struct {
	what string
	n    uint64
}

func (d dropped) Error() string {
	return fmt.Sprintf("the kernel dropped %d packet(s) at keploy's proxy listener while %s ran (its accept queue full among the causes), so a call may not have reached keploy", d.n, d.what)
}

func (d dropped) Is(target error) bool { return target == errDropped }

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

// markDrops notes the proxy listener's drops read as a window began (a
// test's begin, or the settling that ended the window before).
func (a *Agent) markDrops(drops uint64) {
	a.provenMu.Lock()
	a.proven.drops, a.proven.dropsKnown = drops, true
	a.provenMu.Unlock()
}

// settledSince settles (settle) the window what ends, and fails it when the
// kernel dropped packets at the proxy's listener while it was open
// (droppedWhile): a test whose drops differ between its begin and its end is
// not verified. The drops read now begin the next window.
func (a *Agent) settledSince(ctx context.Context, answers bool, what string) error {
	a.provenMu.Lock()
	was, known := a.proven.drops, a.proven.dropsKnown
	a.provenMu.Unlock()
	// What the window owned at its cut is what the next begin waits for
	// (settleBegin).
	var owned coreAgent.InFlightScope
	drops, err := a.settleKeeping(ctx, answers, func(sc coreAgent.InFlightScope) { owned = sc })
	a.provenMu.Lock()
	a.proven.owned = owned
	a.provenMu.Unlock()
	if err != nil && !errors.Is(err, errHoldsUnknown) {
		return err
	}
	// A window's end charges it the calls queued behind lost work.
	var bl behindLost
	if errors.As(err, &bl) {
		bl.charge()
	}
	a.markDrops(drops)
	if known {
		if dropped := droppedWhile(what, was, drops); dropped != nil && err == nil {
			return dropped
		}
	}
	return err
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
// read once settled, which the test's end compares.
func (a *Agent) settleBegin(ctx context.Context, answers bool) (uint64, error) {
	if !provenswitch.On() {
		return a.settle(ctx, answers)
	}
	ir, ok := a.Proxy.(coreAgent.InFlightReader)
	if !ok || a.Proxy == nil {
		return 0, errors.New("this keploy agent's proxy cannot tell which calls it still holds")
	}
	a.provenMu.Lock()
	owned := a.proven.owned
	a.provenMu.Unlock()
	if owned != nil {
		if err := a.settleOwned(ctx, ir, owned, answers); err != nil {
			return 0, err
		}
	}
	c, err := a.ProxyConns(ctx)
	if err != nil {
		return 0, fmt.Errorf("keploy cannot count the calls still waiting for its proxy: %w", err)
	}
	return c.ListenDrops, nil
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
