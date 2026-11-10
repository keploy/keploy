package proxy

import (
	"maps"
	"slices"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
)

// seriesSweep is a staged manager whose worker scope holds s1 and s2, the
// two recordings of one series a declared rule placed (both trail TestA, as
// a trailing rule marks them), placed in that order, and w, an ordinary
// worker: recording. Every recording answers the URL its name's call makes
// (sweepURL): the series one request, w another.
func seriesSweep(t *testing.T) *MockManager {
	t.Helper()
	mks := []*models.Mock{
		httpCall("s1", time.Second, sweepURL),
		httpCall("s2", 2*time.Second, sweepURL),
		httpCall("w", 3*time.Second, "/poll"),
	}
	mm := stagedManager(t, nil, mks)
	mm.ServeExactWorker([]string{"s1", "s2", "w"})
	mm.SetPoolTrails(map[string]string{"s1": "TestA", "s2": "TestA"})
	mm.SetPoolSeries(map[string][]string{"sweep/TestA/1": {"s1", "s2"}})
	return mm
}

const sweepURL = "/delete?app=a1&limit=1"

// callURL answers a call to url as a parser that matches the request
// exactly and, among the recordings that match, ranks them by recorded time
// (earliest first, Mongo v2's matchTieKey) or latest first, and consumes
// through a delete door, reading again when the consume is refused: the name
// served, or "a miss".
func callURL(t *testing.T, mm *MockManager, url string, latestFirst bool) string {
	t.Helper()
	for i := 0; i < 10; i++ {
		mocks, _ := mm.GetSessionScopedMocks()
		var hit *models.Mock
		for _, mk := range mocks {
			if mk.Spec.HTTPReq == nil || mk.Spec.HTTPReq.URL != url {
				continue
			}
			if hit == nil || mk.Spec.ReqTimestampMock.Before(hit.Spec.ReqTimestampMock) != latestFirst {
				hit = mk
			}
		}
		if hit == nil {
			return "a miss"
		}
		if mm.DeleteUnFilteredMock(*hit) {
			return hit.Name
		}
	}
	t.Fatalf("%s: every consume was refused", url)
	return ""
}

// A series of two recordings that a slower replay's application calls five
// times in two test windows: the first two calls are answered by the two
// recordings, in placement order, and each of the three repeats past the
// recorded count by the series' last-placed recording, in either window;
// the repeats are counted by series. An ordinary worker: recording still
// answers at most once in the windows.
func TestSeriesLastMemberAnswersAgainInWindows(t *testing.T) {
	mm := seriesSweep(t)
	mm.ServeExactTest("TestB", nil)
	got := []string{callURL(t, mm, sweepURL, false), callURL(t, mm, sweepURL, false)}
	mm.ServeExactPool("k", nil)
	mm.ServeExactTest("TestC", nil)
	for i := 0; i < 3; i++ {
		got = append(got, callURL(t, mm, sweepURL, false))
	}
	if want := []string{"s1", "s2", "s2", "s2", "s2"}; !slices.Equal(got, want) {
		t.Fatalf("the series answered %v, want %v", got, want)
	}
	if got, want := mm.SeriesRepeats(), map[string]int64{"sweep/TestA/1": 3}; !maps.Equal(got, want) {
		t.Fatalf("repeats %v, want %v", got, want)
	}
	if got := []string{callURL(t, mm, "/poll", false), callURL(t, mm, "/poll", false)}; !slices.Equal(got, []string{"w", "a miss"}) {
		t.Fatalf("an ordinary worker recording answered %v in the windows, want [w a miss]", got)
	}
	// A new staging starts the windows' answers and their count afresh.
	mm.bumpStagingEpoch()
	if got := mm.SeriesRepeats(); got != nil {
		t.Fatalf("a new staging reported the last one's repeats %v", got)
	}
}

// The repeat is answered only once every recording of the series answered
// in the windows, and only by its last-placed recording: a parser that
// takes the latest first is answered s2, then s1 (s2 already answered and
// s1 has not), then s2 again; one that takes the earliest first is never
// answered s1 twice.
func TestSeriesRepeatWaitsForEveryMemberAndIsTheLast(t *testing.T) {
	for _, tc := range []struct {
		name        string
		latestFirst bool
		want        []string
	}{
		{"latest first", true, []string{"s2", "s1", "s2", "s2"}},
		{"earliest first", false, []string{"s1", "s2", "s2", "s2"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mm := seriesSweep(t)
			mm.ServeExactTest("TestB", nil)
			var got []string
			for range tc.want {
				got = append(got, callURL(t, mm, sweepURL, tc.latestFirst))
			}
			if !slices.Equal(got, tc.want) {
				t.Fatalf("the series answered %v, want %v", got, tc.want)
			}
			if got := mm.SeriesRepeats()["sweep/TestA/1"]; got != 2 {
				t.Fatalf("repeats %d, want 2", got)
			}
		})
	}
}

// A repeat that differs from the series' request at one field (its limit)
// is no repeat: once the series answered, the call misses, as any request
// no recording holds does. Serving stays exact.
func TestSeriesRepeatWithAnotherFieldMisses(t *testing.T) {
	mm := seriesSweep(t)
	mm.ServeExactTest("TestB", nil)
	if got := []string{callURL(t, mm, sweepURL, false), callURL(t, mm, sweepURL, false)}; !slices.Equal(got, []string{"s1", "s2"}) {
		t.Fatalf("the series answered %v, want [s1 s2]", got)
	}
	if got := callURL(t, mm, "/delete?app=a1&limit=2", false); got != "a miss" {
		t.Fatalf("a call that differs at limit was answered %q, want a miss", got)
	}
	if got := mm.SeriesRepeats(); got != nil {
		t.Fatalf("repeats %v, want none", got)
	}
}

// Between tests a series answers again, as the rest of the worker scope
// does: a trail move's answer-once rule does not hold a series, and every
// call it answered there is in what the run consumed (GetConsumedMocks),
// through a delete door and through one that keeps the recording, so a
// series answered only between tests is one the run made. A series' answers
// between tests do not count toward the windows' (s1 still answers first
// in a window).
func TestConsumedHoldsWorkerAnsweredBetweenTests(t *testing.T) {
	mm := seriesSweep(t)
	mm.ServeExactPool("k", nil)
	got := []string{callURL(t, mm, sweepURL, true), callURL(t, mm, sweepURL, true), callURL(t, mm, sweepURL, true)}
	if want := []string{"s2", "s2", "s2"}; !slices.Equal(got, want) {
		t.Fatalf("the series answered %v between tests, want %v", got, want)
	}
	byName := map[string]*models.Mock{}
	mocks, _ := mm.GetSessionScopedMocks()
	for _, mk := range mocks {
		byName[mk.Name] = mk
	}
	if got := answer(mm, byName, "s1"); got != "s1" {
		t.Fatalf("s1 through a door that keeps it was answered %q, want s1", got)
	}
	if got := answer(mm, byName, "s1"); got != "s1" {
		t.Fatalf("s1 again between tests was answered %q, want s1", got)
	}
	consumed := map[string]bool{}
	for _, st := range mm.GetConsumedMocks() {
		consumed[st.Name] = true
	}
	if !consumed["s1"] || !consumed["s2"] {
		t.Fatalf("what the run consumed is %v, want s1 and s2 in it", consumed)
	}
	mm.ServeExactTest("TestB", nil)
	if got := callURL(t, mm, sweepURL, false); got != "s1" {
		t.Fatalf("the first call in a window was answered %q, want s1", got)
	}
	if got := mm.SeriesRepeats(); got != nil {
		t.Fatalf("repeats %v, want none", got)
	}
}

// With no series the scope's trail move answers once, as before: a series
// table that names other recordings changes nothing for it, and an empty
// one leaves the set none.
func TestATrailNotInASeriesStillAnswersOnce(t *testing.T) {
	mm := seriesSweep(t)
	mm.SetPoolSeries(map[string][]string{"other/TestZ/1": {"zz"}, "empty": nil, "blank": {""}})
	mm.ServeExactTest("TestB", nil)
	got := []string{callURL(t, mm, sweepURL, false), callURL(t, mm, sweepURL, false), callURL(t, mm, sweepURL, false)}
	if want := []string{"s1", "s2", "a miss"}; !slices.Equal(got, want) {
		t.Fatalf("answers %v, want %v", got, want)
	}
	mm.ServeExactPool("k", nil)
	if got := callURL(t, mm, sweepURL, false); got != "a miss" {
		t.Fatalf("a trail used in a window was answered %q between tests, want a miss", got)
	}
	if idx := mm.exact.poolSeries.Load(); idx.has("") || idx.members["blank"] != nil || idx.members["empty"] != nil {
		t.Fatalf("a series with no name in it was kept: %+v", idx)
	}
	mm.SetPoolSeries(nil)
	if mm.exact.poolSeries.Load().has("s1") {
		t.Fatal("an empty series table kept the previous one")
	}
}

// A name two series list is held by the first in series order; the series
// it is the last of in the second is the one before it there.
func TestSetPoolSeriesNameInTwoSeries(t *testing.T) {
	mm := seriesSweep(t)
	mm.SetPoolSeries(map[string][]string{"a": {"x", "y"}, "b": {"z", "y"}})
	idx := mm.exact.poolSeries.Load()
	if got := idx.of["y"]; got != (seriesMember{id: "a", last: true}) {
		t.Fatalf("y is %+v, want the last of a", got)
	}
	if got := idx.members["b"]; !slices.Equal(got, []string{"z"}) {
		t.Fatalf("b holds %v, want [z]", got)
	}
	if got := idx.of["z"]; got != (seriesMember{id: "b", last: true}) {
		t.Fatalf("z is %+v, want the last of b", got)
	}
}

// The consume is the guard a read cannot be (two calls may read one member
// before either consumes it): in a window, a second consume of a series'
// member is refused until every member answered, a member that is not the
// last is refused again after that, and the last then answers each repeat,
// counted.
func TestASecondConsumeOfASeriesMemberWaitsForTheSeries(t *testing.T) {
	mm := seriesSweep(t)
	mm.prepareOutside()
	byName := map[string]*models.Mock{}
	mocks, _ := mm.GetSessionScopedMocks()
	for _, mk := range mocks {
		byName[mk.Name] = mk
	}
	for i, step := range []struct {
		name string
		want exactVerdict
	}{
		{"s2", exactServed},
		{"s2", exactRefused},
		{"s1", exactServed},
		{"s1", exactRefused},
		{"s2", exactServed},
		{"s2", exactServed},
	} {
		if v := mm.consumeOutside(byName[step.name], true); v != step.want {
			t.Fatalf("consume %d of %s in a window: %v, want %v", i, step.name, v, step.want)
		}
	}
	if got := mm.SeriesRepeats()["sweep/TestA/1"]; got != 2 {
		t.Fatalf("repeats %d, want 2", got)
	}
}
