package learn

import (
	"strconv"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
)

// seriesKind is a kind no parser registers: the rows register its reading.
const seriesKind models.Kind = "SeriesRowKind"

// readSeriesRow is seriesKind's reading: the operation in the metadata's
// "op", one untyped number "at" (an epoch second) and one value "v".
func readSeriesRow(m *models.Mock) (Shape, bool) {
	op := m.Spec.Metadata["op"]
	if op == "" {
		return Shape{}, false
	}
	return Shape{Key: op, Leaves: map[string]Leaf{
		"body.at": {Path: "body.at", Noise: "body.at", Type: Number, Value: m.Spec.Metadata["at"]},
		"body.v":  {Path: "body.v", Noise: "body.v", Type: String, Value: "x"},
	}}, true
}

// rowMock is a recording of kind sent at req and answered at res (zero:
// not known), named name, with metadata md.
func rowMock(name string, kind models.Kind, req, res time.Time, md map[string]string) *models.Mock {
	m := &models.Mock{Name: name, Kind: kind}
	m.Spec.ReqTimestampMock, m.Spec.ResTimestampMock, m.Spec.Metadata = req, res, md
	return m
}

func TestRegisterSeriesShapeIsOneReadingPerKind(t *testing.T) {
	if _, ok := SeriesShapeOf(rowMock("m", seriesKind, time.Time{}, time.Time{}, map[string]string{"op": "find"})); ok {
		t.Fatal("a kind nobody registered was read")
	}
	if _, ok := SeriesShapeOf(nil); ok {
		t.Fatal("a nil recording was read")
	}
	mustPanic := func(what string, f func()) {
		t.Helper()
		defer func() {
			if recover() == nil {
				t.Errorf("%s did not panic", what)
			}
		}()
		f()
	}
	mustPanic("a nil reading", func() { RegisterSeriesShape(seriesKind, nil) })
	unregister := RegisterSeriesShape(seriesKind, readSeriesRow)
	mustPanic("a second reading of the kind", func() { RegisterSeriesShape(seriesKind, readSeriesRow) })
	if s, ok := SeriesShapeOf(rowMock("m", seriesKind, time.Time{}, time.Time{}, map[string]string{"op": "find"})); !ok || s.Key != "find" {
		t.Fatalf("the registered kind read %+v (%v); want its reading's", s, ok)
	}
	if _, ok := SeriesShapeOf(rowMock("m", seriesKind, time.Time{}, time.Time{}, nil)); ok {
		t.Fatal("a recording its reading cannot read was read")
	}
	if _, ok := SeriesShapeOf(rowMock("m", models.Mongo, time.Time{}, time.Time{}, map[string]string{"op": "find"})); ok {
		t.Fatal("another kind was read by this kind's reading")
	}
	unregister()
	if _, ok := SeriesShapeOf(rowMock("m", seriesKind, time.Time{}, time.Time{}, map[string]string{"op": "find"})); ok {
		t.Fatal("the reading was still registered after its removal")
	}
	defer RegisterSeriesShape(seriesKind, readSeriesRow)() // registers again after a removal
}

// A set's recordings, given in no order: a timer's claim every 2 s between
// the application's other calls is read in its calls' spans, from each
// one's previous data-plane call, into one series whose epoch leaf reads
// the clock; its recordings come back in the order of their calls. Not
// read: the set's first call (no previous call bounds its read), a
// heartbeat (protocol phase), a recording with no request time, one its
// reading cannot read, and a kind nobody registered.
func TestSeriesOfSetReadsAKindsRecordingsInTheirCallsSpans(t *testing.T) {
	defer RegisterSeriesShape(seriesKind, readSeriesRow)()
	base := time.Date(2026, 10, 29, 10, 0, 0, 0, time.UTC)
	claim := func(name string, at time.Time) *models.Mock {
		return rowMock(name, seriesKind, at, at.Add(3*time.Millisecond),
			map[string]string{"op": "claim", "at": strconv.FormatInt(at.Add(-time.Millisecond).Unix(), 10)})
	}
	first := claim("first", base.Add(500*time.Millisecond))
	var claims []*models.Mock
	recorded := []*models.Mock{first}
	for i := 1; i <= 5; i++ {
		at := base.Add(time.Duration(2*i)*time.Second + 500*time.Millisecond)
		claims = append(claims, claim("claim-"+strconv.Itoa(i), at))
		recorded = append(recorded, rowMock("http-"+strconv.Itoa(i), models.HTTP, at.Add(-700*time.Millisecond), at.Add(-600*time.Millisecond), nil))
	}
	heartbeat := rowMock("hello", seriesKind, base.Add(4900*time.Millisecond), time.Time{}, map[string]string{"type": "config", "op": "claim", "at": "1"})
	untimed := rowMock("untimed", seriesKind, time.Time{}, time.Time{}, map[string]string{"op": "claim", "at": "2"})
	unread := rowMock("unread", seriesKind, base.Add(7*time.Second), time.Time{}, nil)
	// Reversed: the series is of the calls' order, not the store's.
	for i := len(claims) - 1; i >= 0; i-- {
		recorded = append(recorded, claims[i])
	}
	recorded = append(recorded, heartbeat, untimed, unread, nil)

	got := SeriesOfSet(recorded, nil)
	if len(got.Mocks) != len(claims) {
		t.Fatalf("read %d recording(s), want the %d claims after the first", len(got.Mocks), len(claims))
	}
	for i, m := range got.Mocks {
		if m != claims[i] {
			t.Fatalf("recording %d is %q, want %q: the claims in the order of their calls", i, m.Name, claims[i].Name)
		}
	}
	if len(got.Series) != 1 {
		t.Fatalf("got %d series, want the claims' one: %+v", len(got.Series), got.Series)
	}
	s := got.Series[0]
	if _, ok := s.Clock["body.at"]; !ok || len(s.Members) != len(claims) || s.Members[len(s.Members)-1] != len(claims)-1 {
		t.Fatalf("the claims' series is %+v; want body.at a read of the clock, every claim a member, the newest last", s)
	}
	// Each claim's span starts at the application's call just before it
	// (700 ms before), not at the claim before (2 s before): so the offset
	// learned is the narrow one.
	if off := s.Clock["body.at"].Offset; off.Hi-off.Lo > time.Second {
		t.Errorf("the offset learned is %+v; want it bounded by each claim's previous call", off)
	}
}

// previousCalls is PreviousCall for every recording at once: on a set with
// heartbeats, a call pending across another's send, calls whose answer time
// is not known, two sent at one instant, one with no request time and a nil,
// each recording's previous call is PreviousCall's.
func TestPreviousCallsIsPreviousCall(t *testing.T) {
	base := time.Date(2026, 10, 29, 10, 0, 0, 0, time.UTC)
	ms := func(n int) time.Time { return base.Add(time.Duration(n) * time.Millisecond) }
	set := []*models.Mock{
		rowMock("a", models.HTTP, ms(10), ms(20), nil),
		rowMock("dns", models.DNS, ms(30), ms(31), nil),
		rowMock("pending", models.HTTP, ms(25), ms(200), nil),
		rowMock("b", models.Mongo, ms(40), time.Time{}, nil),
		rowMock("c1", models.HTTP, ms(60), ms(61), nil),
		rowMock("c2", models.HTTP, ms(60), ms(62), nil),
		rowMock("hello", models.Mongo, ms(70), ms(71), map[string]string{"type": "config"}),
		rowMock("d", models.HTTP, ms(80), ms(90), nil),
		rowMock("untimed", models.HTTP, time.Time{}, time.Time{}, nil),
		nil,
		rowMock("e", models.HTTP, ms(250), ms(260), nil),
		rowMock("first", models.HTTP, ms(5), ms(6), nil),
	}
	prev := previousCalls(set)
	for _, paired := range set {
		want := NewPreviousCall(paired)
		for _, m := range set {
			want.See(m)
		}
		if got := prev(paired); !got.Equal(want.At()) {
			name := "<nil>"
			if paired != nil {
				name = paired.Name
			}
			t.Errorf("%s: previous call at %v, want PreviousCall's %v", name, got, want.At())
		}
	}
	// Spot checks, so the row is no comparison of two equal mistakes.
	byName := map[string]*models.Mock{}
	for _, m := range set {
		if m != nil {
			byName[m.Name] = m
		}
	}
	for name, want := range map[string]time.Time{
		"first": {}, "a": ms(5), "b": ms(10), "c1": ms(40), "d": ms(60), "e": ms(80), "hello": ms(60),
	} {
		if got := prev(byName[name]); !got.Equal(want) {
			t.Errorf("%s: previous call at %v, want %v", name, got, want)
		}
	}
}

// HasShape is Holds but for when the reads of the clock were made: a claim
// whose clock read is off the run's clock, or that has no span, is still of
// the series' shape (a call of it the series cannot answer), while one with
// a value changed, a leaf more or less, a read of the clock written as
// another type, or another operation is not.
func TestSeriesHasShapeIsHoldsButForTheClock(t *testing.T) {
	base := time.Date(2026, 10, 29, 10, 0, 0, 0, time.UTC)
	ss := SeriesOf(pollSeries(base, 6), nil)
	if len(ss) != 1 {
		t.Fatalf("got %d series, want the claim's one", len(ss))
	}
	s := ss[0]
	at := base.Add(24 * time.Hour)
	onClock, span := livePoll(at, 0)
	offClock, _ := livePoll(at, time.Hour)
	if !s.Holds(onClock, span, nil) || !s.HasShape(onClock, nil) {
		t.Fatal("a claim on the run's clock does not hold")
	}
	if s.Holds(offClock, span, nil) || !s.HasShape(offClock, nil) {
		t.Error("a claim off the run's clock: want it of the series' shape, never held")
	}
	if s.Holds(onClock, Span{}, nil) || !s.HasShape(onClock, nil) {
		t.Error("a claim with no span: want it of the series' shape, never held")
	}
	change := func(f func(*Shape)) Shape {
		c, _ := livePoll(at, 0)
		f(&c)
		return c
	}
	for name, live := range map[string]Shape{
		"a value changed": change(func(c *Shape) {
			l := c.Leaves["body.cmd.new"]
			l.Value = "false"
			c.Leaves["body.cmd.new"] = l
		}),
		"a leaf more": change(func(c *Shape) { c.Leaves["body.cmd.upsert"] = Leaf{Path: "body.cmd.upsert", Type: Bool, Value: "true"} }),
		"a leaf less": change(func(c *Shape) { delete(c.Leaves, "body.cmd.new") }),
		"a clock read written as text": change(func(c *Shape) {
			l := c.Leaves["body.cmd.update.$set.takenAt"]
			l.Type = String
			c.Leaves["body.cmd.update.$set.takenAt"] = l
		}),
		"a clock read that reads no clock": change(func(c *Shape) {
			l := c.Leaves["body.cmd.update.$set.takenAt"]
			l.Value = "1.5"
			c.Leaves["body.cmd.update.$set.takenAt"] = l
		}),
		"another operation": change(func(c *Shape) { c.Key = "2013\x00update\x00jobs\x00app" }),
	} {
		if s.HasShape(live, nil) || s.Holds(live, span, nil) {
			t.Errorf("%s: want it of no shape of the series", name)
		}
	}
}
