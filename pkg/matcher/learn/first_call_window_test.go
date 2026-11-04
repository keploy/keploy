package learn

import (
	"strconv"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
)

// A server test's first call, with no data-plane call recorded before it
// (the app made no start-up call; a handshake is no data-plane call), has a
// window: the recording's half starts at the test's recorded begin, since
// the app read its clock for the call after the test's request reached it,
// as this run's half starts at the set's begin where the hooks tell the
// clock's run. A test's own call (OwnCall) and any other call
// (CallWindowFrom) read it alike. A call with no test open, or paired with a
// recording made before the open test's recorded begin, still has none:
// nothing recorded bounds when the app read its clock for it. A previous
// call, where one is recorded, still starts the recording's half.
func TestAServerTestsFirstCallsWindowStartsAtItsRecordedBegin(t *testing.T) {
	began := time.Date(2026, 11, 4, 10, 0, 0, 0, time.UTC)
	setBegan, arrival := began.Add(-time.Second), began.Add(30*time.Millisecond)
	recBegan := began.Add(-time.Hour)
	at := func(name string, d time.Duration) *models.Mock {
		m := &models.Mock{Name: name, Kind: models.MySQL}
		m.Spec.ReqTimestampMock = recBegan.Add(d)
		return m
	}
	own, inTest, earlier := at("own-1", 20*time.Millisecond), at("own-0", 10*time.Millisecond), at("timer-1", -time.Second)
	tests := recordedTests{own.Spec.ReqTimestampMock: 1, earlier.Spec.ReqTimestampMock: 0}
	start := func(open bool) *Tracker {
		tr := NewTracker()
		tr.SetAppClock(func(r time.Time) (time.Time, bool) { return r, true })
		tr.SetAppClockRun(func(time.Time) (time.Time, bool) { return time.Time{}, true })
		tr.SetServeOwnerOf(func(*models.Mock) ServeOwner { return OwnerTest })
		tr.NewSetAt(setBegan)
		tr.RecordedBegin(recBegan)
		tr.BeginAt(began)
		if !open {
			tr.End()
		}
		return tr
	}
	for _, c := range []struct {
		what               string
		window             func() *Window
		recFrom, recTestAt time.Time // zero recFrom: no window
	}{
		{"a test's own first call", func() *Window { return start(true).NewOwnCall(own, nil, tests).Window(arrival) }, recBegan, time.Time{}},
		{"any first call of the test", func() *Window { return start(true).CallWindowFrom(time.Time{}, own, arrival) }, recBegan, time.Time{}},
		{"a first call paired with a recording of an earlier test, from that test's begin", func() *Window {
			return start(true).CallWindowIn(time.Time{}, earlier.Spec.ReqTimestampMock.Add(-50*time.Millisecond), earlier, arrival)
		}, earlier.Spec.ReqTimestampMock.Add(-50 * time.Millisecond), time.Time{}},
		{"a call after a previous call recorded before the test", func() *Window { return start(true).CallWindowFrom(earlier.Spec.ReqTimestampMock, own, arrival) }, earlier.Spec.ReqTimestampMock, time.Time{}},
		{"a call after a previous call recorded in the test", func() *Window { return start(true).CallWindowFrom(inTest.Spec.ReqTimestampMock, own, arrival) }, inTest.Spec.ReqTimestampMock, recBegan},
		{"a call with no test open", func() *Window { return start(false).CallWindowFrom(time.Time{}, own, arrival) }, time.Time{}, time.Time{}},
		{"a call paired with a recording made before the test's recorded begin", func() *Window { return start(true).CallWindowFrom(time.Time{}, earlier, arrival) }, time.Time{}, time.Time{}},
	} {
		w := c.window()
		if c.recFrom.IsZero() {
			if w != nil {
				t.Errorf("%s: window %+v; want none", c.what, w)
			}
			continue
		}
		if w == nil {
			t.Errorf("%s: no window; want the recording's half from %v", c.what, c.recFrom)
			continue
		}
		if !w.Rec.From.Equal(c.recFrom) || !w.Rec.TestFrom.Equal(c.recTestAt) {
			t.Errorf("%s: the recording's half %+v; want from %v, test begin %v", c.what, w.Rec, c.recFrom, c.recTestAt)
		}
		if !w.F.From.Equal(setBegan) || !w.F.To.Equal(arrival) {
			t.Errorf("%s: this run's half %+v; want from the set's begin %v to the arrival %v", c.what, w.F, setBegan, arrival)
		}
	}
}

// The first call's window only narrows what HTTP's learn step learns in a
// server test. With no window (before), a value that is one instant whole
// was learned as a plain read of the clock whatever its instants, checked
// only at the next replay. In the window it is learned only where its
// recorded instant lies in the recording's half (the recorded begin to the
// call) and its live one in this run's (the set's begin to the arrival),
// each at its cut: a DATETIME to the second is held where its second
// overlaps the half. Otherwise it is refused. A read moved by the lead is
// learned either way, and a value that is no instant whole (a time of day,
// an instant inside a longer value) gets the verdict it got with no window.
// So nothing the step refused with no window is learned in it.
func TestAServerTestsFirstCallLearnsAWholeInstantOnlyWhereItsWindowHoldsIt(t *testing.T) {
	began := time.Date(2026, 11, 4, 10, 0, 0, 400e6, time.UTC)
	setBegan, arrival := began.Add(-time.Second), began.Add(30*time.Millisecond)
	recBegan := began.Add(-time.Hour)
	paired := &models.Mock{Name: "own-1", Kind: models.HTTP}
	paired.Spec.ReqTimestampMock = recBegan.Add(20 * time.Millisecond)
	tr := NewTracker()
	tr.SetAppClock(func(r time.Time) (time.Time, bool) { return r, true })
	tr.SetAppClockRun(func(time.Time) (time.Time, bool) { return time.Time{}, true })
	tr.SetServeOwnerOf(func(*models.Mock) ServeOwner { return OwnerTest })
	tr.NewSetAt(setBegan)
	tr.RecordedBegin(recBegan)
	tr.BeginAt(began)
	windows := map[string]*Window{
		"any first call":            tr.CallWindowFrom(time.Time{}, paired, arrival),
		"the test's own first call": tr.NewOwnCall(paired, nil, recordedTests{paired.Spec.ReqTimestampMock: 1}).Window(arrival),
	}
	const p = "body.since"
	shape := func(v string) Shape {
		return Shape{Leaves: map[string]Leaf{p: {Path: p, Noise: p, Type: String, Value: v}}}
	}
	classify := func(r, l string, w *Window) Learned {
		return ClassifyIn(clockedDrifts(), []string{p}, shape(l), shape(r), paired.Spec.ReqTimestampMock, nil, nil, nil, w)[0]
	}
	fine := []struct {
		name string
		of   func(time.Time) string
	}{
		{"RFC 3339 in milliseconds", func(x time.Time) string { return x.Format("2006-01-02T15:04:05.000Z07:00") }},
		{"a DATETIME in microseconds", func(x time.Time) string { return x.Format("2006-01-02 15:04:05.000000") }},
		{"epoch milliseconds", func(x time.Time) string { return strconv.FormatInt(x.UnixMilli(), 10) }},
	}
	seconds := func(x time.Time) string { return x.Format("2006-01-02 15:04:05") }
	rIn, lIn := recBegan.Add(15*time.Millisecond), began.Add(25*time.Millisecond)
	for _, c := range []struct {
		what             string
		rec, live        time.Time
		fine, secondsCut bool // learned in the window: a fine form, and a DATETIME to the second
	}{
		{"read in both halves", rIn, lIn, true, true},
		{"moved by the lead", rIn, rIn.Add(FullRoundLead), true, true},
		{"recorded a millisecond before the recorded begin", recBegan.Add(-time.Millisecond), lIn, false, true},
		{"recorded a second before the recorded begin", recBegan.Add(-time.Second), lIn, false, false},
		{"recorded a millisecond after the call", paired.Spec.ReqTimestampMock.Add(time.Millisecond), lIn, false, true},
		{"read a millisecond before the set's begin", rIn, setBegan.Add(-time.Millisecond), false, true},
		{"read two seconds before the set's begin", rIn, setBegan.Add(-2 * time.Second), false, false},
		{"read a millisecond after the arrival", rIn, arrival.Add(time.Millisecond), false, true},
		{"read two seconds after the arrival", rIn, arrival.Add(2 * time.Second), false, false},
		{"a constant", rIn, time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), false, false},
	} {
		forms := append(append([]struct {
			name string
			of   func(time.Time) string
		}(nil), fine...), struct {
			name string
			of   func(time.Time) string
		}{"a DATETIME to the second", seconds})
		for i, f := range forms {
			want := c.fine
			if i == len(fine) {
				want = c.secondsCut
			}
			r, l := f.of(c.rec), f.of(c.live)
			if ln := classify(r, l, nil); !plainRead(ln) {
				t.Errorf("%s, %s, no window: %s Read=%+v (%s); want a plain read of the clock, as before", c.what, f.name, ln.Kind, ln.Read, ln.Why)
			}
			for name, w := range windows {
				ln := classify(r, l, w)
				if want && !plainRead(ln) {
					t.Errorf("%s, %s, %s: %s Read=%+v (%s); want a plain read of the clock", name, c.what, f.name, ln.Kind, ln.Read, ln.Why)
				}
				if !want && (ln.Kind != KindUnaccounted || ln.Read != nil) {
					t.Errorf("%s, %s, %s: %s Read=%+v (%s); want it refused", name, c.what, f.name, ln.Kind, ln.Read, ln.Why)
				}
			}
		}
	}
	// No instant whole: the verdict the value got with no window.
	for _, v := range [][2]string{
		{"09:00:00", "10:00:00"},
		{"expire > '" + rIn.Format("2006-01-02 15:04:05.000000") + "'", "expire > '" + lIn.Format("2006-01-02 15:04:05.000000") + "'"},
		{"expire > '" + recBegan.Add(-time.Second).Format("2006-01-02 15:04:05.000000") + "'", "expire > '" + lIn.Format("2006-01-02 15:04:05.000000") + "'"},
	} {
		before := classify(v[0], v[1], nil)
		for name, w := range windows {
			if ln := classify(v[0], v[1], w); ln.Kind != before.Kind || (ln.Read == nil) != (before.Read == nil) {
				t.Errorf("%s, %q -> %q: %s Read=%+v in the window; want %s Read=%+v, as with no window", name, v[0], v[1], ln.Kind, ln.Read, before.Kind, before.Read)
			}
		}
	}
}
