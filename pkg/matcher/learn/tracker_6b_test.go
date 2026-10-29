package learn

import (
	"context"
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

// A mock served before a test, anywhere in the set (the start-up, an earlier
// test, between tests), is an earlier place of every later test, once, in the
// order F first served it: F gave the application every one of them, and an
// application that consumed one in an earlier test may build a value on it
// in a later one. Each keeps the number F first served it as, across the set.
func TestTrackerKeepsWhatWasServedOutsideATestForEveryLaterTest(t *testing.T) {
	tr := NewTracker()
	startup := &models.Mock{Name: "startup"}
	tr.ServedMock(startup) // serve 1
	tr.Begin()             // test 1
	if got := beforeNames(tr); !reflect.DeepEqual(got, []string{"startup"}) {
		t.Fatalf("test 1: before %v", got)
	}
	own1 := &models.Mock{Name: "own-1"}
	tr.ServedMock(own1) // 2
	tr.End()
	tr.ServedMock(&models.Mock{Name: "between"}) // 3
	tr.ServedMock(startup)                       // 4: served again
	tr.Begin()                                   // test 2
	if got := beforeNames(tr); !reflect.DeepEqual(got, []string{"own-1", "between", "startup"}) {
		t.Fatalf("test 2: before %v", got)
	}
	own2 := &models.Mock{Name: "own-2"}
	tr.ServedMock(own2) // 5
	tr.End()
	tr.Begin() // test 3
	if got := beforeNames(tr); !reflect.DeepEqual(got, []string{"own-2", "startup", "between"}) {
		t.Fatalf("test 3: before %v, want test 2's own and everything served outside a test", got)
	}
	// The set: every mock F served in it, once, in the order it first served
	// each, and each one's first and last serve, as of the read.
	p := tr.Placed()
	var names []string
	for _, m := range p.Set {
		names = append(names, m.Name)
	}
	if !reflect.DeepEqual(names, []string{"startup", "own-1", "between", "own-2"}) {
		t.Fatalf("test 3: the set %v, want every mock F served in it, in the order it first served each", names)
	}
	want := [][2]int{{1, 4}, {2, 2}, {3, 3}, {5, 5}}
	if got := p.Order.Spans(p.Set); !reflect.DeepEqual(got, want) {
		t.Fatalf("test 3: serves %v, want %v", got, want)
	}
	if f, l := p.Order.Of(&models.Mock{Name: "never"}); f != 0 || l != 0 {
		t.Fatalf("a mock F never served is numbered %d..%d", f, l)
	}
	// A later serve changes no earlier read: its order and its set stay
	// what that read saw.
	tr.ServedMock(startup)                    // 6
	tr.ServedMock(&models.Mock{Name: "late"}) // 7
	if f, l := p.Order.Of(startup); f != 1 || l != 4 || len(p.Set) != 4 {
		t.Fatalf("an earlier read sees a later serve: startup %d..%d, set of %d", f, l, len(p.Set))
	}
	if f, l := tr.Placed().Order.Of(startup); f != 1 || l != 6 || len(tr.Placed().Set) != 5 {
		t.Fatalf("a later read: startup %d..%d, set of %d", f, l, len(tr.Placed().Set))
	}
	// A read keeps its set's numbers and places after the next set begins,
	// so a straggler's verdict does not depend on when NewSet ran; the new
	// set numbers its own serves from 1, with places of its own.
	g, err := p.Order.GivenPlaces(own2)
	if f, l := g.Span(); err != nil || f != 5 || l != 5 || g.Mock() != "own-2" {
		t.Fatalf("own-2's places served in %d..%d (%v)", f, l, err)
	}
	if _, ok := p.Order.places.of[MockID(own2)]; !ok {
		t.Fatal("own-2's places are not kept for the set")
	}
	tr.NewSet()
	if f, l := p.Order.Of(own2); f != 5 || l != 5 || len(beforeNames(tr)) != 0 || len(tr.Placed().Set) != 0 {
		t.Fatalf("a read of the previous set lost its order (%d..%d), or the new set keeps its places %v", f, l, beforeNames(tr))
	}
	tr.ServedMock(own2)
	if f, l := tr.Placed().Order.Of(own2); f != 1 || l != 1 {
		t.Fatalf("the new set's first mock is numbered %d..%d", f, l)
	}
	if f, l := p.Order.Of(own2); f != 5 || l != 5 {
		t.Fatalf("a read of the previous set numbers the new set's serve: %d..%d", f, l)
	}
	if q := tr.Placed().Order.places; q == p.Order.places || len(q.of) != 0 {
		t.Fatal("the new set reads the previous set's places")
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
		if ln := classifyProcessedAt(rec, live, typed, insert.Spec.ReqTimestampMock, w, receive, hello5, count, hello7, pending); !noiseOrRead(ln) {
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

// Every DriftCache a tracker makes reads a wall clock of no zone in the
// application's zone it was given (SetAppZone): the one it has, the next
// test's, the next set's; a new tracker's, in UTC alone.
func TestTrackerGivesItsCachesTheAppsZone(t *testing.T) {
	tr := NewTracker()
	if tr.Drifts().Zone() != nil {
		t.Fatalf("a new tracker's zone %v; want none but UTC", tr.Drifts().Zone())
	}
	zone := time.FixedZone("IST", 5*3600+1800)
	tr.SetAppZone(zone)
	if tr.Drifts().Zone() != zone {
		t.Fatalf("the cache it has reads %v; want %v", tr.Drifts().Zone(), zone)
	}
	tr.Begin()
	if tr.Drifts().Zone() != zone {
		t.Fatalf("the next test's cache reads %v; want %v", tr.Drifts().Zone(), zone)
	}
	tr.NewSet()
	if tr.Drifts().Zone() != zone {
		t.Fatalf("the next set's cache reads %v; want %v", tr.Drifts().Zone(), zone)
	}
	var none *DriftCache
	if none.Zone() != nil {
		t.Fatal("no cache reads a zone")
	}
}

// A tracker of keploy mock's client tests (SetClientTests) says so, and so
// does every DriftCache it makes: the one it has, the next test's, the next
// set's. One of keploy test's never does.
func TestTrackerSaysItsTestsAreClientTests(t *testing.T) {
	tr := NewTracker()
	if tr.ClientTests() || tr.Drifts().ClientTests() {
		t.Fatal("a new tracker's tests are client tests")
	}
	tr.SetClientTests(true)
	if !tr.ClientTests() || !tr.Drifts().ClientTests() {
		t.Fatal("SetClientTests(true): the tracker's cache is no client test's")
	}
	tr.Begin()
	if !tr.Drifts().ClientTests() {
		t.Fatal("the next test's cache is no client test's")
	}
	tr.NewSet()
	if !tr.Drifts().ClientTests() {
		t.Fatal("the next set's cache is no client test's")
	}
	tr.SetClientTests(false)
	tr.Begin()
	if tr.ClientTests() || tr.Drifts().ClientTests() {
		t.Fatal("SetClientTests(false): still client tests")
	}
	var none *Tracker
	if none.ClientTests() || none.Drifts().ClientTests() {
		t.Fatal("a nil tracker's tests are client tests")
	}
}

// Where a client test's begin moved the app's clock (keploy mock's tests,
// each at its recorded begin plus the lead), a call's window on F's clock
// runs from the begin, not from the previous call served before it: the
// test's first call has a window. A begin the clock moved after gives none,
// nor does a begin after the arrival; a serve after the begin is the start,
// as before; and with no serve before at all the begin is. keploy test's
// windows are as they were: a begin starts none (its clock moves as it
// sends the test case's request, after the begin).
func TestTrackerCallWindowStartsAtABeginThatMovedTheClock(t *testing.T) {
	rec := time.Date(2026, 10, 20, 15, 49, 55, 0, time.UTC)
	prev := &models.Mock{Name: "mock-0", Kind: models.HTTP, Spec: models.MockSpec{ReqTimestampMock: rec}}
	paired := &models.Mock{Name: "mock-1", Kind: models.HTTP, Spec: models.MockSpec{ReqTimestampMock: rec.Add(3 * time.Second)}}
	base := time.Date(2026, 10, 20, 15, 50, 35, 0, time.UTC)
	moved := base.Add(time.Second) // the begin moved the clock here
	before, after := 20*time.Second, -38*time.Second
	var unread time.Time
	clock := func(r time.Time) (time.Time, bool) {
		if r.Before(moved) {
			return r.Add(before), true
		}
		return r.Add(after), !r.Equal(unread)
	}
	arrival := moved.Add(5 * time.Millisecond)
	unread = moved.Add(3 * time.Millisecond)
	for _, tc := range []struct {
		name   string
		client bool
		served []time.Time
		began  time.Time
		from   time.Time // F's span's start on the real clock, zero for none
	}{
		{"a begin after the previous serve", true, []time.Time{base}, moved.Add(time.Millisecond), moved.Add(time.Millisecond)},
		{"no begin", true, []time.Time{base}, time.Time{}, time.Time{}},
		{"a begin the clock moved after", true, []time.Time{base}, moved.Add(-time.Millisecond), time.Time{}},
		{"a begin after the arrival", true, []time.Time{base}, arrival.Add(time.Millisecond), time.Time{}},
		{"a serve after the begin", true, []time.Time{base, moved.Add(2 * time.Millisecond)}, moved.Add(time.Millisecond), moved.Add(2 * time.Millisecond)},
		{"no serve before", true, nil, moved.Add(time.Millisecond), moved.Add(time.Millisecond)},
		// The clock cannot read the serve (a write in flight), and the begin
		// came before it: no window starts before the previous call.
		{"a begin before a serve the clock cannot read", true, []time.Time{unread}, moved.Add(time.Millisecond), time.Time{}},
		{"keploy test, a begin after the previous serve", false, []time.Time{base}, moved.Add(time.Millisecond), time.Time{}},
		{"keploy test, no serve before", false, nil, moved.Add(time.Millisecond), time.Time{}},
		{"keploy test, a serve after the begin", false, []time.Time{base, moved.Add(2 * time.Millisecond)}, moved.Add(time.Millisecond), moved.Add(2 * time.Millisecond)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tr := NewTracker()
			tr.SetClientTests(tc.client)
			tr.SetAppClock(clock)
			if !tc.began.IsZero() {
				tr.BeginAt(tc.began)
			}
			for _, at := range tc.served {
				tr.ServedMockAt(prev, at)
			}
			w := tr.CallWindowFrom(prev.Spec.ReqTimestampMock, paired, arrival)
			if tc.from.IsZero() {
				if w != nil {
					t.Fatalf("window %+v; want none", w)
				}
				return
			}
			want := &Window{Rec: Span{From: rec, To: paired.Spec.ReqTimestampMock}, F: Span{From: tc.from.Add(after), To: arrival.Add(after)}}
			// A client test's span carries its test's begin on the app's
			// clock (Span.Began), which the clock ran as one from to the
			// arrival; the recording's carries none, none given.
			if tc.client {
				want.F.Began = tc.began.Add(after)
			}
			if !reflect.DeepEqual(w, want) {
				t.Fatalf("window %+v; want %+v", w, want)
			}
		})
	}
}

// A client test's window in the recording starts no earlier than the test's
// recorded begin (RecordedBegin, given before the Begin that takes it): the
// test's first call, which no earlier recorded call precedes, has a window
// from it, and a recorded call before the begin (an earlier test's) gives
// way to it; one after it starts the window, as before. A begin after the
// recorded call gives none. A recorded begin is the next test's alone, and
// a new set forgets it and the running test's begin: its first call before
// a test begins has no window from the previous set's test. keploy test
// reads none.
func TestTrackerCallWindowStartsAtATestsRecordedBegin(t *testing.T) {
	recBegin := time.Date(2026, 10, 20, 15, 49, 54, 0, time.UTC)
	paired := &models.Mock{Name: "mock-1", Kind: models.HTTP, Spec: models.MockSpec{ReqTimestampMock: recBegin.Add(3 * time.Second)}}
	base := time.Date(2026, 10, 20, 15, 50, 35, 0, time.UTC)
	moved := base.Add(time.Second) // the begin moved the clock here
	after := recBegin.Add(FullRoundLead).Sub(moved)
	clock := func(r time.Time) (time.Time, bool) {
		if r.Before(moved) {
			return r.Add(time.Hour), true
		}
		return r.Add(after), true
	}
	began, arrival := moved.Add(time.Millisecond), moved.Add(5*time.Millisecond)
	f := Span{From: began.Add(after), To: arrival.Add(after)}
	// served after the begin: a serve the app's clock runs as one from to
	// the arrival, so F's span starts there whoever's test it is.
	served := began.Add(time.Millisecond)
	for _, tc := range []struct {
		name     string
		client   bool
		prevRec  time.Time
		recBegin time.Time
		serve    time.Time // the previous serve, on the real clock
		from     time.Time // the recording's span's start, zero for none
	}{
		{"no earlier recorded call", true, time.Time{}, recBegin, base, recBegin},
		{"an earlier test's recorded call", true, recBegin.Add(-time.Second), recBegin, base, recBegin},
		{"an earlier call of the test", true, recBegin.Add(time.Second), recBegin, base, recBegin.Add(time.Second)},
		{"no recorded begin", true, time.Time{}, time.Time{}, base, time.Time{}},
		{"a recorded begin after the call", true, time.Time{}, paired.Spec.ReqTimestampMock.Add(time.Millisecond), base, time.Time{}},
		{"keploy test", false, time.Time{}, recBegin, base, time.Time{}},
		{"keploy test, a serve after the begin", false, time.Time{}, recBegin, served, time.Time{}},
		{"keploy test, an earlier recorded call", false, recBegin.Add(-time.Second), recBegin, served, recBegin.Add(-time.Second)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tr := NewTracker()
			tr.SetClientTests(tc.client)
			tr.SetAppClock(clock)
			tr.ServedMockAt(paired, tc.serve)
			tr.RecordedBegin(tc.recBegin)
			tr.BeginAt(began)
			w := tr.CallWindowFrom(tc.prevRec, paired, arrival)
			if tc.from.IsZero() {
				if w != nil {
					t.Fatalf("window %+v; want none", w)
				}
				return
			}
			f := f
			if !tc.serve.Equal(base) {
				f = Span{From: tc.serve.Add(after), To: arrival.Add(after)}
			}
			want := &Window{Rec: Span{From: tc.from, To: paired.Spec.ReqTimestampMock}, F: f}
			// A client test's spans carry each run's test begin (Span.Began):
			// the recorded begin, and the begin on the app's clock.
			if tc.client {
				want.Rec.Began, want.F.Began = tc.recBegin, began.Add(after)
			}
			if !reflect.DeepEqual(w, want) {
				t.Fatalf("window %+v; want %+v", w, want)
			}
		})
	}
	tr := NewTracker()
	tr.SetClientTests(true)
	tr.SetAppClock(clock)
	tr.RecordedBegin(recBegin)
	tr.BeginAt(began)
	tr.BeginAt(began)
	if w := tr.CallWindowFrom(time.Time{}, paired, arrival); w != nil {
		t.Fatalf("a second test, no recorded begin given: window %+v; want none", w)
	}
	tr.RecordedBegin(recBegin)
	tr.BeginAt(began)
	tr.NewSet()
	if w := tr.CallWindowFrom(time.Time{}, paired, arrival); w != nil {
		t.Fatalf("a new set before its first test: window %+v; want none from the last set's recorded begin", w)
	}
	if w := tr.CallWindowFrom(recBegin.Add(-time.Second), paired, arrival); w != nil {
		t.Fatalf("a new set before its first test: window %+v; want none from the last set's begin", w)
	}
	// A serve in the new set the clock runs as one from: still no window
	// from the last set's recorded begin.
	tr.ServedMockAt(paired, began.Add(time.Millisecond))
	if w := tr.CallWindowFrom(time.Time{}, paired, arrival); w != nil {
		t.Fatalf("a new set's serve: window %+v; want none from the last set's recorded begin", w)
	}
	// A recorded begin given before a new set is no test's of it.
	tr.RecordedBegin(recBegin)
	tr.NewSet()
	tr.BeginAt(began)
	if w := tr.CallWindowFrom(time.Time{}, paired, arrival); w != nil {
		t.Fatalf("the new set's first test: window %+v; want none from a recorded begin given before the set", w)
	}
	var none *Tracker
	none.RecordedBegin(recBegin)
	none.SetAppZone(time.UTC)
}

// The running test's number (Epoch) moves on Begin and NewSet and on nothing
// else: an End, a serve and an answer keep it. It is Test's number, and a nil
// tracker's is 0. A learn step reads it as a call is matched and as it
// decides, and equal numbers are one test of one set.
func TestTrackerEpochMovesOnlyWithTheRunningTest(t *testing.T) {
	var none *Tracker
	if got := none.Epoch(); got != 0 {
		t.Fatalf("a nil tracker's epoch is %d; want 0", got)
	}
	tr := NewTracker()
	same := func(step string, want uint64) {
		t.Helper()
		if got := tr.Epoch(); got != want {
			t.Fatalf("after %s the epoch is %d; want %d", step, got, want)
		}
		if n, _, _, _ := tr.Test(); n != want {
			t.Fatalf("after %s Test's number is %d; want the epoch, %d", step, n, want)
		}
	}
	e := tr.Epoch()
	same("NewTracker", e)
	m := &models.Mock{Name: "mock-1", Kind: models.HTTP}
	tr.ServedMock(m)
	same("a start-up serve", e)
	tr.Begin()
	e1 := tr.Epoch()
	if e1 == e {
		t.Fatalf("Begin kept the epoch %d", e)
	}
	same("Begin", e1)
	tr.ServedMock(m)
	tr.Answer(e1, m)
	same("a serve and an answer", e1)
	tr.End()
	same("End", e1)
	tr.Begin()
	e2 := tr.Epoch()
	if e2 == e1 {
		t.Fatalf("the next Begin kept the epoch %d", e1)
	}
	tr.NewSet()
	if e3 := tr.Epoch(); e3 == e2 {
		t.Fatalf("NewSet kept the epoch %d", e2)
	}
}

// The open test's number (OpenEpoch) is the epoch while a test is open, from
// Begin until End or NewSet, and 0 while none is: before the first Begin,
// after End, after NewSet. It is never 0 for an open test (Begin moves the
// epoch off 0), so a store that stamps a test's view with it can say "no
// test of the tracker's" with 0.
func TestTrackerOpenEpochIsTheEpochOnlyWhileATestIsOpen(t *testing.T) {
	var none *Tracker
	if got := none.OpenEpoch(); got != 0 {
		t.Fatalf("a nil tracker's open epoch is %d; want 0", got)
	}
	tr := NewTracker()
	closed := func(step string) {
		t.Helper()
		if got := tr.OpenEpoch(); got != 0 {
			t.Fatalf("after %s, with no test open, the open epoch is %d; want 0", step, got)
		}
	}
	open := func(step string) {
		t.Helper()
		e := tr.Epoch()
		if e == 0 {
			t.Fatalf("after %s an open test's epoch is 0", step)
		}
		if got := tr.OpenEpoch(); got != e {
			t.Fatalf("after %s the open epoch is %d; want the epoch, %d", step, got, e)
		}
	}
	closed("NewTracker")
	m := &models.Mock{Name: "mock-1", Kind: models.HTTP}
	tr.ServedMock(m)
	closed("a start-up serve")
	tr.Begin()
	open("Begin")
	tr.ServedMock(m)
	tr.Answer(tr.Epoch(), m)
	open("a serve and an answer")
	tr.End()
	closed("End")
	tr.ServedMock(m)
	closed("a serve between tests")
	tr.Begin()
	open("the next Begin")
	tr.NewSet()
	closed("NewSet")
	tr.Begin()
	open("the new set's first Begin")
}

// Once the test set's learn step has ended (keploy replay's retry cycles of
// record's full round, SetLearnEnded) a parser finds no tracker on its
// connection's context, so a learn step that reads it per request decides
// none; a parser that kept it reads LearnEnded. The set's staging takes it
// back up. A nil tracker answers false and ignores the call.
func TestTrackerFromGivesNoTrackerOnceTheLearnStepEnded(t *testing.T) {
	tr := NewTracker()
	ctx := WithTracker(context.Background(), tr)
	if TrackerFrom(ctx) != tr || tr.LearnEnded() {
		t.Fatal("precondition: a parser finds the tracker while the set's learn step runs")
	}
	tr.SetLearnEnded(true)
	if got := TrackerFrom(ctx); got != nil {
		t.Fatal("a parser still finds the tracker once the set's learn step ended")
	}
	if !tr.LearnEnded() {
		t.Fatal("a parser that kept the tracker is not told the set's learn step ended")
	}
	tr.SetLearnEnded(false)
	if TrackerFrom(ctx) != tr || tr.LearnEnded() {
		t.Fatal("the set's staging did not take the learn step back up")
	}
	var none *Tracker
	none.SetLearnEnded(true)
	if none.LearnEnded() || TrackerFrom(context.Background()) != nil {
		t.Fatal("a nil tracker says the learn step ended")
	}
}
