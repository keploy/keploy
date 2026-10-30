package learn

import (
	"reflect"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
)

// jobsMock is eJobs' recording: mock-6, 100ms before eRecAt.
func jobsMock(id, n string) *models.Mock {
	return &models.Mock{Name: "mock-6", Kind: models.PostgresV3, Spec: models.MockSpec{
		ReqTimestampMock: eRecAt.Add(-100 * time.Millisecond),
		ResTimestampMock: eRecAt.Add(-100*time.Millisecond + time.Millisecond),
		PostgresV3: &models.PostgresV3Spec{Type: models.PostgresV3TypeQuery, Query: &models.PostgresV3QuerySpec{
			SQLNormalized: "SELECT id, n FROM jobs",
			Response:      &models.PostgresV3Response{Rows: []models.PostgresV3Cells{{{Value: id}, {Value: n}}}, CommandComplete: "SELECT 1"},
		}},
	}}
}

// F's view of a recording it served changed (ServedLive) is what F served,
// built from the recording with what the application sent and F answered
// (integrations' liveMockOf), so it bears the recording's name and times:
// its identity (MockID) is the recording's. The set's caches by recording
// (setDerived, bodyStore) handed the view what they kept of the
// recording's payload, so in a tracker's test a word only the view holds
// read as one the recording gave there and passed as noise (FALSE GREEN),
// where a cache of no tracker refuses it. What a tracker's tests learn
// with a view is what a cache of no tracker learns, in each test of a set,
// with the recording and its view read whole or lazily (LazyRawOf), every
// payload kept or none.
func TestFsViewOfAServedRecordingIsNotReadAsTheRecording(t *testing.T) {
	const path = "bind.$8"
	defer func(b int64) { bodyBudget = b }(bodyBudget)
	for _, c := range []struct {
		name, rec, live     string
		srcN, viewID, viewN string
	}{
		// "6c7d" stands only in the view's id: F gave it, the recording did
		// not (unaccounted).
		{"the view alone holds the word the value holds now", "82", "6c7d", "5", chanceSrcLive, "5"},
		{"the view, the value took none of what F gave", chanceValRec, chanceValLive, "5", chanceSrcLive, "6"},
		{"the view, the value took the word F gave there", chanceValRec, chanceCopy, "5", chanceSrcLive, "6"},
	} {
		rec := Shape{Leaves: map[string]Leaf{path: {Path: path, Type: String, Value: c.rec}}}
		live := Shape{Leaves: map[string]Leaf{path: {Path: path, Type: String, Value: c.live}}}
		src, view := jobsMock(chanceSrcRec, c.srcN), jobsMock(c.viewID, c.viewN)
		if MockID(src) != MockID(view) {
			t.Fatalf("the row's view has another identity than its recording: it pins nothing")
		}
		want := ClassifyFollowing(noSet(clockedDrifts()), []string{path}, live, rec, eRecAt, nil, jobsServed(chanceSrcRec, c.srcN, RawOf(view, true)), nil, nil, nil)
		for _, lazy := range []bool{false, true} {
			for _, budget := range []int64{64 << 20, 0} {
				bodyBudget = budget
				tr := NewTracker()
				tr.NewSet()
				for test := 1; test <= 2; test++ {
					tr.Begin()
					dc := tr.Drifts()
					dc.appClock = true
					x, v := RawOf(src, true), RawOf(view, true)
					if lazy {
						x, v = dc.LazyRawOf(src, true, nil), dc.LazyRawOf(view, true, nil)
					}
					// The recording is read first in the set, as F serves it
					// before its view is kept.
					_ = x.payload()
					raw := append(eStartup(), x.ServedAs(6).ServedLive(v))
					got := ClassifyFollowing(dc, []string{path}, live, rec, eRecAt, nil, raw, nil, nil, nil)
					if !reflect.DeepEqual(got, want) {
						t.Errorf("FALSE GREEN? %s, lazy %v, budget %d, test %d: a tracker's test learned %s (%q), want %s (%q), as a cache of no tracker",
							c.name, lazy, budget, test, got[0].Kind, got[0].Why, want[0].Kind, want[0].Why)
					}
					tr.End()
				}
			}
		}
	}
}
