//go:build linux

package agent

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.keploy.io/server/v3/pkg/models"
)

// The dress-rehearsal janitor shape on the keploy mock path: the
// application's janitor sweeps on its own schedule (GET /sweep), and record
// listed its one recorded sweep in the set's worker: scope. At this replay
// the sweep's first call landed in TestA's window, and the worker recording
// answered it, which spends it. The janitor swept again in TestB's window.
// TestB never recorded a sweep, so nothing answers the call, and its miss
// names the spent worker recording (models.RepeatNote.Background).
//
// That call is the background's, not TestB's: keploy charges it to no test,
// as it charges a call made outside every test, and TestB keeps the outcome
// its test command gave it. The run's misses list the call charged to no
// test, with the window it landed in (UnmatchedCall.WindowOf), so a reader
// sees it. Before, TestB failed on a call it never made at record; the
// confirming round set it aside, and the next sweep failed the next test.
func TestAMissRepeatingASpentBackgroundRecordingFailsNoTest(t *testing.T) {
	a, _ := wireAgent(t, models.MODE_TEST)
	p := newServingProxy()
	a.Proxy = p
	ctx := context.Background()
	require.NoError(t, a.StoreMocks(ctx, nil, []*models.Mock{
		pingMock("mock-1", "/sweep", "swept", t0),
		pingMock("mock-2", "/a", "a", t0.Add(time.Second)),
		pingMock("mock-3", "/b", "b", t0.Add(2*time.Second)),
	}))
	require.NoError(t, a.SetScopeTable(ctx, map[string][]string{"o::TestA": {"mock-2"}, "o::TestB": {"mock-3"}}))
	require.NoError(t, a.SetScopeWorker(ctx, map[string][]string{"o": {"mock-1"}}))
	require.NoError(t, a.SetScopePool(ctx, map[string][]string{"o": {"mock-1"}}))
	claimSelf(t, a)
	got, e := runTest(t, a, p, "TestA", "/a", "/sweep")
	require.Equal(t, []string{"200 a", "200 swept"}, got, "the worker recording answers the sweep in TestA's window")
	require.NotEqual(t, models.ScopeActionFail, e.Action, e.Reason)
	got, e = runTest(t, a, p, "TestB", "/b", "/sweep")
	t.Logf("TestB got %v action=%q reason=%q", got, e.Action, e.Reason)
	require.Equal(t, "200 b", got[0])
	require.True(t, strings.HasPrefix(got[1], "502"), "a spent worker recording answered a second sweep: %s", got[1])
	require.NotEqual(t, models.ScopeActionFail, e.Action, "the background's repeated sweep failed TestB: %s", e.Reason)
	ws, err := a.GetScopeWindows(ctx)
	require.NoError(t, err)
	for _, w := range ws {
		if w.Name == "o::TestB" && w.Evidence != nil {
			require.Empty(t, w.Evidence.Misses, "TestB's evidence lists the background's sweep")
		}
	}
	var sweeps []models.UnmatchedCall
	for _, m := range a.takeChargedMisses() {
		if m.ActualSummary == "GET /sweep" {
			sweeps = append(sweeps, m)
		}
	}
	require.Len(t, sweeps, 1, "the run's misses do not list the repeated sweep once")
	m := sweeps[0]
	require.Empty(t, m.ChargedTo, "the repeated sweep is charged to %s", m.ChargedTo)
	require.Equal(t, "o::TestB", m.WindowOf, "the run's misses do not say which test's window the sweep landed in")
	require.NotNil(t, m.Repeat)
	require.Equal(t, "mock-1", m.Repeat.Mock)
	require.Equal(t, models.RepeatBackgroundWorker, m.Repeat.Background)
}

// A test whose own recordings hold the call is judged on it, whatever
// background recording answered an identical call before: TestB recorded
// one sweep of its own, and sweeps twice. Its second sweep is one call more
// than it recorded (an extra call, with no repeat note), so it stays TestB's
// miss and fails it, after the worker recording answered TestA's sweep.
func TestAnExtraCallOfATestsOwnStaysItsMissAfterABackgroundSpend(t *testing.T) {
	a, _ := wireAgent(t, models.MODE_TEST)
	p := newServingProxy()
	a.Proxy = p
	ctx := context.Background()
	require.NoError(t, a.StoreMocks(ctx, nil, []*models.Mock{
		pingMock("mock-1", "/sweep", "swept", t0),
		pingMock("mock-2", "/a", "a", t0.Add(time.Second)),
		pingMock("mock-3", "/sweep", "own sweep", t0.Add(2*time.Second)),
	}))
	require.NoError(t, a.SetScopeTable(ctx, map[string][]string{"o::TestA": {"mock-2"}, "o::TestB": {"mock-3"}}))
	require.NoError(t, a.SetScopeWorker(ctx, map[string][]string{"o": {"mock-1"}}))
	require.NoError(t, a.SetScopePool(ctx, map[string][]string{"o": {"mock-1"}}))
	claimSelf(t, a)
	got, e := runTest(t, a, p, "TestA", "/a", "/sweep")
	require.Equal(t, []string{"200 a", "200 swept"}, got)
	require.NotEqual(t, models.ScopeActionFail, e.Action, e.Reason)
	got, e = runTest(t, a, p, "TestB", "/sweep", "/sweep")
	t.Logf("TestB got %v action=%q reason=%q", got, e.Action, e.Reason)
	require.Equal(t, "200 own sweep", got[0])
	require.True(t, strings.HasPrefix(got[1], "502"), "TestB's second sweep was answered: %s", got[1])
	require.Equal(t, models.ScopeActionFail, e.Action, "the call past TestB's own recording must fail it")
	require.Contains(t, e.Reason, "was called 2 times, recorded 1")
}

// Which misses read at a test's end repeat a spent recording of the set's
// background (backgroundRepeat), and so are charged to no test, with the
// window they were made in: only a miss whose repeat note names a
// background scope, that is no extra call of the test's own recorded call,
// and no recording another test made. A note naming an earlier test's own
// recording is that test's cache moving, which judging reads; it stays the
// ending test's miss, as an extra call and a recording made elsewhere do. A
// repeat made before the ending test began is charged to no test, and names
// no window: which test's was open then is not known here. One filed as the
// test ended, made after it began, names the test's window.
func TestOnlyARepeatOfASpentBackgroundRecordingIsChargedToNoTest(t *testing.T) {
	began := time.Now()
	worker := &models.RepeatNote{Mock: "mock-bg", Background: models.RepeatBackgroundWorker}
	pool := &models.RepeatNote{Mock: "mock-pool", Background: models.RepeatBackgroundPool}
	own := &models.RepeatNote{Mock: "mock-own", Test: "o::TestZ"}
	misses := []models.UnmatchedCall{
		{Protocol: "HTTP", ActualSummary: "GET /sweep", Destination: "worker:80", Kind: models.MissInTest, At: began.Add(time.Second), Repeat: worker},
		{Protocol: "HTTP", ActualSummary: "GET /sweep", Destination: "pool:80", Kind: models.MissInTest, At: began.Add(time.Second), Repeat: pool},
		{Protocol: "HTTP", ActualSummary: "GET /sweep", Destination: "early:80", Kind: models.MissInTest, At: began.Add(-time.Second), Repeat: worker},
		{Protocol: "HTTP", ActualSummary: "GET /sweep", Destination: "untimed:80", Kind: models.MissInTest, Repeat: worker},
		{Protocol: "HTTP", ActualSummary: "GET /sweep", Destination: "ending:80", Kind: models.MissOutsideTest, At: began.Add(time.Second), Repeat: worker},
		{Protocol: "HTTP", ActualSummary: "GET /token", Destination: "own:80", Kind: models.MissInTest, At: began.Add(time.Second), Repeat: own},
		{Protocol: "HTTP", ActualSummary: "GET /sweep", Destination: "extra:80", Kind: models.MissExtraCall, Recorded: 1, At: began.Add(time.Second), Repeat: worker},
		{Protocol: "HTTP", ActualSummary: "GET /sweep", Destination: "elsewhere:80", Kind: models.MissInTest, NotOwned: true, ClosestMock: "mock-y", At: began.Add(time.Second), Repeat: worker},
		{Protocol: "HTTP", ActualSummary: "GET /plain", Destination: "plain:80", Kind: models.MissInTest, At: began.Add(time.Second)},
	}
	for _, c := range []struct {
		name    string
		beganAt time.Time
		windows map[string]string // uncharged destination -> its WindowOf
	}{
		{"begin known", began, map[string]string{"worker:80": "o::TestA", "pool:80": "o::TestA", "early:80": "", "untimed:80": "", "ending:80": "o::TestA"}},
		{"begin not known", time.Time{}, map[string]string{"worker:80": "", "pool:80": "", "early:80": "", "untimed:80": "", "ending:80": ""}},
	} {
		t.Run(c.name, func(t *testing.T) {
			a, p := wireAgent(t, models.MODE_TEST)
			p.misses = append([]models.UnmatchedCall(nil), misses...)
			ev, err := a.testEvidence(context.Background(), "o::TestA", false, c.beganAt)
			require.NoError(t, err)
			var listed []string
			for _, m := range ev.Misses {
				listed = append(listed, m.Destination)
			}
			uncharged := map[string]string{}
			for _, m := range a.takeChargedMisses() {
				if m.ChargedTo == "" {
					uncharged[m.Destination] = m.WindowOf
					continue
				}
				require.Equal(t, "o::TestA", m.ChargedTo)
				require.Empty(t, m.WindowOf, "%s, charged to the test, names a window", m.Destination)
			}
			require.Equal(t, c.windows, uncharged, "the misses charged to no test, and the window each names")
			require.Equal(t, []string{"own:80", "extra:80", "elsewhere:80", "plain:80"}, listed, "the misses the ending test's evidence lists")
		})
	}
}
