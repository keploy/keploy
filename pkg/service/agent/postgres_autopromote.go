// Package agent — postgres_autopromote.go
//
// Cross-window autopromote glue between the agent layer and the
// integrations-side heuristic.
//
// Why this lives here
// -------------------
//
// The integrations-side ApplyAutopromote runs inside PerTestProvider
// AFTER GetPerTestMocksInWindow has filtered the per-test pool down
// to the active test's window. A SQL hash that the application fires
// once per test across N tests presents to that pass as N separate
// cohort_size=1 evaluations and never trips the heuristic's len>=2
// gate. The listmonk auth-middleware session-lookup is the canonical
// example: 33 recorded invocations, one per test, all required during
// replay of every test that follows their respective record-time test.
// At replay-time, only the matching test's window contains the mock,
// so cross-test bleed must be handled before the per-test partition,
// not after.
//
// The agent's ApplyMockFilters is the natural cut: it owns the full
// originalFiltered slice and runs immediately upstream of
// FilterPerTestAndLaxPromotedTierAware (which performs the per-test
// window-partition). Calling EvaluateCrossWindowAutopromote here gives
// the integrations heuristic the un-windowed view it needs and lets
// the agent re-route promoted mocks from the per-test pool into the
// session pool in a single in-place pass.
//
// Env gate
// --------
//
// Off-switch: KEPLOY_PG_V3_CROSS_WINDOW_AUTOPROMOTE=0 disables the
// upstream pass entirely (the legacy ApplyAutopromote in PerTestProvider
// still runs). The shared KEPLOY_PG_V3_AUTOPROMOTE_LIFETIME=0 setting
// also short-circuits the heuristic via EvaluateCrossWindowAutopromote
// itself; this knob is the agent-local override for operators who want
// per-window autopromote but not the cross-window path (e.g. while
// triaging a regression report).
package agent

import (
	"os"
	"strings"
	"sync"

	v3types "github.com/keploy/integrations/pkg/postgres/v3/types"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// envCrossWindowAutopromote is the agent-local off-switch. Defaults
// to ON; explicit "0" / "false" / "no" / "off" disables. Any other
// value falls back to the default (ON) so a typo does not silently
// disable a fix.
const envCrossWindowAutopromote = "KEPLOY_PG_V3_CROSS_WINDOW_AUTOPROMOTE"

var (
	crossWindowEnabledOnce sync.Once
	crossWindowEnabled     bool
)

func crossWindowAutopromoteEnabled() bool {
	crossWindowEnabledOnce.Do(func() {
		crossWindowEnabled = true
		v, ok := os.LookupEnv(envCrossWindowAutopromote)
		if !ok {
			return
		}
		switch strings.ToLower(strings.TrimSpace(v)) {
		case "0", "false", "no", "off":
			crossWindowEnabled = false
		}
	})
	return crossWindowEnabled
}

// applyCrossWindowAutopromote runs the integrations-side cross-window
// autopromote heuristic on the supplied per-test mock pool BEFORE the
// caller window-partitions it. Returns the (possibly-shortened)
// per-test pool and the slice of mocks promoted to the session tier
// — the caller appends the second slice to its session pool input
// for SetMocksWithWindow.
//
// Behaviour:
//
//   - When the env gate is disabled, returns the input unchanged and
//     a nil promoted slice.
//   - When the heuristic returns no decisions, same.
//   - For every promoted mock: mutates mock.TestModeInfo.Lifetime to
//     LifetimeSession, sets LifetimeDerived=true so a downstream
//     DeriveLifetime call cannot revert the decision, and removes
//     the mock from the returned per-test slice (a single allocation
//     of len(perTestIn) - len(promoted) capacity, no in-place
//     rewrite of the input slice header).
//
// Why mutate mock.TestModeInfo.Lifetime in place rather than build a
// fresh copy: the same *models.Mock value flows through the proxy's
// MockManager partition and on to the dispatcher's index loaders. A
// deep copy here would make MockManager and the dispatcher disagree
// on lifetime for the same mock (the deep copy would partition into
// session, the original would still be PerTest in any other slice
// the caller still holds a pointer to). Mutating in place keeps a
// single source of truth across every consumer.
//
// Logger may be nil; the integrations helper handles nil internally.
func applyCrossWindowAutopromote(logger *zap.Logger, perTestIn []*models.Mock) (perTest, promoted []*models.Mock) {
	if !crossWindowAutopromoteEnabled() {
		return perTestIn, nil
	}
	if len(perTestIn) < 2 {
		return perTestIn, nil
	}
	decisions := v3types.EvaluateCrossWindowAutopromote(perTestIn, logger)
	if len(decisions) == 0 {
		return perTestIn, nil
	}
	keep := make([]*models.Mock, 0, len(perTestIn))
	promoted = make([]*models.Mock, 0, len(decisions))
	for _, m := range perTestIn {
		if m == nil {
			continue
		}
		newLT, ok := decisions[m.Name]
		if !ok {
			keep = append(keep, m)
			continue
		}
		// Mutate in place. The proxy and the dispatcher both read
		// TestModeInfo.Lifetime from this same pointer; partitioning
		// a deep copy would create a split-brain across the two
		// readers. LifetimeDerived is the "we already classified this
		// mock; do not redo" signal that DeriveLifetime honours, so
		// setting it suppresses any downstream reversion.
		m.TestModeInfo.Lifetime = newLT
		m.TestModeInfo.LifetimeDerived = true
		promoted = append(promoted, m)
	}
	if logger != nil {
		logger.Info("cross-window autopromote moved mocks from per-test to session pool",
			zap.Int("input", len(perTestIn)),
			zap.Int("promoted", len(promoted)),
			zap.Int("perTestRemaining", len(keep)))
	}
	return keep, promoted
}
