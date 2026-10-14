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
// intercepted and could not record or mock (a client-side TLS handshake that
// failed against keploy's CA, a TLS upgrade it could not make). Each later
// source (design v14 §3.2) adds its kind here. With neither source able to
// count it gives models.ErrCaptureLossesUnsupported, "unknown"; a count that
// could not be read is its own error.
func (a *Agent) CaptureLosses(ctx context.Context) (models.CaptureLosses, error) {
	var out models.CaptureLosses
	proxy, proxyCounts := a.Proxy.(coreAgent.CaptureLossReporter)
	if proxyCounts && a.Proxy != nil {
		out = append(out, proxy.CaptureLosses()...)
	}
	// The hooks' own (an AF_UNIX connect to a socket no member owns).
	if hooks, ok := a.Hooks.(coreAgent.CaptureLossReporter); ok && a.Hooks != nil {
		out = append(out, hooks.CaptureLosses()...)
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
		if n := refusals.Target + refusals.Excluded; n > 0 {
			out = append(out, models.CaptureLoss{Kind: models.CaptureLossPIDMap, Count: n, What: pidMapExample(refusals)})
		}
	}
	return out, nil
}

// pidMapExample names the latest refusal by its map and errno, never a value
// the app sent.
func pidMapExample(r models.PIDMapRefusals) string {
	switch {
	case r.Target > 0:
		if c := models.PIDMapRefusalCause(r.LastTargetErr); c != "" {
			return "target_namespace_pids: " + c
		}
		return "target_namespace_pids"
	case r.Excluded > 0:
		if c := models.PIDMapRefusalCause(r.LastExcludedErr); c != "" {
			return "excluded_pids: " + c
		}
		return "excluded_pids"
	}
	return ""
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

// captureLossesSince is what was lost since the run began, at a test's end:
// it taints the test open when it happened and every test after it (design
// v14 §3.2), so every test that ends after a loss carries it. A mark or a read
// that failed is an error: a test keploy cannot tell was fully captured is
// not judged as if it were. Hooks that cannot count at all (other platforms'
// hooks, which release 1 does not prove) leave every test as it was, as the
// PID-map check does for a session (models.ErrPIDMapRefusalsUnsupported).
func (a *Agent) captureLossesSince(ctx context.Context, mark captureLossMark) (models.CaptureLosses, error) {
	if errors.Is(mark.err, models.ErrCaptureLossesUnsupported) {
		return nil, nil
	}
	if !mark.known {
		if mark.err == nil {
			return nil, errors.New("keploy did not read its capture-loss counts when the run began")
		}
		return nil, fmt.Errorf("at the start of the run: %w", mark.err)
	}
	now, err := a.CaptureLosses(ctx)
	if err != nil {
		// Hooks that counted at the run's start and no longer can: the count
		// was lost, which is not "never counted".
		return nil, err
	}
	return now.Since(mark.at), nil
}
