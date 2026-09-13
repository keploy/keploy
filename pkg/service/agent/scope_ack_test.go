package agent

// /agent/scope/begin has several paths that leave the WHOLE suite's mock pool
// armed instead of narrowing to the calling test. Before ScopeAck they were
// indistinguishable on the wire from a correctly-isolated test, so a renamed or
// moved test silently degraded to whole-suite replay. These tests pin one
// distinct, stable reason token per path.

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	"go.keploy.io/server/v3/config"
	coreAgent "go.keploy.io/server/v3/pkg/agent"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// testModeAgent is a MODE_TEST agent with no proxy and no resident mocks: every
// case here returns before UpdateMockParams, which is exactly what makes them
// the silent-fallback paths.
func testModeAgent(t *testing.T, table map[string][]string) *Agent {
	t.Helper()
	a := &Agent{logger: zap.NewNop()}
	a.config = &config.Config{}
	a.config.Agent.Mode = models.MODE_TEST
	a.scopeTable = table
	return a
}

func TestBeginScopeAckReplayNotScoped(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name   string
		table  map[string][]string
		scope  string
		reason string
		why    string
	}{
		{
			name: "no mapping table", table: nil, scope: "alpha",
			reason: models.ScopeReasonNoMappingTable,
			why:    "replay ran without a mappings.yaml, so no scope table was ever installed",
		},
		{
			name: "name absent from table", table: map[string][]string{"beta": {"m1"}}, scope: "alpha",
			reason: models.ScopeReasonUnmappedScope,
			why:    "the test was renamed or moved since the recording",
		},
		{
			name: "name mapped to zero mocks", table: map[string][]string{"alpha": {}}, scope: "alpha",
			reason: models.ScopeReasonEmptyMapping,
			why:    "record captured no calls for this test",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := testModeAgent(t, tc.table)
			ack, err := a.BeginScope(ctx, tc.scope, 0)
			require.NoError(t, err)
			require.False(t, ack.Scoped, "the whole pool is still armed — %s", tc.why)
			require.Equal(t, tc.reason, ack.Reason)
			require.Zero(t, ack.Mocks)
		})
	}
}

// The two scoped paths must be distinguishable from each other, because they
// isolate differently: pid==0 re-stages one global pool, pid>0 narrows only
// this worker's view.
func TestBeginScopeAckReplayScoped(t *testing.T) {
	ctx := context.Background()

	t.Run("worker scoped", func(t *testing.T) {
		a, _ := replayAgent(t, []string{"m1", "m2"}, map[string][]string{"alpha": {"m1", "m2"}})
		ack, err := a.BeginScope(ctx, "alpha", 7)
		require.NoError(t, err)
		require.True(t, ack.Scoped)
		require.Equal(t, models.ScopeReasonWorkerScoped, ack.Reason)
		require.Equal(t, 2, ack.Mocks)
	})

	t.Run("global pool restricted", func(t *testing.T) {
		a, _ := replayAgent(t, []string{"m1", "m2"}, map[string][]string{"alpha": {"m1"}})
		ack, err := a.BeginScope(ctx, "alpha", 0)
		require.NoError(t, err)
		require.True(t, ack.Scoped)
		require.Equal(t, models.ScopeReasonPoolRestricted, ack.Reason)
		require.Equal(t, 1, ack.Mocks, "the count a runner asserts its call volume against")
	})
}

// Record mode serves nothing, so Scoped is always false; the reason still has
// to separate a clean window from a double begin, which silently discards the
// earlier window start and orphans everything captured before it.
func TestBeginScopeAckRecord(t *testing.T) {
	ctx := context.Background()
	a := newRecordAgent()

	ack, err := a.BeginScope(ctx, "alpha", 0)
	require.NoError(t, err)
	require.False(t, ack.Scoped)
	require.Equal(t, models.ScopeReasonRecordWindowOpened, ack.Reason)

	ack, err = a.BeginScope(ctx, "alpha", 0) // no EndScope in between
	require.NoError(t, err)
	require.Equal(t, models.ScopeReasonRecordAlreadyOpen, ack.Reason)
}

func TestBeginScopeAckEmptyName(t *testing.T) {
	a := newRecordAgent()
	ack, err := a.BeginScope(context.Background(), "", 0)
	require.NoError(t, err)
	require.False(t, ack.Scoped)
	require.Equal(t, models.ScopeReasonEmptyName, ack.Reason)
	require.Empty(t, windowNames(a), "an empty name must not open a record window")
}

// fakeScopeProxy is a coreAgent.Proxy stand-in that records what the agent
// stages. The embedded nil interface supplies the rest of the method set; only
// the methods BeginScope's replay path actually calls are implemented, so any
// unexpected call panics loudly instead of passing silently.
//
// It deliberately DOES implement coreAgent.ConsumedStateReader
// (GetPersistentConsumed) — that is the upstream read side, and the point of
// TestBeginScopeIgnoresPersistentConsumed is that the mock-replay path never
// asks for it.
type fakeScopeProxy struct {
	coreAgent.Proxy
	staged    [][]string
	workers   map[uint32][]string
	consumed  map[string]models.MockState
	universes [][]string
}

func newFakeScopeProxy() *fakeScopeProxy {
	return &fakeScopeProxy{workers: map[uint32][]string{}, consumed: map[string]models.MockState{}}
}

func (f *fakeScopeProxy) SetMocks(_ context.Context, filtered, _ []*models.Mock) error {
	names := make([]string, 0, len(filtered))
	for _, m := range filtered {
		names = append(names, m.Name)
	}
	f.staged = append(f.staged, names)
	return nil
}

func (f *fakeScopeProxy) SetWorkerScope(pid uint32, names []string) { f.workers[pid] = names }
func (f *fakeScopeProxy) ClearWorkerScope(pid uint32)               { delete(f.workers, pid) }
func (f *fakeScopeProxy) SetMappedUniverse(names []string)          { f.universes = append(f.universes, names) }

func (f *fakeScopeProxy) GetPersistentConsumed() map[string]models.MockState {
	out := make(map[string]models.MockState, len(f.consumed))
	for k, v := range f.consumed {
		out[k] = v
	}
	return out
}

// replayAgent builds an agent in the shape `keploy mock replay` leaves it in:
// MODE_TEST, the whole set resident under client 0, and a scope table installed
// from mappings.yaml.
func replayAgent(t *testing.T, mockNames []string, table map[string][]string) (*Agent, *fakeScopeProxy) {
	t.Helper()
	p := newFakeScopeProxy()
	a := &Agent{logger: zap.NewNop(), Proxy: p}
	a.config = &config.Config{}
	a.config.Agent.Mode = models.MODE_TEST

	resident := make([]*models.Mock, 0, len(mockNames))
	for _, n := range mockNames {
		resident = append(resident, &models.Mock{Name: n, Kind: models.HTTP})
	}
	a.clientMocks.Store(uint64(0), &ClientMockStorage{filtered: resident})

	require.NoError(t, a.SetScopeTable(context.Background(), table))
	return a, p
}
