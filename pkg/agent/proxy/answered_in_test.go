package proxy

import (
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

func serveFor(t *testing.T, mm *MockManager, name string, lt models.Lifetime) {
	t.Helper()
	require.True(t, mm.MarkMockAsUsed(models.Mock{Name: name, Kind: models.HTTP, TestModeInfo: models.TestModeInfo{Lifetime: lt}}), name)
}

// seqsOf is the window ids of answers, in their order.
func seqsOf(answers []models.WindowAnswer) []uint64 {
	var out []uint64
	for _, a := range answers {
		out = append(out, a.Seq)
	}
	return out
}

// firstIn is when answers say the mock first answered in window seq; 0 when
// they do not name it.
func firstIn(answers []models.WindowAnswer, seq uint64) int64 {
	for _, a := range answers {
		if a.Seq == seq {
			return a.First
		}
	}
	return 0
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
// while none is says so (AnsweredOutsideAt). Each place keeps when the mock
// first answered there, never a later answer's time. The read forgets them:
// the next read says only where a mock answered since.
func TestTheConsumedReadSaysEveryTestWindowAMockAnsweredIn(t *testing.T) {
	mm := NewMockManager(nil, nil, zap.NewNop())
	t.Cleanup(mm.Close)
	// step is a time strictly after every serve before it, and before
	// every serve after it.
	step := func() int64 {
		time.Sleep(2 * time.Millisecond)
		at := time.Now().UnixNano()
		time.Sleep(2 * time.Millisecond)
		return at
	}
	t0 := step()
	serveFor(t, mm, "mock-30", models.LifetimeSession)
	t1 := step()
	mm.OpenTestWindow(1)
	serveFor(t, mm, "mock-21", models.LifetimeSession)
	t2 := step()
	serveFor(t, mm, "mock-21", models.LifetimeSession)
	mm.CloseTestWindow(1)
	mm.OpenTestWindow(2)
	serveFor(t, mm, "mock-10", models.LifetimePerTest)
	t3 := step()
	mm.OpenTestWindow(3)
	serveFor(t, mm, "mock-21", models.LifetimeSession)
	mm.CloseTestWindow(3)
	mm.CloseTestWindow(2)
	t4 := step()
	serveFor(t, mm, "mock-21", models.LifetimeSession)
	t5 := step()
	serveFor(t, mm, "mock-21", models.LifetimeSession)

	got := mm.GetConsumedMocks()
	require.Equal(t, []string{"mock-30", "mock-21", "mock-10"}, []string{got[0].Name, got[1].Name, got[2].Name}, "first-serve order kept")
	by := consumedByName(got)
	require.Empty(t, by["mock-30"].AnsweredIn)
	require.True(t, by["mock-30"].AnsweredOutsideAt > t0 && by["mock-30"].AnsweredOutsideAt < t1, "answered outside every window, at its serve")
	require.Equal(t, []uint64{1, 2, 3}, seqsOf(by["mock-21"].AnsweredIn))
	in1 := firstIn(by["mock-21"].AnsweredIn, 1)
	require.True(t, in1 > t1 && in1 < t2, "window 1 keeps its first answer, not the second")
	require.Equal(t, firstIn(by["mock-21"].AnsweredIn, 2), firstIn(by["mock-21"].AnsweredIn, 3), "one serve under two open windows is both's, at one time")
	require.True(t, firstIn(by["mock-21"].AnsweredIn, 3) > t3 && firstIn(by["mock-21"].AnsweredIn, 3) < t4)
	require.True(t, by["mock-21"].AnsweredOutsideAt > t4 && by["mock-21"].AnsweredOutsideAt < t5, "outside keeps its first answer, not the later one")
	require.Equal(t, []uint64{2}, seqsOf(by["mock-10"].AnsweredIn))
	require.True(t, firstIn(by["mock-10"].AnsweredIn, 2) > t2 && firstIn(by["mock-10"].AnsweredIn, 2) < t3)
	require.Zero(t, by["mock-10"].AnsweredOutsideAt)
	for _, s := range got {
		require.Zero(t, s.AnsweredInLost, s.Name)
	}

	mm.OpenTestWindow(4)
	t6 := step()
	serveFor(t, mm, "mock-21", models.LifetimeSession)
	again := mm.GetConsumedMocks()
	require.Len(t, again, 1)
	require.Equal(t, []uint64{4}, seqsOf(again[0].AnsweredIn), "a read forgets where a mock answered before it")
	require.True(t, firstIn(again[0].AnsweredIn, 4) > t6, "and when")
	require.Zero(t, again[0].AnsweredOutsideAt)
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
	require.True(t, slices.IsSorted(seqsOf(by["mock-1"].AnsweredIn)))
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
	require.Equal(t, []uint64{7}, seqsOf(run.GetConsumedMocks()[0].AnsweredIn))
	by := consumedByName(worker.GetConsumedMocks())
	require.Equal(t, []uint64{7}, seqsOf(by["mock-2"].AnsweredIn))
	require.Empty(t, by["mock-3"].AnsweredIn)
	require.NotZero(t, by["mock-3"].AnsweredOutsideAt)
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
	require.Equal(t, []uint64{2}, seqsOf(got[0].AnsweredIn))
}
