package proxy

import (
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

// The shape of a sweep that lists marks and unmarks each one: the read
// series marks (r1, r2) lists them, and its last answer (r2) caused w, the
// unmark (models.MockEntry.CausedBy). x is another write of the worker
// scope, and poll (q1) another read series.
const (
	marksURL  = "/marks"
	unmarkURL = "/unmark?app=a1"
	otherURL  = "/other"
	pollURL   = "/poll"
)

func againSweep(t *testing.T) (*MockManager, []*models.Mock) {
	t.Helper()
	return againSweepLogged(t, zap.NewNop(), "w")
}

// againSweepLogged is againSweep logging to lg, its marks gate naming the
// writes closers.
func againSweepLogged(t *testing.T, lg *zap.Logger, closers ...string) (*MockManager, []*models.Mock) {
	t.Helper()
	mks := []*models.Mock{
		httpCall("r1", time.Second, marksURL),
		httpCall("r2", 2*time.Second, marksURL),
		httpCall("w", 3*time.Second, unmarkURL),
		httpCall("x", 4*time.Second, otherURL),
		httpCall("q1", 5*time.Second, pollURL),
	}
	mm := stagedManagerLogged(t, lg, nil, mks)
	mm.ServeExactWorker([]string{"r1", "r2", "w", "x", "q1"})
	mm.SetPoolGates(map[string]models.SeriesGate{"marks/-/1": {AgainClosedBy: closers}})
	mm.SetPoolSeries(map[string][]string{"marks/-/1": {"r1", "r2"}, "unmark/T/1": {"w"}, "other/-/1": {"x"}, "poll/-/1": {"q1"}})
	mm.ServeExactTest("TestU", nil)
	return mm, mks
}

func calls(t *testing.T, mm *MockManager, url string, n int) []string {
	t.Helper()
	var got []string
	for i := 0; i < n; i++ {
		got = append(got, callURL(t, mm, url, false))
	}
	return got
}

// Once the run served w, the marks series' last answer, which still lists
// what w changed, answers no repeat: the next list call misses. Before w,
// it answers the repeats; poll's repeats go on after w.
func TestASeriesIsNotAnsweredAgainAfterAWriteItsLastAnswerCaused(t *testing.T) {
	mm, _ := againSweep(t)
	if got, want := calls(t, mm, marksURL, 3), []string{"r1", "r2", "r2"}; !slices.Equal(got, want) {
		t.Fatalf("before the write the list was answered %v, want %v", got, want)
	}
	if got := callURL(t, mm, unmarkURL, false); got != "w" {
		t.Fatalf("the unmark was answered %q, want w", got)
	}
	if got := calls(t, mm, marksURL, 2); !slices.Equal(got, []string{"a miss", "a miss"}) {
		t.Fatalf("after the write the list was answered %v, want two misses", got)
	}
	if got, want := calls(t, mm, pollURL, 3), []string{"q1", "q1", "q1"}; !slices.Equal(got, want) {
		t.Fatalf("another series was answered %v after the write, want %v", got, want)
	}
}

// With no such write served, the series' last answers every repeat
// (served, not required); a write the gate does not name ends nothing.
func TestASeriesWithNoWriteItsAnswerCausedIsAnsweredAgain(t *testing.T) {
	mm, _ := againSweep(t)
	if got := callURL(t, mm, otherURL, false); got != "x" {
		t.Fatalf("the other write was answered %q, want x", got)
	}
	if got, want := calls(t, mm, marksURL, 4), []string{"r1", "r2", "r2", "r2"}; !slices.Equal(got, want) {
		t.Fatalf("the list was answered %v, want %v", got, want)
	}
}

// The series' recordings stay the set's background recordings once its
// repeats ended, so a parser marks the list call that misses Background
// (counted against no test). A new staging answers the repeats again
// until the write is served in it.
func TestAnEndedSeriesMissIsABackgroundCallAndANewStagingAnswersAgain(t *testing.T) {
	mm, mks := againSweep(t)
	mm.SeedBackgroundRecordings([]models.BackgroundRecording{{Mock: mks[0]}, {Mock: mks[1]}})
	calls(t, mm, marksURL, 2)
	callURL(t, mm, unmarkURL, false)
	if got := callURL(t, mm, marksURL, false); got != "a miss" {
		t.Fatalf("after the write the list was answered %q, want a miss", got)
	}
	var names []string
	for _, r := range mm.BackgroundRecordings(models.HTTP) {
		names = append(names, r.Mock.Name)
	}
	require.Equal(t, []string{"r1", "r2"}, names, "the ended series' recordings are still the set's background recordings")

	mm.stagingEpoch.Add(1)
	if got, want := calls(t, mm, marksURL, 3), []string{"r1", "r2", "r2"}; !slices.Equal(got, want) {
		t.Fatalf("a new staging answered the list %v, want %v", got, want)
	}
}

// A served recording with no series installed ends nothing and does not
// panic.
func TestAServedRecordingWithNoSeriesEndsNoRepeats(t *testing.T) {
	a := httpCall("a", time.Second, marksURL)
	mm := stagedManager(t, []*models.Mock{a}, nil)
	mm.ServeExactTest("TestT", []*models.Mock{a})
	var got string
	require.NotPanics(t, func() { got = callURL(t, mm, marksURL, false) })
	require.Equal(t, "a", got)
}

// The end is said once per series and staging, at Info, naming the series
// and the write that ended it: a second write the gate names ends nothing
// more and says nothing more.
func TestAnEndedSeriesIsSaidOncePerStaging(t *testing.T) {
	core, logs := observer.New(zap.InfoLevel)
	mm, _ := againSweepLogged(t, zap.New(core), "w", "x")
	ended := func() []observer.LoggedEntry {
		return logs.FilterMessageSnippet("ends that series' repeats").All()
	}
	if got := callURL(t, mm, unmarkURL, false); got != "w" {
		t.Fatalf("the unmark was answered %q, want w", got)
	}
	if got := callURL(t, mm, otherURL, false); got != "x" {
		t.Fatalf("the other write was answered %q, want x", got)
	}
	got := ended()
	require.Len(t, got, 1, "the end of the marks series' repeats is said once in the staging")
	require.Equal(t, map[string]any{"series": "marks/-/1", "write": "w"}, got[0].ContextMap())
}
