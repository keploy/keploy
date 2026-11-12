package learn

import (
	"reflect"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
)

// A client test's call paired with a recording another test made (a timer's
// call recorded in test A's window, made while test B runs) has no window
// from B's recorded begin, which came after the recording (CallWindowFrom);
// from A's recorded begin, the begin of the test whose window the recording
// lies in, it has one (CallWindowIn): the recording's half starts at the
// later of A's begin and the previous recorded call and carries A's begin,
// this run's half is B's. Given the running test's own recorded begin,
// CallWindowIn is CallWindowFrom; a begin after the recording, or none and
// no earlier recorded call, gives none; and keploy test reads no recorded
// begin, so CallWindowIn is CallWindowFrom there.
func TestTrackerCallWindowInStartsAtTheBeginOfTheTestTheRecordingLiesIn(t *testing.T) {
	aBegan := time.Date(2026, 10, 22, 11, 14, 17, 0, time.UTC)
	paired := &models.Mock{Name: "mock-8", Kind: models.Mongo, Spec: models.MockSpec{ReqTimestampMock: aBegan.Add(155 * time.Millisecond)}}
	bBegan := aBegan.Add(13 * time.Second)
	real := time.Date(2026, 10, 22, 11, 20, 0, 0, time.UTC)
	began, served, arrival := real, real.Add(2*time.Millisecond), real.Add(5*time.Millisecond)
	// B's begin moved the app's clock to B's recorded begin; it runs as one
	// from there.
	offset := bBegan.Sub(began)
	clock := func(r time.Time) (time.Time, bool) { return r.Add(offset), true }
	tracker := func(client bool) *Tracker {
		tr := NewTracker()
		tr.SetClientTests(client)
		tr.SetAppClock(clock)
		tr.RecordedBegin(bBegan)
		tr.BeginAt(began)
		tr.ServedMockAt(&models.Mock{Name: "mock-9", Kind: models.Mongo}, served)
		return tr
	}
	f := Span{From: served.Add(offset), To: arrival.Add(offset), Began: began.Add(offset)}

	tr := tracker(true)
	if w := tr.CallWindowFrom(time.Time{}, paired, arrival); w != nil {
		t.Fatalf("CallWindowFrom %+v; want none: B's recorded begin came after A's recording", w)
	}
	for _, tc := range []struct {
		name    string
		prevRec time.Time
		from    time.Time
	}{
		{"no earlier recorded call", time.Time{}, aBegan},
		{"an earlier test's recorded call", aBegan.Add(-time.Second), aBegan},
		{"an earlier call in A's window", aBegan.Add(100 * time.Millisecond), aBegan.Add(100 * time.Millisecond)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := tr.CallWindowIn(tc.prevRec, aBegan, paired, arrival)
			want := &Window{Rec: Span{From: tc.from, To: paired.Spec.ReqTimestampMock, Began: aBegan}, F: f}
			if !reflect.DeepEqual(w, want) {
				t.Fatalf("window %+v; want %+v", w, want)
			}
		})
	}
	if w := tr.CallWindowIn(time.Time{}, paired.Spec.ReqTimestampMock.Add(time.Millisecond), paired, arrival); w != nil {
		t.Fatalf("window %+v from a begin after the recording; want none", w)
	}
	if w := tr.CallWindowIn(time.Time{}, time.Time{}, paired, arrival); w != nil {
		t.Fatalf("window %+v with no begin and no earlier recorded call; want none", w)
	}
	// The running test's own recording: its own recorded begin is the one
	// CallWindowFrom reads.
	own := &models.Mock{Name: "mock-12", Kind: models.Mongo, Spec: models.MockSpec{ReqTimestampMock: bBegan.Add(40 * time.Millisecond)}}
	if a, b := tr.CallWindowIn(time.Time{}, bBegan, own, arrival), tr.CallWindowFrom(time.Time{}, own, arrival); a == nil || !reflect.DeepEqual(a, b) {
		t.Fatalf("CallWindowIn %+v, CallWindowFrom %+v; want one window", a, b)
	}

	tr = tracker(false)
	prev := aBegan.Add(-time.Second)
	a, b := tr.CallWindowIn(prev, aBegan, paired, arrival), tr.CallWindowFrom(prev, paired, arrival)
	if a == nil || !reflect.DeepEqual(a, b) || !a.Rec.Began.IsZero() || !a.Rec.From.Equal(prev) {
		t.Fatalf("keploy test: CallWindowIn %+v, CallWindowFrom %+v; want one window from the previous recorded call, no begin", a, b)
	}
}
