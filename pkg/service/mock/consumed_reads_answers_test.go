package mock

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/agent/proxy"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// A recording two reads carry keeps where it answered in every read: each
// read hands back where it answered since the read before (the agent forgets
// it as it is read), so the run's one entry for it holds every read's test
// windows, each once at its first answer and ascending by window, its first
// answer outside every window, and every read's count of window ids the agent
// could not keep.
func TestARecordingTwoReadsCarryKeepsWhereItAnsweredInEach(t *testing.T) {
	var c consumedReads
	c.add([]models.MockState{
		{Name: "mock-21", Timestamp: 30, AnsweredIn: []models.WindowAnswer{{Seq: 3, First: 30}, {Seq: 7, First: 70}}, AnsweredInLost: 1},
		{Name: "mock-22", Timestamp: 10, AnsweredOutsideAt: 10},
	})
	c.add([]models.MockState{
		{Name: "mock-21", Timestamp: 75, AnsweredIn: []models.WindowAnswer{{Seq: 5, First: 80}, {Seq: 7, First: 75}}, AnsweredOutsideAt: 90},
		{Name: "mock-22", Timestamp: 85, AnsweredOutsideAt: 85},
	})
	require.Equal(t, []models.MockState{
		{Name: "mock-21", Timestamp: 30, AnsweredIn: []models.WindowAnswer{{Seq: 3, First: 30}, {Seq: 5, First: 80}, {Seq: 7, First: 70}}, AnsweredOutsideAt: 90, AnsweredInLost: 1},
		{Name: "mock-22", Timestamp: 10, AnsweredOutsideAt: 10},
	}, c.list())
}

// A recording that answered the application's start and a test that failed
// before the wait for the series the run owes read what was consumed, and
// then answered a test left open when the test command exited, is under both
// tests' lines and listed at all three places. Each read hands back where it
// answered since the read before, so the run's end's read alone says only
// the test left open: a mapping edit that moved it would be judged as if it
// never answered the failed test or before it.
func TestARecordingTwoReadsCarryIsUnderEveryTestItAnsweredIn(t *testing.T) {
	mm := proxy.NewMockManager(nil, nil, zap.NewNop())
	t.Cleanup(mm.Close)
	serve := func(name string, lt models.Lifetime) {
		t.Helper()
		require.True(t, mm.MarkMockAsUsed(models.Mock{Name: name, Kind: models.HTTP, TestModeInfo: models.TestModeInfo{Lifetime: lt}}), name)
	}
	tick := func() time.Time {
		time.Sleep(2 * time.Millisecond)
		now := time.Now()
		time.Sleep(2 * time.Millisecond)
		return now
	}
	serve("mock-21", models.LifetimeSession) // the application's start
	aStart := tick()
	mm.OpenTestWindow(1) // pkg.TestA
	serve("mock-21", models.LifetimeSession)
	serve("mock-10", models.LifetimePerTest)
	mm.CloseTestWindow(1)
	aEnd := tick()
	mm.OpenTestWindow(2) // pkg.TestB, open when the test command exited
	m := &mockService{logger: zap.NewNop(), config: &config.Config{}, instrumentation: drainInstr{mm: mm}}
	m.readOwedSeries(context.Background(), nil, nil) // the wait's read
	read := m.consumed.list()
	require.Len(t, read, 2, "%+v", read)
	early := read[0]
	require.Equal(t, "mock-21", early.Name)
	require.Len(t, early.AnsweredIn, 1, "%+v", early)
	require.NotZero(t, early.AnsweredOutsideAt, "%+v", early)
	tick()
	serve("mock-21", models.LifetimeSession) // the application runs on
	mm.CloseTestWindow(2)
	bEnd := tick()
	windows := []models.ScopeWindow{
		{Name: "pkg.TestA", Seq: 1, Start: aStart, End: aEnd, Outcome: models.ScopeOutcomeFailed},
		{Name: "pkg.TestB", Seq: 2, Start: aEnd, End: bEnd, Outcome: models.ScopeOutcomeFailed},
	}
	counts := m.reportOutcome(context.Background(), 2, replayDetail{set: "s", windows: windows}, func() bool { return false })
	require.Len(t, counts.tests, 2, "%+v", counts.tests)
	byName := map[string]TestReceipt{}
	for _, tr := range counts.tests {
		byName[tr.Name] = tr
	}
	require.Equal(t, []string{"mock-21", "mock-10"}, byName["pkg.TestA"].AnsweredBy, "mock-21 answered pkg.TestA, read before the run's end's read")
	require.Equal(t, 2, byName["pkg.TestA"].Consumed)
	require.Equal(t, []string{"mock-21"}, byName["pkg.TestB"].AnsweredBy)
	require.Len(t, counts.answered, 2, "%+v", counts.answered)
	r := counts.answered[0]
	require.Equal(t, "mock-21", r.Mock)
	require.Len(t, r.In, 3, "%+v", r)
	require.Equal(t, []AnswerPlace{
		{Test: "", First: time.Unix(0, early.AnsweredOutsideAt).UTC()},
		{Test: "pkg.TestA", First: time.Unix(0, early.AnsweredIn[0].First).UTC()},
		{Test: "pkg.TestB", First: r.In[2].First},
	}, r.In, "the application's start, then pkg.TestA, from the wait's read; pkg.TestB from the run's end's")
	require.True(t, r.In[2].First.After(aEnd), "%+v", r)
}
