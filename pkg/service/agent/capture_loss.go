package agent

import (
	"context"
	"errors"
	"fmt"

	coreAgent "go.keploy.io/server/v3/pkg/agent"
	"go.keploy.io/server/v3/pkg/models"
)

// CaptureLosses counts every kind of capture loss the agent can see, each
// only ever growing for its life (models.CaptureLosses): the processes the
// kernel refused a place in the PID maps (the hooks), and the calls the proxy
// intercepted and could not record or mock (a TLS client that rejected
// keploy's certificate, a TLS upgrade it could not make). Each later
// source (design v14 §3.2) adds its kind here. With neither source able to
// count it gives models.ErrCaptureLossesUnsupported, "unknown"; a count that
// could not be read is its own error.
func (a *Agent) CaptureLosses(ctx context.Context) (models.CaptureLosses, error) {
	// Every report the kernel made before this read is counted first, so a
	// call the application made before it is in it (a test's end, the run's
	// last read), however late the agent's goroutines run.
	if d, ok := a.Hooks.(coreAgent.CaptureLossDrainer); ok && a.Hooks != nil {
		if err := d.DrainCaptureLosses(ctx); err != nil {
			return nil, err
		}
	}
	var out models.CaptureLosses
	proxy, proxyCounts := a.Proxy.(coreAgent.CaptureLossReporter)
	if proxyCounts && a.Proxy != nil {
		out = append(out, proxy.CaptureLosses()...)
	}
	// The hooks' own: the application's connections lost before the proxy
	// got them, and its AF_UNIX connects to sockets no member of it owns
	// (under the proven switch; none otherwise).
	if hooks, ok := a.Hooks.(coreAgent.CaptureLossReporter); ok && a.Hooks != nil {
		out = append(out, hooks.CaptureLosses()...)
	}
	// A count of theirs that could not start is unknown, never 0: not
	// models.ErrCaptureLossesUnsupported (an agent that counts nothing),
	// which leaves every test as it was.
	if blind, ok := a.Hooks.(coreAgent.CaptureLossErrReporter); ok && a.Hooks != nil {
		if err := blind.CaptureLossesErr(); err != nil {
			return nil, err
		}
	}
	refusals, err := a.PIDMapRefusals(ctx)
	switch {
	case errors.Is(err, models.ErrPIDMapRefusalsUnsupported):
		if !proxyCounts {
			return nil, fmt.Errorf("%w: %v", models.ErrCaptureLossesUnsupported, err)
		}
	case err != nil:
		return nil, fmt.Errorf("read the PID map refusals: %w", err)
	default:
		// One kind per map, so what grew since a mark names its own map.
		if refusals.Target > 0 {
			out = append(out, models.CaptureLoss{Kind: models.CaptureLossPIDMap, Count: refusals.Target, What: pidMapCause("target_namespace_pids", refusals.LastTargetErr)})
		}
		if refusals.Excluded > 0 {
			out = append(out, models.CaptureLoss{Kind: models.CaptureLossPIDMapExcluded, Count: refusals.Excluded, What: pidMapCause("excluded_pids", refusals.LastExcludedErr)})
		}
	}
	return out, nil
}

// pidMapCause names a PID map's latest refusal by the map and its errno, never
// a value the app sent.
func pidMapCause(m string, err int32) string {
	if c := models.PIDMapRefusalCause(err); c != "" {
		return m + ", " + c
	}
	return m
}

// captureLossMark is the run's capture-loss counts at its start, which each
// test's end counts from. known is false when they could not be read; err
// says why.
type captureLossMark struct {
	at    models.CaptureLosses
	known bool
	err   error
}

// markCaptureLosses reads the counts the run's tests are counted from. Called
// as a session starts.
func (a *Agent) markCaptureLosses(ctx context.Context) captureLossMark {
	at, err := a.CaptureLosses(ctx)
	if err != nil {
		return captureLossMark{err: err}
	}
	return captureLossMark{at: at, known: true}
}

// UnchargedCaptureLosses is what the agent could not capture since the run
// began that no test was charged with: every test's end charges what was lost
// up to it (chargeCaptureLosses: the open test and every later one for a
// taint, the test it happened in for a test's own kind), so this is what was
// lost after the last test ended, or before a test process's claim. The CLI
// reads it once the run's last test has ended; a record's judging fails the
// run on it (design v14 §7.5: evidence charged to no test). A standing state
// (unix_unseen) is kept by every mark and is in it whatever was charged.
func (a *Agent) UnchargedCaptureLosses(ctx context.Context) (models.CaptureLosses, error) {
	a.provenMu.Lock()
	mark, charged, outside := a.proven.losses, a.proven.lossesCharged, a.proven.unreadOutside
	a.provenMu.Unlock()
	_, now, err := a.captureLossesSince(ctx, mark, nil)
	if err != nil {
		return nil, err
	}
	uncharged := sinceRun(now, mark).Since(charged)
	if !a.replaying() {
		// A record charges a container read the daemon did not answer by
		// the mocks its stamps marked (unreadMock): the run's are those of
		// the time outside tests, whatever the count.
		uncharged = uncharged.Without(models.CaptureLossContainerUnread).Plus(outside)
	}
	return uncharged, nil
}

// sinceRun is what was lost since the run began, read as now: what keploy
// cannot capture at all is counted from the hooks' load, not the session's
// start -- the application may open its Unix-socket connection before its
// first test claims the session, and every test after that rides on it
// (decision 20) -- and every other kind from the run's mark.
func sinceRun(now models.CaptureLosses, mark captureLossMark) models.CaptureLosses {
	return now.SinceWithin(mark.at, nil)
}

// captureLossesSince is what a test is charged as it ends (models.Charged):
// what was lost since the run began (sinceRun), which taints the test open
// when it happened and every test after it (design v14 §3.2), so every test
// that ends after such a loss carries it; and what of a test's own kind
// (PerTest) was lost since the previous test ended (test; the run's mark for
// the first), which is that test's only. It returns the counts read now, the
// next test's mark (a read that failed leaves the next test to count its own
// kinds from the test before, so it is charged this one's too). A mark or a
// read that failed is an error: a test keploy cannot tell was fully captured
// is not judged as if it were. Hooks that cannot count at all (other
// platforms' hooks, which release 1 does not prove) leave every test as it
// was, as the PID-map check does for a session
// (models.ErrPIDMapRefusalsUnsupported).
func (a *Agent) captureLossesSince(ctx context.Context, mark captureLossMark, test models.CaptureLosses) (lost, now models.CaptureLosses, err error) {
	if errors.Is(mark.err, models.ErrCaptureLossesUnsupported) {
		return nil, nil, nil
	}
	if !mark.known {
		if mark.err == nil {
			return nil, nil, errors.New("keploy did not read its capture-loss counts when the run began")
		}
		return nil, nil, fmt.Errorf("at the start of the run: %w", mark.err)
	}
	now, err = a.CaptureLosses(ctx)
	if err != nil {
		// Hooks that counted at the run's start and no longer can: the count
		// was lost, which is not "never counted".
		return nil, nil, err
	}
	if test == nil {
		test = mark.at
	}
	return now.Charged(mark.at, nil, test), now, nil
}
