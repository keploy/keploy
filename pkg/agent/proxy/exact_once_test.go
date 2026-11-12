package proxy

import (
	"slices"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
)

// The once series' requests: the two reads and the write of a credential's
// last-use touch, which the application makes once per process for
// whichever test authenticates first.
const (
	onceByID  = "/v1/secret/data/keploy/personal-access-tokens/_id/e2e-go-key"
	onceRead  = "/v1/secret/data/keploy/personal-access-tokens/keploy.io/k1"
	onceWrite = "/v1/secret/data/keploy/personal-access-tokens/keploy.io/k1?touch"
	onceID    = "pat-last-use-touch/-/1"
)

// onceTouch is a staged manager whose worker scope holds p1, p2 and p3, the
// three recordings of one series a background rule placed (no trail), in
// that order, each answering its own request. once says whether the table
// says the series answers once in a run (SetPoolOnce before SetPoolSeries,
// as the agent hands them).
func onceTouch(t *testing.T, once bool) *MockManager {
	t.Helper()
	mks := []*models.Mock{
		httpCall("p1", time.Second, onceByID),
		httpCall("p2", 2*time.Second, onceRead),
		httpCall("p3", 3*time.Second, onceWrite),
	}
	mm := stagedManager(t, nil, mks)
	mm.ServeExactWorker([]string{"p1", "p2", "p3"})
	if once {
		mm.SetPoolOnce([]string{onceID, ""})
	} else {
		mm.SetPoolOnce(nil)
	}
	mm.SetPoolSeries(map[string][]string{onceID: {"p1", "p2", "p3"}})
	return mm
}

// A once series answers each of its calls once in a test's window, and a
// second call of each, its last-placed recording's included, misses in
// that window, in the next test's and between tests: no repeat past the
// recorded count. Without the once mark the same series' last-placed
// recording answers the repeat, and between tests every recording answers
// again.
func TestAOnceSeriesAnswersEachCallOnceInTheRun(t *testing.T) {
	mm := onceTouch(t, true)
	mm.ServeExactTest("TestNonExistentApp", nil)
	got := []string{callURL(t, mm, onceByID, false), callURL(t, mm, onceRead, false), callURL(t, mm, onceWrite, false),
		callURL(t, mm, onceWrite, false), callURL(t, mm, onceByID, false)}
	if want := []string{"p1", "p2", "p3", "a miss", "a miss"}; !slices.Equal(got, want) {
		t.Fatalf("in the first window the once series answered %v, want %v", got, want)
	}
	mm.ServeExactTest("TestTestSuiteUpdate", nil)
	if got := callURL(t, mm, onceWrite, false); got != "a miss" {
		t.Fatalf("the touch again in the next window was answered %q, want a miss", got)
	}
	mm.ServeExactPool("k", nil)
	for _, url := range []string{onceByID, onceRead, onceWrite} {
		if got := callURL(t, mm, url, true); got != "a miss" {
			t.Fatalf("%s between tests after its window answer was answered %q, want a miss", url, got)
		}
	}
	if got := mm.SeriesRepeats(); got != nil {
		t.Fatalf("a once series counted repeats %v, want none", got)
	}

	plain := onceTouch(t, false)
	plain.ServeExactTest("TestNonExistentApp", nil)
	got = []string{callURL(t, plain, onceByID, false), callURL(t, plain, onceRead, false), callURL(t, plain, onceWrite, false), callURL(t, plain, onceWrite, false)}
	if want := []string{"p1", "p2", "p3", "p3"}; !slices.Equal(got, want) {
		t.Fatalf("without the once mark the series answered %v, want %v", got, want)
	}
	plain.ServeExactPool("k", nil)
	if got := callURL(t, plain, onceByID, true); got != "p1" {
		t.Fatalf("without the once mark p1 between tests was answered %q, want p1", got)
	}
}

// A once series' call made first between tests (before any test, as an
// application that authenticates at start does) is answered there once,
// and then misses between tests, through a door that keeps it, and in
// every window.
func TestAOnceSeriesAnsweredBetweenTestsMissesInTheWindows(t *testing.T) {
	mm := onceTouch(t, true)
	mm.ServeExactPool("k", nil)
	if got := callURL(t, mm, onceByID, true); got != "p1" {
		t.Fatalf("the first call between tests was answered %q, want p1", got)
	}
	if got := callURL(t, mm, onceByID, true); got != "a miss" {
		t.Fatalf("a second call between tests was answered %q, want a miss", got)
	}
	byName := map[string]*models.Mock{}
	mocks, _ := mm.GetSessionScopedMocks()
	for _, mk := range mocks {
		byName[mk.Name] = mk
	}
	if got := answer(mm, byName, "p2"); got != "p2" {
		t.Fatalf("p2 through a door that keeps it was answered %q, want p2", got)
	}
	if got := answer(mm, byName, "p2"); got != "a miss" {
		t.Fatalf("p2 again through a door that keeps it was answered %q, want a miss", got)
	}
	mm.ServeExactTest("TestB", nil)
	if got := callURL(t, mm, onceByID, false); got != "a miss" {
		t.Fatalf("p1 in a window after its answer between tests was answered %q, want a miss", got)
	}
	if got := callURL(t, mm, onceWrite, false); got != "p3" {
		t.Fatalf("p3, not yet answered, was answered %q in the window, want p3", got)
	}
}

// A table with no once series leaves the set none, never the previous
// set's, and an empty id marks nothing.
func TestSetPoolOnceNoneLeavesNone(t *testing.T) {
	mm := onceTouch(t, true)
	if idx := mm.exact.poolSeries.Load(); !idx.once("p1") || !idx.once("p3") || idx.once("") {
		t.Fatalf("the once series' members are not once: %+v", idx.of)
	}
	if set := *mm.exact.poolOnce.Load(); len(set) != 1 || !set[onceID] {
		t.Fatalf("the once set is %v, want only %s", set, onceID)
	}
	mm.SetPoolOnce(nil)
	mm.SetPoolSeries(map[string][]string{onceID: {"p1", "p2", "p3"}})
	if mm.exact.poolSeries.Load().once("p1") {
		t.Fatal("a table with no once series kept the previous one")
	}
}
