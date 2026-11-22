package learn

import (
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
)

// node-jwt's seeded start-up, as recorded: Sequelize reads JavaScript's Date
// once per role in one tick (Promise.all), at .961, then sends the role
// INSERTs queued one behind another on one connection: mock-13 reached
// keploy at .969, mock-14 at .9746. The call before the burst, mock-12,
// reached keploy at .9575. mock-14's stamp was read before mock-13 reached
// keploy.
type burst struct {
	read, before, first, call time.Time
	stamp                     Leaf
	o                         models.ClockOffset
}

const burstPath = "bind.$3"

func newBurst(t *testing.T) burst {
	t.Helper()
	const layout = "2006-01-02T15:04:05.000Z07:00"
	read := time.Date(2026, 11, 2, 22, 2, 9, 961_000_000, time.UTC)
	b := burst{read: read, before: read.Add(-3500 * time.Microsecond), first: read.Add(8 * time.Millisecond), call: read.Add(13600 * time.Microsecond)}
	b.stamp = Leaf{Path: burstPath, Type: String, Value: read.Format(layout)}
	live := Leaf{Path: burstPath, Type: String, Value: read.Add(FullRoundLead).Format(layout)}
	ln := ClassifyIn(clockedDrifts(), []string{burstPath}, Shape{Leaves: map[string]Leaf{burstPath: live}}, Shape{Leaves: map[string]Leaf{burstPath: b.stamp}}, b.call, nil, nil, nil, nil)[0]
	o, ok := ln.Clock()
	if ln.Kind != KindClockRead || !ok || !o.Plain() {
		t.Fatalf("learned %s %+v,%v (%s); want a plain read", ln.Kind, o, ok, ln.Why)
	}
	b.o = o
	return b
}

func burstMock(name string) *models.Mock { return &models.Mock{Name: name, Kind: models.PostgresV3} }

// burstSiblings names mock-13 the call's sibling (the same statement on the
// same connection), as the protocol's sibling rule would.
func burstSiblings(m *models.Mock) bool { return m.Name == "mock-13" }

// A replay checks a learned plain read in the call's live span. A serve of
// the call's sibling starts no span of it: the burst's queued call holds its
// own read, and the same read is still refused behind a call of another
// statement, or made before the latest serve that is not a sibling's.
func TestASiblingServeStartsNoLiveSpan(t *testing.T) {
	b := newBurst(t)
	for _, row := range []struct {
		what    string
		serves  []*models.Mock
		at      []time.Time
		sibling Siblings
		holds   bool
		because string
	}{
		{"the burst: mock-14 queued behind its sibling mock-13", []*models.Mock{burstMock("mock-12"), burstMock("mock-13")}, []time.Time{b.before, b.first}, burstSiblings, true, ""},
		{"a read made before the latest serve that is not a sibling's", []*models.Mock{burstMock("mock-12"), burstMock("other"), burstMock("mock-13")}, []time.Time{b.before, b.read.Add(2 * time.Millisecond), b.first}, burstSiblings, false, "it changed"},
		{"a read queued behind a call of another statement", []*models.Mock{burstMock("mock-12"), burstMock("mock-13")}, []time.Time{b.before, b.first}, func(*models.Mock) bool { return false }, false, "it changed"},
		{"no sibling named (nil) keeps LiveSpanOf's span", []*models.Mock{burstMock("mock-12"), burstMock("mock-13")}, []time.Time{b.before, b.first}, nil, false, "it changed"},
		{"a lone read", []*models.Mock{burstMock("mock-12")}, []time.Time{b.before}, nil, true, ""},
		{"only siblings served before it: no bound", []*models.Mock{burstMock("mock-13")}, []time.Time{b.first}, burstSiblings, false, ""},
	} {
		tr := NewTracker()
		tr.SetAppClock(func(r time.Time) (time.Time, bool) { return r, true })
		for i, m := range row.serves {
			tr.ServedMockAt(m, row.at[i])
		}
		s, known := tr.LiveSpanOfSiblings(b.call, burstMock("mock-14"), row.sibling)
		got, why := CheckClockRead(burstPath, b.o, b.stamp, s, known)
		if got != row.holds || !strings.Contains(why, row.because) {
			t.Errorf("%s: span %v..%v known=%v: holds=%v %q; want %v naming %q", row.what, s.From, s.To, known, got, why, row.holds, row.because)
		}
		// ClockSpanOfSiblings reads the same span from the tracker on ctx.
		cs, cknown := ClockSpanOfSiblings(WithTracker(t.Context(), tr), b.call, burstMock("mock-14"), row.sibling)
		if cknown != known || !cs.From.Equal(s.From) || !cs.To.Equal(s.To) {
			t.Errorf("%s: ClockSpanOfSiblings %v..%v,%v; want LiveSpanOfSiblings' %v..%v,%v", row.what, cs.From, cs.To, cknown, s.From, s.To, known)
		}
	}
}

// Checked against the open test's own recording, a span starts at the later
// of the latest serve made before the test began and the latest of the
// test's own serves (LiveSpanOf). A sibling's serve starts neither.
func TestASiblingServeStartsNoOwnSpan(t *testing.T) {
	b := newBurst(t)
	for _, row := range []struct {
		what    string
		began   time.Time
		own     map[string]bool
		serves  []*models.Mock
		at      []time.Time
		sibling Siblings
		holds   bool
	}{
		{"the sibling served before the test began", b.first.Add(500 * time.Microsecond), map[string]bool{"mock-14": true},
			[]*models.Mock{burstMock("mock-12"), burstMock("mock-13")}, []time.Time{b.before, b.first}, burstSiblings, true},
		{"the sibling served before the test began, no sibling named", b.first.Add(500 * time.Microsecond), map[string]bool{"mock-14": true},
			[]*models.Mock{burstMock("mock-12"), burstMock("mock-13")}, []time.Time{b.before, b.first}, nil, false},
		{"the sibling one of the test's own serves, a worker's between", b.before.Add(-time.Millisecond), map[string]bool{"mock-12": true, "mock-13": true, "mock-14": true},
			[]*models.Mock{burstMock("mock-12"), burstMock("worker"), burstMock("mock-13")}, []time.Time{b.before, b.read.Add(2 * time.Millisecond), b.first}, burstSiblings, true},
		{"the sibling one of the test's own serves, no sibling named", b.before.Add(-time.Millisecond), map[string]bool{"mock-12": true, "mock-13": true, "mock-14": true},
			[]*models.Mock{burstMock("mock-12"), burstMock("worker"), burstMock("mock-13")}, []time.Time{b.before, b.read.Add(2 * time.Millisecond), b.first}, nil, false},
		{"an own serve after the read that is not a sibling's", b.before.Add(-time.Millisecond), map[string]bool{"mock-12": true, "other": true, "mock-13": true, "mock-14": true},
			[]*models.Mock{burstMock("mock-12"), burstMock("other"), burstMock("mock-13")}, []time.Time{b.before, b.read.Add(2 * time.Millisecond), b.first}, burstSiblings, false},
	} {
		tr := NewTracker()
		tr.SetAppClock(func(r time.Time) (time.Time, bool) { return r, true })
		own := row.own
		tr.SetServeOwner(func(m *models.Mock) bool { return own[m.Name] })
		began := false
		for i, m := range row.serves {
			if !began && !row.at[i].Before(row.began) {
				tr.BeginAt(row.began)
				began = true
			}
			tr.ServedMockAt(m, row.at[i])
		}
		if !began {
			tr.BeginAt(row.began)
		}
		s, known := tr.LiveSpanOfSiblings(b.call, burstMock("mock-14"), row.sibling)
		got, why := CheckClockRead(burstPath, b.o, b.stamp, s, known)
		if got != row.holds {
			t.Errorf("%s: span %v..%v known=%v: holds=%v %q; want %v", row.what, s.From, s.To, known, got, why, row.holds)
		}
	}
}

// record's learn step reads the call's window from its previous call in
// both halves: the recording's (PreviousCall) and this run's (the store's
// serves). A sibling starts neither half, so the burst's queued call whose
// stamp did not move by the lead is learned as a plain read of the clock,
// as a replay then checks it; read from its sibling, no window holds it.
func TestASiblingStartsNoLearnWindow(t *testing.T) {
	const layout = "2006-01-02T15:04:05.000Z07:00"
	read := time.Date(2026, 11, 2, 22, 2, 9, 961_000_000, time.UTC)
	rec := func(name string, req, res time.Duration) *models.Mock {
		m := burstMock(name)
		m.Spec.ReqTimestampMock, m.Spec.ResTimestampMock = read.Add(req), read.Add(res)
		return m
	}
	recorded := []*models.Mock{rec("mock-12", -3500*time.Microsecond, -2200*time.Microsecond), rec("mock-13", 8*time.Millisecond, 9*time.Millisecond)}
	paired := rec("mock-14", 13600*time.Microsecond, 15*time.Millisecond)
	// This run is the recording moved by ten minutes, not by the lead: only
	// a window of the call can show its stamp is a read of the clock.
	shift := 10 * time.Minute
	live := Leaf{Path: burstPath, Type: String, Value: read.Add(shift).Format(layout)}
	stamp := Leaf{Path: burstPath, Type: String, Value: read.Format(layout)}
	for _, row := range []struct {
		what    string
		sibling Siblings
		prev    string
		read    bool
	}{
		{"mock-13 the call's sibling", burstSiblings, "mock-12", true},
		{"no sibling named", nil, "mock-13", false},
	} {
		prev := NewPreviousCallOf(paired, row.sibling)
		for _, m := range append(recorded, paired) {
			prev.See(m)
		}
		var want time.Time
		for _, m := range recorded {
			if m.Name == row.prev {
				want = m.Spec.ReqTimestampMock
			}
		}
		if !prev.At().Equal(want) {
			t.Errorf("%s: previous call at %v; want %s's %v", row.what, prev.At(), row.prev, want)
		}
		tr := NewTracker()
		tr.SetAppClock(func(r time.Time) (time.Time, bool) { return r, true })
		for _, m := range recorded {
			tr.ServedMockAt(m, m.Spec.ReqTimestampMock.Add(shift))
		}
		w := tr.CallWindowFromSiblings(prev.At(), paired, paired.Spec.ReqTimestampMock.Add(shift), row.sibling)
		if w == nil || !w.Rec.From.Equal(want) || !w.F.From.Equal(want.Add(shift)) {
			t.Errorf("%s: window %+v; want both halves from %s's call", row.what, w, row.prev)
		}
		ln := ClassifyIn(clockedDrifts(), []string{burstPath}, Shape{Leaves: map[string]Leaf{burstPath: live}}, Shape{Leaves: map[string]Leaf{burstPath: stamp}}, paired.Spec.ReqTimestampMock, nil, nil, nil, w)[0]
		o, ok := ln.Clock()
		if got := ln.Kind == KindClockRead && ok && o.Plain(); got != row.read {
			t.Errorf("%s: window %+v learned %s (%s); want a plain read %v", row.what, w, ln.Kind, ln.Why, row.read)
		}
	}
	// A nil Siblings is NewPreviousCall's and CallWindowFrom's.
	a, b := NewPreviousCallOf(paired, nil), NewPreviousCall(paired)
	for _, m := range recorded {
		a.See(m)
		b.See(m)
	}
	if !a.At().Equal(b.At()) {
		t.Errorf("NewPreviousCallOf(nil) at %v; NewPreviousCall at %v", a.At(), b.At())
	}
}

// A serve is filed at the instant the store made it, and its call may have
// reached keploy earlier (servedInstant.from). Leaving a sibling out, the
// span still starts at the latest instant a call that is not a sibling's
// reached keploy, wherever its serve was filed.
func TestASiblingLeftOutKeepsTheLatestArrival(t *testing.T) {
	b := newBurst(t)
	ms := time.Millisecond
	type serve struct {
		name     string
		at, from time.Duration // from the read
	}
	for _, row := range []struct {
		what   string
		serves []serve // in the order the store filed them
		from   time.Duration
	}{
		{"mock-12 filed after a call that reached keploy before it, the sibling filed first",
			[]serve{{"mock-13", -ms, -ms}, {"other", -ms / 2, -3500*time.Microsecond - 5*ms}, {"mock-12", -ms / 4, -3500 * time.Microsecond}}, -3500 * time.Microsecond},
		{"mock-11 reached keploy last of the three, filed first",
			[]serve{{"mock-11", -2 * ms, -2 * ms}, {"other", -ms, -3500*time.Microsecond - 5*ms}, {"mock-12", -ms / 2, -3500 * time.Microsecond}}, -2 * ms},
	} {
		tr := NewTracker()
		tr.SetAppClock(func(r time.Time) (time.Time, bool) { return r, true })
		for _, x := range row.serves {
			tr.servedIn(burstMock(x.name), b.read.Add(x.at), b.read.Add(x.from), nil, serveKind{})
		}
		s, known := tr.LiveSpanOfSiblings(b.call, burstMock("mock-14"), burstSiblings)
		if want := b.read.Add(row.from); !known || !s.From.Equal(want) {
			t.Errorf("%s: span %v..%v known=%v; want it from %v", row.what, s.From, s.To, known, want)
		}
	}
}
