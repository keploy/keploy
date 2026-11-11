package proxy

import (
	"slices"
	"testing"

	"github.com/stretchr/testify/require"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

func serveFor(t *testing.T, mm *MockManager, name string, lt models.Lifetime) {
	t.Helper()
	require.True(t, mm.MarkMockAsUsed(models.Mock{Name: name, Kind: models.HTTP, TestModeInfo: models.TestModeInfo{Lifetime: lt}}), name)
}

func consumedByName(states []models.MockState) map[string]models.MockState {
	out := make(map[string]models.MockState, len(states))
	for _, s := range states {
		out[s.Name] = s
	}
	return out
}

// The consumed read says, of each mock, every test window it answered in
// since the last read (models.MockState.AnsweredIn), not the one its first
// serve fell in: a session recording that answers TestB, then TestA, is
// read under both. A serve while two windows are open is both's, and one
// while none is says so (AnsweredOutside). The read forgets them: the next
// read says only where a mock answered since.
func TestTheConsumedReadSaysEveryTestWindowAMockAnsweredIn(t *testing.T) {
	mm := NewMockManager(nil, nil, zap.NewNop())
	t.Cleanup(mm.Close)
	serveFor(t, mm, "mock-30", models.LifetimeSession)
	mm.OpenTestWindow(1)
	serveFor(t, mm, "mock-21", models.LifetimeSession)
	serveFor(t, mm, "mock-21", models.LifetimeSession)
	mm.CloseTestWindow(1)
	mm.OpenTestWindow(2)
	serveFor(t, mm, "mock-10", models.LifetimePerTest)
	mm.OpenTestWindow(3)
	serveFor(t, mm, "mock-21", models.LifetimeSession)
	mm.CloseTestWindow(3)
	mm.CloseTestWindow(2)
	serveFor(t, mm, "mock-21", models.LifetimeSession)

	got := mm.GetConsumedMocks()
	require.Equal(t, []string{"mock-30", "mock-21", "mock-10"}, []string{got[0].Name, got[1].Name, got[2].Name}, "first-serve order kept")
	by := consumedByName(got)
	require.Empty(t, by["mock-30"].AnsweredIn)
	require.True(t, by["mock-30"].AnsweredOutside)
	require.Equal(t, []uint64{1, 2, 3}, by["mock-21"].AnsweredIn)
	require.True(t, by["mock-21"].AnsweredOutside)
	require.Equal(t, []uint64{2}, by["mock-10"].AnsweredIn)
	require.False(t, by["mock-10"].AnsweredOutside)
	for _, s := range got {
		require.Zero(t, s.AnsweredInLost, s.Name)
	}

	mm.OpenTestWindow(4)
	serveFor(t, mm, "mock-21", models.LifetimeSession)
	again := mm.GetConsumedMocks()
	require.Len(t, again, 1)
	require.Equal(t, []uint64{4}, again[0].AnsweredIn, "a read forgets where a mock answered before it")
	require.False(t, again[0].AnsweredOutside)
}

// The window ids a manager keeps between reads are bounded
// (answeredInMax): one past it is counted on the mock that answered in it
// (AnsweredInLost), never kept, so a reader knows it cannot say every test
// that mock answered. A read frees the bound.
func TestTheWindowsAMockAnsweredInAreBoundedAndCountedPastIt(t *testing.T) {
	mm := NewMockManager(nil, nil, zap.NewNop())
	t.Cleanup(mm.Close)
	for seq := uint64(1); seq <= answeredInMax+2; seq++ {
		mm.OpenTestWindow(seq)
	}
	serveFor(t, mm, "mock-1", models.LifetimeSession)
	serveFor(t, mm, "mock-1", models.LifetimeSession)
	serveFor(t, mm, "mock-2", models.LifetimeSession)
	by := consumedByName(mm.GetConsumedMocks())
	require.Len(t, by["mock-1"].AnsweredIn, answeredInMax)
	require.True(t, slices.IsSorted(by["mock-1"].AnsweredIn))
	require.Equal(t, 4, by["mock-1"].AnsweredInLost, "each serve past the bound is counted")
	require.Empty(t, by["mock-2"].AnsweredIn)
	require.Equal(t, answeredInMax+2, by["mock-2"].AnsweredInLost)

	serveFor(t, mm, "mock-2", models.LifetimeSession)
	by = consumedByName(mm.GetConsumedMocks())
	require.Len(t, by["mock-2"].AnsweredIn, answeredInMax, "the read freed the bound")
	require.Equal(t, 2, by["mock-2"].AnsweredInLost)
}

// Every mock manager the proxy makes serves under the test windows the
// agent told the proxy of (agent.TestWindowNoter), one made after the window
// opened (a test worker's, a new session's) as well.
func TestAProxysManagersServeUnderTheWindowsTheAgentHoldsOpen(t *testing.T) {
	p := &Proxy{}
	run := p.shareWindows(NewMockManager(nil, nil, zap.NewNop()))
	t.Cleanup(run.Close)
	p.OpenTestWindow(7)
	worker := p.shareWindows(NewMockManager(nil, nil, zap.NewNop()))
	t.Cleanup(worker.Close)
	serveFor(t, run, "mock-1", models.LifetimeSession)
	serveFor(t, worker, "mock-2", models.LifetimeSession)
	p.CloseTestWindow(7)
	serveFor(t, worker, "mock-3", models.LifetimeSession)
	require.Equal(t, []uint64{7}, run.GetConsumedMocks()[0].AnsweredIn)
	by := consumedByName(worker.GetConsumedMocks())
	require.Equal(t, []uint64{7}, by["mock-2"].AnsweredIn)
	require.Empty(t, by["mock-3"].AnsweredIn)
	require.True(t, by["mock-3"].AnsweredOutside)
}

// A test set's fence drops what the previous set consumed and not read
// (dropPreviousSetConsumes), and where it answered with it: a mock of the
// next set, which reuses the names, answered in none of the previous set's
// windows.
func TestASetsFenceDropsTheWindowsItsMocksAnsweredIn(t *testing.T) {
	mm := NewMockManager(nil, nil, zap.NewNop())
	t.Cleanup(mm.Close)
	mm.OpenTestWindow(1)
	serveFor(t, mm, "mock-1", models.LifetimeSession)
	mm.CloseTestWindow(1)
	mm.dropPreviousSetConsumes()
	mm.OpenTestWindow(2)
	serveFor(t, mm, "mock-1", models.LifetimeSession)
	got := mm.GetConsumedMocks()
	require.Len(t, got, 1)
	require.Equal(t, []uint64{2}, got[0].AnsweredIn)
}
