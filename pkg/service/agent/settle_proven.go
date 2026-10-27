package agent

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	coreAgent "go.keploy.io/server/v3/pkg/agent"
)

// A test's end count (design v14 §5.2; release1-cut §5.1 item 5; verify-v14
// M1 and M2), under the proven switch; with it off, the same settle before a
// read of a test's misses, for an agent started settling reads whose calls
// are counted (Agent.settlesReads). Before a test is judged at its end, and
// before one opens at its begin, every call the application made must be in
// the proxy's hands, and in a replay answered:
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
// and a deadlock can.

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
	ir, ok := a.Proxy.(coreAgent.InFlightReader)
	if !ok || a.Proxy == nil {
		return 0, errors.New("this keploy agent's proxy cannot tell which calls it still holds")
	}
	counts, err := a.ProxyConns(ctx)
	if err != nil {
		return 0, fmt.Errorf("keploy cannot count the calls still waiting for its proxy: %w", err)
	}
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
	stop := a.watchSettle(ctx, cancel, func() map[string]uint64 {
		c, err := a.ProxyConns(ctx)
		if !working.Load() {
			if err != nil {
				return nil
			}
			v := min(c.AppSettled, counts.AppConns) + min(c.Accepted, accepts.Load())
			if pb, ok := a.Proxy.(coreAgent.PairedBytesWaiter); ok {
				v += pb.PairedBytesProgress()
			}
			return map[string]uint64{"counts": v}
		}
		out := map[string]uint64{}
		for id, n := range ir.InFlightPendingProgress(answers) {
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
	working.Store(true)
	for {
		changed := ir.InFlightChanged()
		// One view of the connections (InFlightState): read apart, a parser
		// that ended a state it could not tell between the two reads would be
		// seen neither busy nor unknown.
		pending, unknown := ir.InFlightState(answers)
		if pending == 0 {
			c, err := a.ProxyConns(ctx)
			if err != nil {
				return 0, fmt.Errorf("keploy cannot count the calls still waiting for its proxy: %w", err)
			}
			// Settled, but for a connection whose parser reads ahead and does
			// not say which requests it holds, or says that for now it cannot
			// tell: it may hold one.
			if len(unknown) > 0 {
				return c.ListenDrops, holdsUnknown(unknown)
			}
			return c.ListenDrops, nil
		}
		select {
		case <-changed:
		case <-ctx.Done():
			return 0, context.Cause(ctx)
		}
	}
}

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
			fire := false
			for k, v := range now {
				if was, ok := last[k]; ok && was == v {
					stalls[k]++
				} else {
					stalls[k] = 0
				}
				fire = fire || stalls[k] >= settleStallLimit
			}
			for k := range stalls {
				if _, ok := now[k]; !ok {
					delete(stalls, k)
				}
			}
			last = now
			if fire {
				cancel(errNoProgress)
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
	drops, err := a.settle(ctx, answers)
	if err != nil && !errors.Is(err, errHoldsUnknown) {
		return err
	}
	a.markDrops(drops)
	if known {
		if dropped := droppedWhile(what, was, drops); dropped != nil && err == nil {
			return dropped
		}
	}
	return err
}
