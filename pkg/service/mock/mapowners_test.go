// mapOwners builds mappings.yaml. It prefers the owner the agent stamped on each
// capture at emit time and keeps correlateScopes -- the after-the-fact timestamp
// bucketing -- as the fallback for anything unstamped, and as a cross-check on
// everything that is.
package mock

import (
	"testing"

	"github.com/stretchr/testify/require"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

func entryNames(byTest map[string][]models.MockEntry) map[string][]string {
	out := map[string][]string{}
	for test, entries := range byTest {
		for _, e := range entries {
			out[test] = append(out[test], e.Name)
		}
	}
	return out
}

// With no stamps at all -- an agent that predates the stamp, or a run whose
// captures all fell outside every scope -- mapOwners must be correlateScopes.
func TestMapOwnersFallsBackWholesaleWhenNothingIsStamped(t *testing.T) {
	windows := []models.ScopeWindow{
		{Name: "t1", Start: ts(10), End: ts(20)},
		{Name: "t2", Start: ts(30), End: ts(40)},
	}
	mocks := []capturedMock{
		{name: "mock-0", ts: ts(15)},
		{name: "mock-1", ts: ts(35)},
		{name: "boot", ts: ts(1)},
	}
	require.Equal(t,
		entryNames(correlateScopes(append([]models.ScopeWindow(nil), windows...), mocks)),
		entryNames(mapOwners(zap.NewNop(), windows, mocks)))
}

// A stamp wins over the timestamp bucketing, and an unstamped mock in the same
// run still gets the fallback.
func TestMapOwnersPrefersTheStampAndStillCorrelatesTheRest(t *testing.T) {
	windows := []models.ScopeWindow{{Name: "t1", Start: ts(10), End: ts(40)}}
	got := entryNames(mapOwners(zap.NewNop(), windows, []capturedMock{
		{name: "a", ts: ts(15), owner: "declared"},
		{name: "b", ts: ts(20)}, // no stamp -> correlated into t1
		{name: "c", ts: ts(99), owner: "declared"},
	}))
	require.Equal(t, map[string][]string{
		"declared": {"a", "c"},
		"t1":       {"b"},
	}, got)
}

// A capture that is stamped but falls in no window keeps its stamp: it is not
// silently dropped the way an unstamped orphan is.
func TestMapOwnersKeepsAStampOutsideEveryWindow(t *testing.T) {
	got := entryNames(mapOwners(zap.NewNop(), nil, []capturedMock{
		{name: "a", ts: ts(5), owner: "still-open-at-record"},
	}))
	require.Equal(t, map[string][]string{"still-open-at-record": {"a"}}, got)
}

// When the two disagree the agent wins -- it resolved against live scopes -- but
// the disagreement is logged, once per mock, because each one is a mock replay
// could serve to the wrong test.
func TestMapOwnersWarnsOnDisagreement(t *testing.T) {
	core, logs := observer.New(zapcore.WarnLevel)
	windows := []models.ScopeWindow{{Name: "correlated-to-this", Start: ts(10), End: ts(20)}}
	got := entryNames(mapOwners(zap.New(core), windows, []capturedMock{
		{name: "a", ts: ts(15), owner: "stamped-with-this"},
		{name: "b", ts: ts(15), owner: "correlated-to-this"}, // agrees, no warn
	}))
	require.Equal(t, map[string][]string{
		"stamped-with-this":  {"a"},
		"correlated-to-this": {"b"},
	}, got)
	require.Equal(t, 1, logs.Len(), "exactly one disagreement must be reported")
	entry := logs.All()[0]
	require.Contains(t, entry.Message, "disagree")
	fields := entry.ContextMap()
	require.Equal(t, "a", fields["mock"])
	require.Equal(t, "stamped-with-this", fields["agent_owner"])
	require.Equal(t, "correlated-to-this", fields["correlated_owner"])
}
