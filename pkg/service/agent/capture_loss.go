package agent

import (
	"context"
	"errors"
	"fmt"

	"go.keploy.io/server/v3/pkg/models"
)

// CaptureLosses counts every kind of capture loss the agent can see, each
// only ever growing for its life (models.CaptureLosses). Today that is the
// processes the kernel refused a place in the PID maps; each later source
// (design v14 §3.2) adds its kind here. Hooks that cannot count give
// models.ErrCaptureLossesUnsupported, "unknown"; a count that could not be
// read is its own error.
func (a *Agent) CaptureLosses(ctx context.Context) (models.CaptureLosses, error) {
	refusals, err := a.PIDMapRefusals(ctx)
	if errors.Is(err, models.ErrPIDMapRefusalsUnsupported) {
		return nil, fmt.Errorf("%w: %v", models.ErrCaptureLossesUnsupported, err)
	}
	if err != nil {
		return nil, fmt.Errorf("read the PID map refusals: %w", err)
	}
	var out models.CaptureLosses
	if n := refusals.Target + refusals.Excluded; n > 0 {
		out = append(out, models.CaptureLoss{Kind: models.CaptureLossPIDMap, Count: n, What: pidMapExample(refusals)})
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
// not judged as if it were.
func (a *Agent) captureLossesSince(ctx context.Context, mark captureLossMark) (models.CaptureLosses, error) {
	if !mark.known {
		if mark.err == nil {
			return nil, errors.New("keploy did not read its capture-loss counts when the run began")
		}
		return nil, fmt.Errorf("at the start of the run: %w", mark.err)
	}
	now, err := a.CaptureLosses(ctx)
	if err != nil {
		return nil, err
	}
	return now.Since(mark.at), nil
}
