package learn

import (
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
