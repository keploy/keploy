package proxy

import (
	"errors"
	"strconv"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/matcher/learn"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
)

// seriesRowKind is a kind no parser registers a series reading for: the rows
// register theirs.
const seriesRowKind models.Kind = "ProxySeriesRow"

// readSeriesRowCall is seriesRowKind's reading: the operation in the
// metadata's "op" and one untyped number "at" (an epoch second).
func readSeriesRowCall(m *models.Mock) (learn.Shape, bool) {
	op := m.Spec.Metadata["op"]
	if op == "" {
		return learn.Shape{}, false
	}
	return learn.Shape{Key: op, Leaves: map[string]learn.Leaf{
		"body.at": {Path: "body.at", Noise: "body.at", Type: learn.Number, Value: m.Spec.Metadata["at"]},
	}}, true
}

// timerSet is a set staged as keploy test stages one under the background
// rules: an application call (app-N) and, 700 ms after it, a timer's claim
// (claim-N) every 2 s, whose "at" is the epoch second it was made, N = 1..6.
// The pool is claim-1 and claim-2 (recorded while no test was open); the rest
// are tests' (claim-3 and claim-4 T1's, claim-5 and claim-6 T2's).
func timerSet(t *testing.T) *MockManager {
	t.Helper()
	t.Setenv(provenswitch.Env, "1")
	probe := &models.Mock{Kind: seriesRowKind, Spec: models.MockSpec{Metadata: map[string]string{"op": "probe"}}}
	if _, registered := learn.SeriesShapeOf(probe); !registered { // a subtest's parent registered it
		t.Cleanup(learn.RegisterSeriesShape(seriesRowKind, readSeriesRowCall))
	}
	var recs []*models.Mock
	for i := 1; i <= 6; i++ {
		at := time.Duration(2*i)*time.Second + 500*time.Millisecond
		app := exactMock("app-"+strconv.Itoa(i), at-700*time.Millisecond, models.LifetimeSession)
		claim := exactMock("claim-"+strconv.Itoa(i), at, models.LifetimeSession)
		claim.Kind = seriesRowKind
		claim.Spec.Metadata = map[string]string{"op": "claim", "at": strconv.FormatInt(exactT0.Add(at).Unix(), 10)}
		recs = append(recs, app, claim)
	}
	mm := stagedManager(t, nil, recs)
	mm.ServeExactBackground(models.RecordedWithS12Routing, []string{"claim-1", "claim-2"}, nil)
	return mm
}

func seriesNames(s integrations.MarkedSeries) string {
	out := ""
	for i, m := range s.Recordings {
		if i > 0 {
			out += ","
		}
		out += m.Name
	}
	return out
}

// A timer's claim judging marked on every test's list it is on, the rest of
// it the pool's, is one marked series: the parser is handed what was learned
// of it (its "at" a read of the clock) and its recordings in the order of
// their calls, the newest last. A series a test's unmarked recording is in is
// a call that test must make, and is none; so is one only the pool holds
// (judging marked nothing of it), and so is every series under today's rules.
func TestMarkedSeriesIsATimersCallMarkedWhereverItWasRecorded(t *testing.T) {
	all := models.BackgroundMarks{
		"T1": {"claim-3": "a timer's call", "claim-4": "a timer's call"},
		"T2": {"claim-5": "a timer's call", "claim-6": "a timer's call"},
	}
	mm := timerSet(t)
	mm.SetUnrequired(all)
	mm.ServeExactTest("T1", nil)
	got, err := mm.MarkedSeries(seriesRowKind, nil)
	if err != nil || len(got) != 1 {
		t.Fatalf("MarkedSeries = %d series, %v; want the claims' one", len(got), err)
	}
	if n := seriesNames(got[0]); n != "claim-1,claim-2,claim-3,claim-4,claim-5,claim-6" {
		t.Fatalf("the series' recordings are %s; want every claim in the order of its call", n)
	}
	if _, ok := got[0].Series.Clock["body.at"]; !ok {
		t.Fatalf("the series learned %+v; want its at a read of the clock", got[0].Series)
	}
	if other, _ := mm.MarkedSeries(models.HTTP, nil); len(other) != 0 {
		t.Fatalf("another kind's series were handed out: %d", len(other))
	}

	for _, c := range []struct {
		name  string
		marks models.BackgroundMarks
	}{
		{"a test's unmarked recording is in it", models.BackgroundMarks{
			"T1": {"claim-3": "a timer's call"},
			"T2": {"claim-5": "a timer's call", "claim-6": "a timer's call"},
		}},
		{"judging marked none of it", models.BackgroundMarks{"T1": {"app-3": "a delayed write"}}},
		{"nothing is marked", nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			mm := timerSet(t)
			mm.SetUnrequired(c.marks)
			mm.ServeExactTest("T1", nil)
			if got, err := mm.MarkedSeries(seriesRowKind, nil); err != nil || len(got) != 0 {
				t.Fatalf("MarkedSeries = %d series, %v; want none", len(got), err)
			}
		})
	}
	t.Run("only the pool holds it", func(t *testing.T) {
		mm := timerSet(t)
		mm.ServeExactBackground(models.RecordedWithS12Routing,
			[]string{"claim-1", "claim-2", "claim-3", "claim-4", "claim-5", "claim-6"}, nil)
		mm.SetUnrequired(models.BackgroundMarks{"T1": {"app-3": "a delayed write"}})
		mm.ServeExactTest("T1", nil)
		if got, _ := mm.MarkedSeries(seriesRowKind, nil); len(got) != 0 {
			t.Fatalf("a series judging marked none of has %d marked series", len(got))
		}
	})
	t.Run("its background is not named yet", func(t *testing.T) {
		// Every claim marked, so only the staging's state can refuse it:
		// the trees still hold the previous set's recordings, whose names
		// may be this set's.
		mm := timerSet(t)
		mm.SetUnrequired(models.BackgroundMarks{
			"T0": {"claim-1": "a timer's call", "claim-2": "a timer's call"},
			"T1": {"claim-3": "a timer's call", "claim-4": "a timer's call"},
			"T2": {"claim-5": "a timer's call", "claim-6": "a timer's call"},
		})
		mm.ServeExactTest("T1", nil)
		mm.StageExactBackground(models.RecordedWithS12Routing) // the next set's staging
		if got, _ := mm.MarkedSeries(seriesRowKind, nil); len(got) != 0 {
			t.Fatalf("a staging whose background is not named yet has %d marked series", len(got))
		}
	})
	t.Run("today's rules", func(t *testing.T) {
		mm := timerSet(t)
		mm.ServeExactBackground("", nil, nil)
		mm.SetUnrequired(all)
		mm.ServeExactTest("T1", nil)
		if got, _ := mm.MarkedSeries(seriesRowKind, nil); len(got) != 0 {
			t.Fatalf("a set served by today's rules has %d marked series", len(got))
		}
	})
	t.Run("switch off", func(t *testing.T) {
		mm := timerSet(t)
		mm.SetUnrequired(all)
		mm.ServeExactTest("T1", nil)
		t.Setenv(provenswitch.Env, "")
		mm.ServeExactBackground(models.RecordedWithS12Routing, []string{"claim-1", "claim-2"}, nil)
		if got, _ := mm.MarkedSeries(seriesRowKind, nil); len(got) != 0 {
			t.Fatalf("with the switch off %d series are marked", len(got))
		}
	})
}

// The series are read once per staging, and again when what they are read
// from changes: the marks, and the store's on-disk recordings. A store that
// cannot read the set's recordings marks no series, and says why.
func TestMarkedSeriesIsReadAgainWhenTheMarksOrTheRecordingsChange(t *testing.T) {
	mm := timerSet(t)
	partial := models.BackgroundMarks{"T1": {"claim-3": "a timer's call"}, "T2": {"claim-5": "w", "claim-6": "w"}}
	mm.SetUnrequired(partial)
	mm.ServeExactTest("T1", nil)
	if got, _ := mm.MarkedSeries(seriesRowKind, nil); len(got) != 0 {
		t.Fatalf("claim-4 is unmarked, yet %d series are marked", len(got))
	}
	mm.SetUnrequired(models.BackgroundMarks{"T1": {"claim-3": "w", "claim-4": "w"}, "T2": {"claim-5": "w", "claim-6": "w"}})
	if got, _ := mm.MarkedSeries(seriesRowKind, nil); len(got) != 1 {
		t.Fatalf("after the marks changed %d series are marked; want the claims' one read again", len(got))
	}
	reads := 0
	mm.SetRecordedBefore(func(time.Time, func(*models.Mock) bool) error { reads++; return nil })
	for i := 0; i < 3; i++ {
		if got, _ := mm.MarkedSeries(seriesRowKind, nil); len(got) != 1 {
			t.Fatalf("call %d: %d series are marked; want the claims' one", i, len(got))
		}
	}
	if reads != 1 {
		t.Fatalf("the set's recordings were read %d times for 3 calls in one staging; want once", reads)
	}
	boom := errors.New("disk store closed")
	mm.SetRecordedBefore(func(time.Time, func(*models.Mock) bool) error { return boom })
	if got, err := mm.MarkedSeries(seriesRowKind, nil); !errors.Is(err, boom) || len(got) != 0 {
		t.Fatalf("with the recordings unreadable MarkedSeries = %d series, %v; want none and why", len(got), err)
	}
	mm.SetRecordedBefore(nil)
	if got, err := mm.MarkedSeries(seriesRowKind, nil); err != nil || len(got) != 1 {
		t.Fatalf("with the recordings readable again MarkedSeries = %d series, %v; want the claims' one", len(got), err)
	}
}

// A view made before exact serving began, and a call's held round, hand the
// parser the store's marked series: the held round only while the staging is
// still its call's (recordings' names restart in each set).
func TestMarkedSeriesIsForwardedByTheStoresWrappers(t *testing.T) {
	mm := timerSet(t)
	mm.SetUnrequired(models.BackgroundMarks{"T1": {"claim-3": "w", "claim-4": "w"}, "T2": {"claim-5": "w", "claim-6": "w"}})
	mm.ServeExactTest("T1", nil)
	scoped := &scopedMockDb{MockMemDb: mm}
	if got, err := integrations.MarkedSeriesOf(scoped, seriesRowKind, nil); err != nil || len(got) != 1 {
		t.Fatalf("a scoped view forwarded %d series, %v; want the store's one", len(got), err)
	}
	held, restaged := mm.HoldToRound(integrations.NewCallRound(mm.stagingEpoch.Load(), nil))
	if got, err := integrations.MarkedSeriesOf(held, seriesRowKind, nil); err != nil || len(got) != 1 {
		t.Fatalf("a call's held round forwarded %d series, %v; want the store's one", len(got), err)
	}
	mm.bumpStagingEpoch()
	if got, _ := integrations.MarkedSeriesOf(held, seriesRowKind, nil); len(got) != 0 || !restaged() {
		t.Fatalf("once another set was staged the held call is handed %d series (restaged %v); want none, restaged", len(got), restaged())
	}
	if got, _ := integrations.MarkedSeriesOf(nil, seriesRowKind, nil); got != nil {
		t.Fatalf("a store without the capability handed out %d series", len(got))
	}
}
