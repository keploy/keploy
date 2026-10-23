package learn

import (
	"reflect"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
)

func beforeNames(t *Tracker) []string {
	_, _, _, before := t.Test()
	var out []string
	for _, m := range before {
		out = append(out, m.Name)
	}
	return out
}

// A mock served while no test is open (the start-up, between tests) is an
// earlier place of every later test, once; one served in a test is one of
// the next test only, as before.
func TestTrackerKeepsWhatWasServedOutsideATestForEveryLaterTest(t *testing.T) {
	tr := NewTracker()
	tr.ServedMock(&models.Mock{Name: "startup"})
	tr.Begin() // test 1
	if got := beforeNames(tr); !reflect.DeepEqual(got, []string{"startup"}) {
		t.Fatalf("test 1: before %v", got)
	}
	tr.ServedMock(&models.Mock{Name: "own-1"})
	tr.End()
	tr.ServedMock(&models.Mock{Name: "between"})
	tr.ServedMock(&models.Mock{Name: "startup"}) // served again
	tr.Begin()                                   // test 2
	if got := beforeNames(tr); !reflect.DeepEqual(got, []string{"own-1", "between", "startup"}) {
		t.Fatalf("test 2: before %v", got)
	}
	tr.ServedMock(&models.Mock{Name: "own-2"})
	tr.End()
	tr.Begin() // test 3
	if got := beforeNames(tr); !reflect.DeepEqual(got, []string{"own-2", "startup", "between"}) {
		t.Fatalf("test 3: before %v, want test 2's own and everything served outside a test", got)
	}
}

// A new test set forgets what the previous set served: its mocks reuse the
// previous set's names (mock-0 again), and none of the previous set's is an
// earlier place of the new set's tests. Without NewSet, a mock is still kept
// by its identity, not its name: another set's mock-0 never hides this one's.
func TestTrackerTellsTestSetsApart(t *testing.T) {
	at := func(s int) time.Time { return time.Date(2026, 10, 16, s, 0, 0, 0, time.UTC) }
	set1 := &models.Mock{Name: "mock-0", Spec: models.MockSpec{ReqTimestampMock: at(1)}}
	set2 := &models.Mock{Name: "mock-0", Spec: models.MockSpec{ReqTimestampMock: at(2)}}
	for _, newSet := range []bool{true, false} {
		tr := NewTracker()
		tr.ServedMock(set1)
		tr.Begin()
		tr.End()
		if newSet {
			tr.NewSet()
		}
		tr.ServedMock(set2)
		tr.Begin()
		tr.End()
		tr.Begin() // set 2, test 2
		_, _, _, before := tr.Test()
		var has1, has2 bool
		for _, m := range before {
			has1, has2 = has1 || m == set1, has2 || m == set2
		}
		if !has2 || has1 == newSet {
			t.Errorf("NewSet %v: set 2's test 2 has set 1's mock-0 %v, set 2's %v", newSet, has1, has2)
		}
	}
}

// The start-up's calls share one drift cache, so each earlier recording's
// text is read once there; a test, and a new set, start afresh.
func TestTrackerKeepsADriftCacheForTheStartUp(t *testing.T) {
	tr := NewTracker()
	startUp := tr.Drifts()
	if startUp == nil || tr.Drifts() != startUp {
		t.Fatal("the start-up's calls share no drift cache")
	}
	tr.Begin()
	test := tr.Drifts()
	if test == startUp {
		t.Fatal("the first test read the start-up's cache")
	}
	tr.End()
	tr.NewSet()
	if d := tr.Drifts(); d == nil || d == startUp || d == test {
		t.Fatal("a new set's start-up has no cache of its own")
	}
}

// A call's window runs, in the recording, from the set's previous
// data-plane recording to the paired one, and in this run, on the app's
// clock, from the previous data-plane call the store served to the call's
// arrival. A protocol-phase call is no previous call, in either run.
func TestTrackerCallWindow(t *testing.T) {
	rec := time.Date(2026, 10, 20, 15, 49, 55, 0, time.UTC)
	at := func(name string, d time.Duration, kind models.Kind, config bool) *models.Mock {
		m := &models.Mock{Name: name, Kind: kind, Spec: models.MockSpec{ReqTimestampMock: rec.Add(d), Metadata: map[string]string{}}}
		if config {
			m.Spec.Metadata["type"] = "config"
		}
		return m
	}
	boot := at("mock-0", -time.Second, models.Mongo, false)
	receive := at("mock-1", 0, models.HTTP, false)
	hello := at("mock-2", 2*time.Millisecond, models.Mongo, true)
	insert := at("mock-4", 4*time.Millisecond, models.Mongo, false)
	later := at("mock-5", 9*time.Millisecond, models.HTTP, false)
	recorded := []*models.Mock{insert, later, receive, hello, boot}

	base := time.Date(2026, 10, 20, 15, 50, 35, 0, time.UTC)
	offset := 21 * time.Second
	clock := func(r time.Time) (time.Time, bool) { return r.Add(offset), true }

	tr := NewTracker()
	if w := tr.CallWindow(insert, recorded, base.Add(3*time.Second)); w != nil {
		t.Fatalf("a window with no app clock: %+v", w)
	}
	tr.SetAppClock(clock)
	if w := tr.CallWindow(insert, recorded, base.Add(3*time.Second)); w != nil {
		t.Fatalf("a window with no call served before: %+v", w)
	}
	tr.ServedMockAt(receive, base.Add(2*time.Second))
	tr.ServedMockAt(hello, base.Add(2*time.Second+time.Millisecond))
	// Served after the insert arrived, on another connection.
	tr.ServedMockAt(later, base.Add(4*time.Second))
	w := tr.CallWindow(insert, recorded, base.Add(2*time.Second+3*time.Millisecond))
	want := &Window{
		Rec: Span{From: receive.Spec.ReqTimestampMock, To: insert.Spec.ReqTimestampMock},
		F:   Span{From: base.Add(2*time.Second + offset), To: base.Add(2*time.Second + 3*time.Millisecond + offset)},
	}
	if !reflect.DeepEqual(w, want) {
		t.Fatalf("window %+v; want %+v", w, want)
	}
	// The set's first data-plane call has no previous call recorded.
	if w := tr.CallWindow(boot, recorded, base.Add(time.Second)); w != nil {
		t.Fatalf("the first call's window: %+v", w)
	}
	// A clock that cannot read an instant gives no window.
	tr.SetAppClock(func(r time.Time) (time.Time, bool) { return r, r.After(base.Add(2*time.Second + time.Millisecond)) })
	if w := tr.CallWindow(insert, recorded, base.Add(2*time.Second+3*time.Millisecond)); w != nil {
		t.Fatalf("a window over an unread instant: %+v", w)
	}
	// A new set forgets what the last one served.
	tr.SetAppClock(clock)
	tr.NewSet()
	if w := tr.CallWindow(insert, recorded, base.Add(5*time.Second)); w != nil {
		t.Fatalf("a window across sets: %+v", w)
	}
	// A recording with no recorded time has no window.
	tr.ServedMockAt(receive, base)
	if w := tr.CallWindow(&models.Mock{Name: "mock-9", Kind: models.Mongo}, recorded, base.Add(time.Second)); w != nil {
		t.Fatalf("a window of no recorded time: %+v", w)
	}
}

// Serves the store reports out of order (two connections) are read in the
// order they happened: the previous call is the latest served before the
// arrival, whichever was reported first.
func TestTrackerCallWindowReadsServesInTheirOrder(t *testing.T) {
	rec := time.Date(2026, 10, 20, 15, 49, 55, 0, time.UTC)
	data := func(name string, d time.Duration) *models.Mock {
		return &models.Mock{Name: name, Kind: models.HTTP, Spec: models.MockSpec{ReqTimestampMock: rec.Add(d)}}
	}
	prev, paired := data("mock-0", 0), data("mock-1", 4*time.Millisecond)
	base := time.Date(2026, 10, 20, 15, 50, 35, 0, time.UTC)
	tr := NewTracker()
	tr.SetAppClock(func(r time.Time) (time.Time, bool) { return r, true })
	tr.ServedMockAt(data("late", time.Second), base.Add(5*time.Second))
	tr.ServedMockAt(prev, base.Add(2*time.Second))
	tr.ServedMockAt(data("early", -time.Second), base)
	w := tr.CallWindow(paired, []*models.Mock{prev}, base.Add(3*time.Second))
	if w == nil || !w.F.From.Equal(base.Add(2*time.Second)) {
		t.Fatalf("window %+v; want F from the serve at +2s, the latest before the arrival", w)
	}
}

// A window is one run of the app's clock: if the clock was re-written
// between the previous serve and the arrival (a test's begin), there is
// none, though each instant reads.
func TestTrackerCallWindowIsOneRunOfTheAppsClock(t *testing.T) {
	rec := time.Date(2026, 10, 20, 15, 49, 55, 0, time.UTC)
	prev := &models.Mock{Name: "mock-0", Kind: models.HTTP, Spec: models.MockSpec{ReqTimestampMock: rec}}
	paired := &models.Mock{Name: "mock-1", Kind: models.HTTP, Spec: models.MockSpec{ReqTimestampMock: rec.Add(4 * time.Millisecond)}}
	base := time.Date(2026, 10, 20, 15, 50, 35, 0, time.UTC)
	for _, jump := range []time.Duration{0, 300 * time.Second, -300 * time.Second} {
		tr := NewTracker()
		tr.SetAppClock(func(r time.Time) (time.Time, bool) {
			if r.Before(base.Add(time.Millisecond)) {
				return r.Add(time.Minute), true
			}
			return r.Add(time.Minute + jump), true
		})
		tr.ServedMockAt(prev, base)
		w := tr.CallWindowFrom(prev.Spec.ReqTimestampMock, paired, base.Add(3*time.Millisecond))
		if (w != nil) != (jump == 0) {
			t.Fatalf("jump %v: window %+v; want one only with one offset", jump, w)
		}
	}
	// A previous recorded call no earlier than the paired one gives none.
	tr := NewTracker()
	tr.SetAppClock(func(r time.Time) (time.Time, bool) { return r, true })
	tr.ServedMockAt(prev, base)
	if w := tr.CallWindowFrom(paired.Spec.ReqTimestampMock, paired, base.Add(time.Millisecond)); w != nil {
		t.Fatalf("a previous call at the paired one's own time gave %+v", w)
	}
}

// A call still unanswered when paired was sent is no previous call of
// paired's: the queue consumer's lane, leg (b), run r2's recordings (keploy
// 6db6cd0f3). The
// consumer read processed_at (13.458) after its duplicate read (mock-6, sent
// 13.112, answered 13.458619) and sent its insert at 13.459990; post-url-1's
// handler sent its upsert (mock-8) at 13.459791 between the two, which the
// lane's hold kept unanswered until 14.421. The upsert is another flow's: the
// window runs from the duplicate read, and holds the read, in both readings.
// Answered before the insert was sent, it would be the previous call, and
// the read falls before it; with no answer time it still counts.
func TestAPendingCallIsNoPreviousCall(t *testing.T) {
	ts := func(sec, nsec int) time.Time { return time.Date(2026, 10, 22, 18, 38, sec, nsec, time.UTC) }
	const read = 1792694293458
	receive := r1Receive("")
	receive.Spec.ReqTimestampMock, receive.Spec.ResTimestampMock = ts(12, 106755139), ts(13, 111622044)
	receive.Spec.HTTPReq.Header["X-Amz-Date"] = "20261022T183812Z"
	receive.Spec.HTTPResp.Header["Date"] = "Thu, 22 Oct 2026 18:38:13 GMT"
	hello5 := r2Hello("mock-5", 8, ts(13, 205118312), ts(13, 458911204), read)
	hello7 := r2Hello("mock-7", 10, ts(13, 301455028), ts(13, 459120377), read)
	count := r2Mongo("mock-6", ts(13, 112600041), ts(13, 458619421),
		[]string{`{ SectionSingle msg: {"aggregate":"events","pipeline":[{"$match":{"event":"S12 message"}}],"cursor":{},"$db":"keploy"} }`},
		`{ SectionSingle msg: {"cursor":{"firstBatch":[],"id":{"$numberLong":"0"},"ns":"keploy.events"},"ok":{"$numberDouble":"1.0"}} }`)
	upsert := func(answered time.Time) *models.Mock {
		return r2Mongo("mock-8", ts(13, 459791587), answered,
			[]string{`{ SectionSingle msg: {"update":"url-shortener","ordered":true,"$db":"keploy"} }`,
				`{ SectionSingle identifier: updates , msgs: [ {"q":{"_id":"Lhr4BWAi"},"u":{"$set":{"_id":"Lhr4BWAi","created":{"$date":{"$numberLong":"1792694293219"}},"url":"https://google.com"}},"upsert":true} ] }`},
			`{ SectionSingle msg: {"n":{"$numberInt":"1"},"ok":{"$numberDouble":"1.0"}} }`)
	}
	insert := &models.Mock{Name: "mock-9", Kind: models.Mongo, Spec: models.MockSpec{ReqTimestampMock: ts(13, 459990012)}}
	prevOf := func(recorded ...*models.Mock) time.Time {
		p := NewPreviousCall(insert)
		for _, m := range recorded {
			p.See(m)
		}
		return p.At()
	}
	pending := upsert(ts(14, 421828601))
	if got := prevOf(receive, hello5, count, hello7, pending); !got.Equal(count.Spec.ReqTimestampMock) {
		t.Fatalf("the previous call is at %s; want the duplicate read's %s: the upsert was unanswered when the insert was sent", got.Format(time.RFC3339Nano), count.Spec.ReqTimestampMock.Format(time.RFC3339Nano))
	}
	answered := upsert(ts(13, 459900000))
	if got := prevOf(receive, hello5, count, hello7, answered); !got.Equal(answered.Spec.ReqTimestampMock) {
		t.Fatalf("an upsert answered before the insert was sent is no previous call: %s", got.Format(time.RFC3339Nano))
	}
	unknown := upsert(time.Time{})
	if got := prevOf(receive, count, unknown); !got.Equal(unknown.Spec.ReqTimestampMock) {
		t.Fatalf("an upsert of no answer time is no previous call: %s", got.Format(time.RFC3339Nano))
	}

	// F at boot: the duplicate read served, then the insert, on the app's
	// clock F set past the recording.
	rec := time.UnixMilli(read).UTC()
	offset := FullRoundLead + 4*time.Second + 217*time.Millisecond
	real0 := time.Date(2026, 10, 22, 19, 2, 0, 0, time.UTC)
	tr := NewTracker()
	tr.SetAppClock(func(r time.Time) (time.Time, bool) { return r.Add(rec.Sub(real0) + offset), true })
	tr.ServedMockAt(count, real0.Add(-2*time.Millisecond))
	arrival := real0.Add(time.Millisecond)
	live, _ := tr.appClock(real0)
	w := tr.CallWindowFrom(prevOf(receive, hello5, count, hello7, pending), insert, arrival)
	if w == nil || !w.Rec.holds(rec, time.Millisecond) || !w.F.holds(live, time.Millisecond) {
		t.Fatalf("the insert's window %+v does not hold its read (recorded %s, live %s)", w, rec.Format(time.RFC3339Nano), live.Format(time.RFC3339Nano))
	}
	for _, typed := range []bool{false, true} {
		if ln := classifyProcessedAt(rec, live, typed, insert.Spec.ReqTimestampMock, w, receive, hello5, count, hello7, pending); ln.Kind != KindNoise {
			t.Errorf("typed %v: %s (from %q, %s); want noise, a read in its call's window", typed, ln.Kind, ln.From.Mock, ln.Why)
		}
	}
	// With the upsert as the previous call (answered before the insert was
	// sent), the read falls before the window: the typed reading's words are
	// then searched, and the receive's Date header holds its second.
	w = tr.CallWindowFrom(prevOf(receive, hello5, count, hello7, answered), insert, arrival)
	if ln := classifyProcessedAt(rec, live, true, insert.Spec.ReqTimestampMock, w, receive, hello5, count, hello7, answered); ln.Kind == KindNoise {
		t.Error("a read before its call's previous call, typed: noise; want it checked, and refused by the receive")
	}
}
