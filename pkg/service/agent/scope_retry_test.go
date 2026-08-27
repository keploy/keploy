package agent

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	"go.keploy.io/server/v3/config"
	coreAgent "go.keploy.io/server/v3/pkg/agent"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// retryProxyStub is a coreAgent.Proxy that implements only the three methods
// BeginScope's replay branch reaches. The embedded nil interface satisfies the
// rest of the contract at compile time; calling any of it would panic, which is
// the point — the test fails loudly if this path grows a new proxy dependency.
type retryProxyStub struct {
	coreAgent.Proxy
	consumed  map[string]models.MockState
	resetWith []string
	// calls records the order the agent touched the proxy in. The retry reset
	// MUST land before the totals read, or the re-stage filters out the very
	// mocks it just un-consumed.
	calls []string
}

func (s *retryProxyStub) TotalConsumedMocks(context.Context) (map[string]models.MockState, error) {
	s.calls = append(s.calls, "total")
	out := make(map[string]models.MockState, len(s.consumed))
	for k, v := range s.consumed {
		out[k] = v
	}
	return out, nil
}

func (s *retryProxyStub) ResetConsumedMocks(_ context.Context, names []string) (int, error) {
	s.calls = append(s.calls, "reset")
	s.resetWith = append([]string(nil), names...)
	removed := 0
	for _, n := range names {
		if _, ok := s.consumed[n]; ok {
			delete(s.consumed, n)
			removed++
		}
	}
	return removed, nil
}

func (s *retryProxyStub) SetMocks(context.Context, []*models.Mock, []*models.Mock) error {
	s.calls = append(s.calls, "setmocks")
	return nil
}

func (s *retryProxyStub) SetWorkerScope(uint32, []string) {
	s.calls = append(s.calls, "setworkerscope")
}

// newReplayAgent builds an agent on the MODE_TEST (replay) branch with a
// mappings.yaml-shaped scope table: "retried" owns two mocks, "other" owns one.
func newReplayAgent(t *testing.T, consumed ...string) (*Agent, *retryProxyStub) {
	t.Helper()
	ledger := make(map[string]models.MockState, len(consumed))
	for _, n := range consumed {
		ledger[n] = models.MockState{Name: n, Usage: models.Deleted}
	}
	stub := &retryProxyStub{consumed: ledger}
	a := &Agent{
		logger: zap.NewNop(),
		Proxy:  stub,
		config: &config.Config{Agent: config.Agent{SetupOptions: models.SetupOptions{Mode: models.MODE_TEST}}},
		scopeTable: map[string][]string{
			"retried": {"mock-0", "mock-1"},
			"other":   {"mock-2"},
		},
	}
	a.clientMocks.Store(uint64(0), &ClientMockStorage{})
	return a, stub
}

// A retry (attempt > 0) must un-consume ONLY the retried scope's own mocks, and
// must do it before the re-stage reads the ledger.
func TestBeginScope_RetryResetsOnlyThisScope(t *testing.T) {
	a, stub := newReplayAgent(t, "mock-0", "mock-1", "mock-2")

	ack, err := a.BeginScope(context.Background(), "retried", 0, 1)
	require.NoError(t, err)

	require.True(t, ack.RetryReset, "attempt>0 on a mapped scope must report a retry reset")
	require.Equal(t, 2, ack.RestoredMocks)
	require.Equal(t, 1, ack.Attempt, "the ack echoes the attempt the runner sent")
	require.Equal(t, models.ScopeReasonPoolRestricted, ack.Reason)

	require.Equal(t, []string{"mock-0", "mock-1"}, stub.resetWith,
		"the reset must be handed exactly this scope's mapped mock names")
	// THE B17 REGRESSION GUARD at the unit level: another test's consumption
	// must survive a retry of this one.
	require.Contains(t, stub.consumed, "mock-2", "another scope's consumed mock must NOT be re-armed")
	require.NotContains(t, stub.consumed, "mock-0")

	require.Equal(t, []string{"reset", "total", "setmocks"}, stub.calls,
		"the reset must precede the totals read, else the re-stage filters out what it just restored")
}

// attempt 0 (and an absent attempt, which decodes to 0) must behave exactly as
// before: no reset, and the ack carries none of the new fields.
func TestBeginScope_FirstAttemptDoesNotReset(t *testing.T) {
	a, stub := newReplayAgent(t, "mock-0", "mock-1", "mock-2")

	ack, err := a.BeginScope(context.Background(), "retried", 0, 0)
	require.NoError(t, err)

	require.False(t, ack.RetryReset)
	require.Zero(t, ack.RestoredMocks)
	require.Zero(t, ack.Attempt)
	require.Nil(t, stub.resetWith, "attempt 0 must never call the resetter")
	require.Equal(t, []string{"total", "setmocks"}, stub.calls)
	require.Len(t, stub.consumed, 3, "nothing is un-consumed on a first run")
}

// A retry of a scope with no mappings.yaml entry has nothing of its own to
// reset. It must decline rather than broaden the reset — resetting "everything"
// here is the resurrection bug wearing a retry costume.
func TestBeginScope_RetryOfUnmappedScopeResetsNothing(t *testing.T) {
	a, stub := newReplayAgent(t, "mock-0", "mock-1", "mock-2")

	ack, err := a.BeginScope(context.Background(), "renamed-or-never-recorded", 0, 1)
	require.NoError(t, err)

	require.Equal(t, models.ScopeReasonUnmappedScope, ack.Reason)
	require.Equal(t, 1, ack.Attempt, "the ack still echoes the attempt, so the runner sees it was understood")
	require.False(t, ack.RetryReset, "nothing was reset")
	require.Zero(t, ack.RestoredMocks)
	require.Nil(t, stub.resetWith)
	require.Empty(t, stub.calls, "an unmapped scope must not touch the proxy at all")
	require.Len(t, stub.consumed, 3)
}

// The per-worker (pid>0) path never re-stages the pool, so un-consuming the
// ledger there would restore nothing while claiming it had. It must report
// retry_reset false rather than lie.
func TestBeginScope_RetryOnWorkerScopedPathReportsNoReset(t *testing.T) {
	a, stub := newReplayAgent(t, "mock-0", "mock-1", "mock-2")

	ack, err := a.BeginScope(context.Background(), "retried", 4242, 1)
	require.NoError(t, err)

	require.Equal(t, models.ScopeReasonWorkerScoped, ack.Reason)
	require.Equal(t, []string{"setworkerscope"}, stub.calls, "the worker path narrows a view; it never re-stages")
	require.True(t, ack.Scoped)
	require.Equal(t, 1, ack.Attempt)
	require.False(t, ack.RetryReset)
	require.Nil(t, stub.resetWith)
	require.Len(t, stub.consumed, 3)
}
