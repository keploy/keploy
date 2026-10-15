package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	coreAgent "go.keploy.io/server/v3/pkg/agent"
)

// A test's end count (design v14 §5.2; release1-cut §5.1 item 5; verify-v14
// M1 and M2), under the proven switch. Before a test is judged at its end, and
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
//     has not answered: its handler is blocked reading it, its parser holds
//     none it read ahead (integrations.HoldRequest), and its handler is done
//     with it if it closed. A record skips it: a relayed request waits for
//     its real server, which the end never waits on.
//
// Each step waits on an event: an accept, a read beginning or returning, a
// request held or released, a connection closing. No clock decides. A test
// whose calls keploy cannot count (models.ErrProxyConnsUnsupported, or hooks
// not loaded) is not settled: "unknown" is never "none in flight". Nor is one
// with a connection whose parser reads ahead and does not say what it holds
// (holdsUnknown), or one during which the kernel dropped packets at the
// proxy's listener (dropped): its end leaves it not verified. The run's read
// of its misses settles the same way, so a call after the last test's end is
// read, never filed after it.
//
// The wait has a minimal watchdog (design v14 §8.4): it wakes once a second
// and counts the wake-ups in a row at which nothing moved (no accept, no
// change of a connection's state). At settleStallLimit it fails the wait,
// and the test is not judged. It counts its own wake-ups, not seconds: a
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
// (integrations.RequestHolding), so whether it still holds one of the test's
// calls cannot be told. A test's end that finds one is not verified; a begin
// goes on (the test's end, or the run's last read, says it).
var errHoldsUnknown = errors.New("a connection's held requests are unknown")

// holdsUnknown is errHoldsUnknown for the protocols it names.
type holdsUnknown []string

func (h holdsUnknown) Error() string {
	return "keploy cannot tell whether " + strings.Join(h, ", ") + " on a connection still holds a request: its parser reads ahead of its answers and does not say which requests it holds"
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
	// The watchdog reads the counts at every wake-up: that read is also what
	// ends an application connection's wait when the kernel dropped its
	// event (WaitAppConnsSettled).
	stop := a.watchSettle(ctx, cancel, func() uint64 {
		c, err := a.ProxyConns(ctx)
		if err != nil {
			return ir.InFlightProgress()
		}
		return c.Accepted + c.AppSettled + ir.InFlightProgress()
	})
	defer stop()
	if err := a.WaitProxySettled(ctx, counts); err != nil {
		if cause := context.Cause(ctx); cause != nil {
			return 0, cause
		}
		return 0, err
	}
	for {
		changed := ir.InFlightChanged()
		if ir.InFlightPending(answers) == 0 {
			c, err := a.ProxyConns(ctx)
			if err != nil {
				return 0, fmt.Errorf("keploy cannot count the calls still waiting for its proxy: %w", err)
			}
			// Settled, but for a connection whose parser reads ahead and does
			// not say which requests it holds: it may hold one.
			if unknown := ir.InFlightUnknown(answers); len(unknown) > 0 {
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

// watchSettle is the settle wait's watchdog: progress is a count that moves
// with every sign of progress. It cancels ctx with errNoProgress after
// settleStallLimit wake-ups in a row at which progress did not move.
func (a *Agent) watchSettle(ctx context.Context, cancel context.CancelCauseFunc, progress func() uint64) (stop func()) {
	wake, stopWake := settleWake(ctx)
	quit, done := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		last, stalls := progress(), 0
		for {
			select {
			case <-quit:
				return
			case _, ok := <-wake:
				if !ok {
					return
				}
			}
			if now := progress(); now != last {
				last, stalls = now, 0
			} else {
				stalls++
			}
			fire := stalls >= settleStallLimit
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
