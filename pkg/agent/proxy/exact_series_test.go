package proxy

import (
	"context"
	"errors"
	"strconv"
	"sync/atomic"
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
	got, err := mm.MarkedSeries(context.Background(), seriesRowKind, nil)
	if err != nil || len(got) != 1 {
		t.Fatalf("MarkedSeries = %d series, %v; want the claims' one", len(got), err)
	}
	if n := seriesNames(got[0]); n != "claim-1,claim-2,claim-3,claim-4,claim-5,claim-6" {
		t.Fatalf("the series' recordings are %s; want every claim in the order of its call", n)
	}
	if _, ok := got[0].Series.Clock["body.at"]; !ok {
		t.Fatalf("the series learned %+v; want its at a read of the clock", got[0].Series)
	}
	if other, _ := mm.MarkedSeries(context.Background(), models.HTTP, nil); len(other) != 0 {
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
			if got, err := mm.MarkedSeries(context.Background(), seriesRowKind, nil); err != nil || len(got) != 0 {
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
		if got, _ := mm.MarkedSeries(context.Background(), seriesRowKind, nil); len(got) != 0 {
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
		if got, _ := mm.MarkedSeries(context.Background(), seriesRowKind, nil); len(got) != 0 {
			t.Fatalf("a staging whose background is not named yet has %d marked series", len(got))
		}
	})
	t.Run("today's rules", func(t *testing.T) {
		mm := timerSet(t)
		mm.ServeExactBackground("", nil, nil)
		mm.SetUnrequired(all)
		mm.ServeExactTest("T1", nil)
		if got, _ := mm.MarkedSeries(context.Background(), seriesRowKind, nil); len(got) != 0 {
			t.Fatalf("a set served by today's rules has %d marked series", len(got))
		}
	})
	t.Run("switch off", func(t *testing.T) {
		mm := timerSet(t)
		mm.SetUnrequired(all)
		mm.ServeExactTest("T1", nil)
		t.Setenv(provenswitch.Env, "")
		mm.ServeExactBackground(models.RecordedWithS12Routing, []string{"claim-1", "claim-2"}, nil)
		if got, _ := mm.MarkedSeries(context.Background(), seriesRowKind, nil); len(got) != 0 {
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
	if got, _ := mm.MarkedSeries(context.Background(), seriesRowKind, nil); len(got) != 0 {
		t.Fatalf("claim-4 is unmarked, yet %d series are marked", len(got))
	}
	mm.SetUnrequired(models.BackgroundMarks{"T1": {"claim-3": "w", "claim-4": "w"}, "T2": {"claim-5": "w", "claim-6": "w"}})
	if got, _ := mm.MarkedSeries(context.Background(), seriesRowKind, nil); len(got) != 1 {
		t.Fatalf("after the marks changed %d series are marked; want the claims' one read again", len(got))
	}
	reads := 0
	mm.SetRecordedBefore(func(time.Time, func(*models.Mock) bool) error { reads++; return nil })
	for i := 0; i < 3; i++ {
		if got, _ := mm.MarkedSeries(context.Background(), seriesRowKind, nil); len(got) != 1 {
			t.Fatalf("call %d: %d series are marked; want the claims' one", i, len(got))
		}
	}
	if reads != 1 {
		t.Fatalf("the set's recordings were read %d times for 3 calls in one staging; want once", reads)
	}
	boom := errors.New("disk store closed")
	mm.SetRecordedBefore(func(time.Time, func(*models.Mock) bool) error { return boom })
	if got, err := mm.MarkedSeries(context.Background(), seriesRowKind, nil); !errors.Is(err, boom) || len(got) != 0 {
		t.Fatalf("with the recordings unreadable MarkedSeries = %d series, %v; want none and why", len(got), err)
	}
	mm.SetRecordedBefore(nil)
	if got, err := mm.MarkedSeries(context.Background(), seriesRowKind, nil); err != nil || len(got) != 1 {
		t.Fatalf("with the recordings readable again MarkedSeries = %d series, %v; want the claims' one", len(got), err)
	}
}

// A series judging marked as one (models.BackgroundSeriesPrefix on any list
// that holds a recording of it) is answered again; one marked with any other
// why is the set's one-offs: set-wide, each of its recordings answers at most
// once in a staging (Once), whichever test's window the call lands in, and a
// new read (new marks, a new staging) starts with none answered. Once takes
// nothing from the test's own recordings.
func TestMarkedSeriesSaysASeriesIsAnsweredAgainAndAOneOffOnce(t *testing.T) {
	series := "series_of: a timer's call"
	for _, c := range []struct {
		name  string
		marks models.BackgroundMarks
		again bool
	}{
		{"marked as a series", models.BackgroundMarks{
			"T1": {"claim-3": series, "claim-4": series}, "T2": {"claim-5": series, "claim-6": series}}, true},
		{"one list marks it as a series", models.BackgroundMarks{
			"T1": {"claim-3": "a timer's call", "claim-4": "a timer's call"}, "T2": {"claim-5": series, "claim-6": "w"}}, true},
		{"one-offs", models.BackgroundMarks{
			"T1": {"claim-3": "a timer's call", "claim-4": "a timer's call"}, "T2": {"claim-5": "w", "claim-6": "w"}}, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			mm := timerSet(t)
			mm.SetUnrequired(c.marks)
			mm.ServeExactTest("T1", nil)
			got, err := mm.MarkedSeries(context.Background(), seriesRowKind, nil)
			if err != nil || len(got) != 1 {
				t.Fatalf("MarkedSeries = %d series, %v; want the claims' one", len(got), err)
			}
			if got[0].Again != c.again {
				t.Fatalf("Again = %v; want %v", got[0].Again, c.again)
			}
			if got[0].Once == nil {
				t.Fatal("Once is nil: a one-off could answer no call")
			}
		})
	}

	mm := timerSet(t)
	marks := models.BackgroundMarks{"T1": {"claim-3": "w", "claim-4": "w"}, "T2": {"claim-5": "w", "claim-6": "w"}}
	mm.SetUnrequired(marks)
	mm.ServeExactTest("T1", nil)
	got, _ := mm.MarkedSeries(context.Background(), seriesRowKind, nil)
	if len(got) != 1 || got[0].Once == nil {
		t.Fatalf("MarkedSeries = %+v; want the claims' one with Once", got)
	}
	once := got[0].Once
	if !once("claim-5") || once("claim-5") {
		t.Fatal("claim-5 answered set-wide twice, or not once")
	}
	if !once("claim-6") {
		t.Fatal("claim-6 was spent by claim-5's answer")
	}
	var t2 []*models.Mock // T2's own list
	for _, r := range got[0].Recordings {
		if r.Name == "claim-5" || r.Name == "claim-6" {
			t2 = append(t2, r)
		}
	}
	mm.ServeExactTest("T2", t2) // another test's window: the same staging
	if again, _ := mm.MarkedSeries(context.Background(), seriesRowKind, nil); len(again) != 1 || again[0].Once("claim-5") {
		t.Fatal("in the next test's window claim-5 answered set-wide again in one staging")
	}
	if test, unused, ok := mm.ExactTestUnusedBackground(); !ok || test != "T2" || len(unused) != 2 {
		t.Fatalf("T2's unused marked recordings = %s %v (%v); want claim-5 and claim-6: a set-wide answer is none of the test's own", test, unused, ok)
	}
	mm.SetUnrequired(marks.Clone()) // a new read
	if again, _ := mm.MarkedSeries(context.Background(), seriesRowKind, nil); len(again) != 1 || !again[0].Once("claim-5") {
		t.Fatal("a new read of the set's series kept the last read's one-off answers")
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
	if got, err := integrations.MarkedSeriesOf(context.Background(), scoped, seriesRowKind, nil); err != nil || len(got) != 1 {
		t.Fatalf("a scoped view forwarded %d series, %v; want the store's one", len(got), err)
	}
	held, restaged := mm.HoldToRound(integrations.NewCallRound(mm.stagingEpoch.Load(), nil))
	if got, err := integrations.MarkedSeriesOf(context.Background(), held, seriesRowKind, nil); err != nil || len(got) != 1 {
		t.Fatalf("a call's held round forwarded %d series, %v; want the store's one", len(got), err)
	}
	mm.bumpStagingEpoch()
	if got, _ := integrations.MarkedSeriesOf(context.Background(), held, seriesRowKind, nil); len(got) != 0 || !restaged() {
		t.Fatalf("once another set was staged the held call is handed %d series (restaged %v); want none, restaged", len(got), restaged())
	}
	if got, _ := integrations.MarkedSeriesOf(context.Background(), nil, seriesRowKind, nil); got != nil {
		t.Fatalf("a store without the capability handed out %d series", len(got))
	}
}

// The set's recordings are read for its series as it is staged, on a
// goroutine of the manager's, so no call pays for the read: whichever of
// what they are read from is named last (the marks, the background, the
// on-disk store, a staging of the set again), the read is under way before
// the first call, which never reads them itself, and a call that comes while
// they are still being read waits for the read (event-driven, no budget), or
// for its own ctx. A read another takes the place of (the marks or the store
// changed, the next set's staging) stops at its next recording, and a call
// that waited for it is handed the read that took its place.
func TestMarkedSeriesIsReadAtStagingNotByTheFirstCall(t *testing.T) {
	all := models.BackgroundMarks{"T1": {"claim-3": "w", "claim-4": "w"}, "T2": {"claim-5": "w", "claim-6": "w"}}
	partial := models.BackgroundMarks{"T1": {"claim-3": "w"}, "T2": {"claim-5": "w", "claim-6": "w"}}
	// gated is an on-disk source whose first read once armed waits for
	// gate, then offers one recording and keeps whether the read went on
	// (went); reads counts its reads once armed.
	type gated struct {
		armed         atomic.Bool
		reads         atomic.Int32
		entered, gate chan struct{}
		went          atomic.Bool
	}
	newGated := func() *gated {
		g := &gated{entered: make(chan struct{}, 1), gate: make(chan struct{})}
		g.armed.Store(true)
		return g
	}
	src := func(g *gated) func(time.Time, func(*models.Mock) bool) error {
		return func(_ time.Time, visit func(*models.Mock) bool) error {
			if g.armed.Load() && g.reads.Add(1) == 1 {
				g.entered <- struct{}{}
				<-g.gate
				g.went.Store(visit(exactMock("late", time.Second, models.LifetimeSession)))
			}
			return nil
		}
	}
	pending := func(mm *MockManager) *setSeriesRead {
		mm.exact.series.mu.Lock()
		defer mm.exact.series.mu.Unlock()
		return mm.exact.series.read
	}
	for _, c := range []struct {
		name string
		last func(mm *MockManager, g *gated)
	}{
		{"the marks named last", func(mm *MockManager, g *gated) {
			mm.SetRecordedBefore(src(g))
			mm.SetUnrequired(all)
		}},
		{"the background named last", func(mm *MockManager, g *gated) {
			mm.StageExactBackground(models.RecordedWithS12Routing)
			mm.SetRecordedBefore(src(g))
			mm.SetUnrequired(all)
			mm.ServeExactBackground(models.RecordedWithS12Routing, []string{"claim-1", "claim-2"}, nil)
		}},
		{"the on-disk store named last", func(mm *MockManager, g *gated) {
			mm.SetUnrequired(all)
			<-pending(mm).done
			mm.SetRecordedBefore(src(g))
		}},
		{"the set staged again", func(mm *MockManager, g *gated) {
			g.armed.Store(false)
			mm.SetRecordedBefore(src(g))
			mm.SetUnrequired(all)
			<-pending(mm).done
			g.armed.Store(true)
			mm.bumpStagingEpoch()
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			mm := timerSet(t)
			g := newGated()
			c.last(mm, g)
			r := pending(mm)
			if r == nil || r.key.epoch != mm.stagingEpoch.Load() {
				close(g.gate)
				t.Fatal("no read of the set's series began as it was staged: the first call would do it")
			}
			<-g.entered
			mm.ServeExactTest("T1", nil)
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			if got, err := mm.MarkedSeries(ctx, seriesRowKind, nil); !errors.Is(err, context.Canceled) || len(got) != 0 {
				close(g.gate)
				t.Fatalf("a call whose ctx ended while the read was under way got %d series, %v; want none and ctx's error", len(got), err)
			}
			close(g.gate)
			got, err := mm.MarkedSeries(context.Background(), seriesRowKind, nil)
			if err != nil || len(got) != 1 {
				t.Fatalf("MarkedSeries = %d series, %v; want the claims' one, the staging's read", len(got), err)
			}
			if n := g.reads.Load(); n != 1 {
				t.Fatalf("the set's on-disk recordings were read %d times; want once, as it was staged", n)
			}
		})
	}
	for _, c := range []struct {
		name  string
		first func(mm *MockManager, g *gated)
		next  func(mm *MockManager)
		want  int
	}{
		{"the marks changed", func(mm *MockManager, g *gated) {
			mm.SetRecordedBefore(src(g))
			mm.SetUnrequired(partial) // claim-4 unmarked: no series
		}, func(mm *MockManager) { mm.SetUnrequired(all) }, 1},
		{"the store changed", func(mm *MockManager, g *gated) {
			mm.SetUnrequired(all)
			<-pending(mm).done
			mm.SetRecordedBefore(src(g))
		}, func(mm *MockManager) { mm.SetRecordedBefore(nil) }, 1},
		{"the next set's staging", func(mm *MockManager, g *gated) {
			mm.SetRecordedBefore(src(g))
			mm.SetUnrequired(all)
		}, func(mm *MockManager) { mm.StageExactBackground(models.RecordedWithS12Routing) }, 0},
	} {
		t.Run("a read another takes the place of: "+c.name, func(t *testing.T) {
			mm := timerSet(t)
			mm.ServeExactTest("T1", nil)
			g := newGated()
			c.first(mm, g)
			old := pending(mm)
			if old == nil {
				close(g.gate)
				t.Fatal("no read of the set's series began as it was staged")
			}
			<-g.entered
			c.next(mm)
			close(g.gate)
			<-old.done
			if !old.stopped || g.went.Load() {
				t.Fatalf("the read of what was named first was not stopped at its next recording (stopped %v, went on %v)", old.stopped, g.went.Load())
			}
			got, err := mm.seriesFrom(context.Background(), old, seriesRowKind, nil)
			if err != nil || len(got) != c.want {
				t.Fatalf("a call that waited for the stopped read got %d series, %v; want %d, the read that took its place", len(got), err, c.want)
			}
			if c.want == 0 && pending(mm) != nil {
				t.Fatal("a read of the set's series is pending while the next set's background is not named")
			}
		})
	}
	t.Run("each kind's series are learned once", func(t *testing.T) {
		mm := timerSet(t)
		mm.SetUnrequired(all)
		mm.ServeExactTest("T1", nil)
		first, _ := mm.MarkedSeries(context.Background(), seriesRowKind, nil)
		again, _ := mm.MarkedSeries(context.Background(), seriesRowKind, excuseAll{})
		if len(first) != 1 || len(again) != 1 || len(again[0].Series.Clock) != len(first[0].Series.Clock) {
			t.Fatalf("the series were learned again for a later call (%d, then %d); want the first call's, once per staging", len(first), len(again))
		}
	})
}

// excuseAll excuses every leaf (as keploy.yml noise would).
type excuseAll struct{}

func (excuseAll) Excused(learn.Leaf) (bool, bool) { return true, false }
