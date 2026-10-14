package agent

import (
	"context"
	"errors"
	"fmt"
	"time"

	coreAgent "go.keploy.io/server/v3/pkg/agent"
)

// A test's end count (design v14 §5.2; release1-cut §5.1 item 5; verify-v14
// M1 and M2), under the proven switch. Before a test is judged at its end, and
// before one opens at its begin, every call the application made must be in
// the proxy's hands, and in a replay answered:
//
//  1. every connection the kernel completed to the proxy has been accepted
//     (ProxyConns, then WaitProxyAccepted on the Established it read);
//  2. no accepted connection holds bytes the proxy has not read;
//  3. in a replay, no accepted connection holds a request the proxy read and
//     has not answered. A record skips it: a relayed request waits for its
//     real server, which the end never waits on.
//
// Each step waits on an event: an accept, a read beginning or returning, a
// connection closing. No clock decides. A test whose calls keploy cannot
// count (models.ErrProxyConnsUnsupported, or hooks not loaded) is not
// settled: "unknown" is never "none in flight".
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

// settle waits until the test's calls are all in the proxy's hands, and, in a
// replay (answers), answered. An error says keploy cannot tell: the test is
// not judged.
func (a *Agent) settle(ctx context.Context, answers bool) error {
	ir, ok := a.Proxy.(coreAgent.InFlightReader)
	if !ok || a.Proxy == nil {
		return errors.New("this keploy agent's proxy cannot tell which calls it still holds")
	}
	counts, err := a.ProxyConns(ctx)
	if err != nil {
		return fmt.Errorf("keploy cannot count the calls still waiting for its proxy: %w", err)
	}
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	stop := a.watchSettle(ctx, cancel, func() uint64 {
		c, err := a.ProxyConns(ctx)
		if err != nil {
			return ir.InFlightProgress()
		}
		return c.Accepted + ir.InFlightProgress()
	})
	defer stop()
	if err := a.WaitProxyAccepted(ctx, counts.Established); err != nil {
		if cause := context.Cause(ctx); cause != nil {
			return cause
		}
		return err
	}
	for {
		changed := ir.InFlightChanged()
		if ir.InFlightPending(answers) == 0 {
			return nil
		}
		select {
		case <-changed:
		case <-ctx.Done():
			return context.Cause(ctx)
		}
	}
}

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
