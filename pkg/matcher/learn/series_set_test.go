package learn

import (
	"errors"
	"reflect"
	"runtime"
	"strconv"
	"strings"
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
// the clock; its recordings come back in the order of their calls. The
// set's first call is read too, in a span with no start (no call before it
// bounds its read), and is the series' first: a test whose miss is closest
// to it varied in the series as much as one closest to a later claim. Not
// read: a heartbeat (protocol phase), a recording with no request time, one
// its reading cannot read, and a kind nobody registered.
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
	claims = append([]*models.Mock{first}, claims...)
	if len(got.Mocks) != len(claims) {
		t.Fatalf("read %d recording(s), want the %d claims, the first among them", len(got.Mocks), len(claims))
	}
	for i, m := range got.Mocks {
		if m != claims[i] {
			t.Fatalf("recording %d is %q, want %q: the claims in the order of their calls", i, m.Name, claims[i].Name)
		}
	}
	rd, err := ReadSetRange(func(visit func(*models.Mock) bool) error {
		for _, m := range recorded {
			visit(m)
		}
		return nil
	}, func(*models.Mock) bool { return true })
	if err != nil || len(rd.Reads) != len(claims) {
		t.Fatalf("ReadSetRange read %d recording(s) (%v), want the %d claims", len(rd.Reads), err, len(claims))
	}
	if sp := rd.Reads[0].Span; !sp.From.IsZero() || !sp.To.Equal(first.Spec.ReqTimestampMock) {
		t.Fatalf("the set's first call is read in %+v; want a span with no start, ending at its request", sp)
	}
	if len(got.Series) != 1 {
		t.Fatalf("got %d series, want the claims' one: %+v", len(got.Series), got.Series)
	}
	s := got.Series[0]
	if _, ok := s.Clock["body.at"]; !ok || len(s.Members) != len(claims) || s.Members[0] != 0 || s.Members[len(s.Members)-1] != len(claims)-1 {
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

// A reader that never holds a set's recordings at once (keploy's agent)
// reads them with SeriesOfRange, one visit at a time and in any order: it
// gets SeriesOfSet's series, the recordings keep keeps as visited, and every
// other as a stand-in holding only its name, kind and times, never one to
// answer with. A reader's error comes back with no series.
func TestSeriesOfRangeIsSeriesOfSetKeepingOnlyWhatItIsAskedTo(t *testing.T) {
	defer RegisterSeriesShape(seriesKind, readSeriesRow)()
	base := time.Date(2026, 10, 29, 10, 0, 0, 0, time.UTC)
	var recorded []*models.Mock
	for i := 0; i <= 6; i++ {
		at := base.Add(time.Duration(2*i)*time.Second + 500*time.Millisecond)
		recorded = append(recorded,
			rowMock("http-"+strconv.Itoa(i), models.HTTP, at.Add(-700*time.Millisecond), at.Add(-600*time.Millisecond), nil),
			rowMock("claim-"+strconv.Itoa(i), seriesKind, at, at.Add(3*time.Millisecond),
				map[string]string{"op": "claim", "at": strconv.FormatInt(at.Add(-time.Millisecond).Unix(), 10)}))
	}
	want := SeriesOfSet(recorded, nil)
	if len(want.Series) != 1 || len(want.Mocks) != 7 {
		t.Fatalf("the fixture's set reads %d series of %d recording(s); want one of 7", len(want.Series), len(want.Mocks))
	}
	kept := map[string]bool{"claim-2": true, "claim-6": true}
	got, err := SeriesOfRange(func(visit func(*models.Mock) bool) error {
		for i := len(recorded) - 1; i >= 0; i-- { // newest first: any order
			if !visit(recorded[i]) {
				break
			}
		}
		return nil
	}, func(m *models.Mock) bool { return kept[m.Name] }, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Series) != 1 || !reflect.DeepEqual(got.Series[0], want.Series[0]) {
		t.Fatalf("SeriesOfRange read %+v; want SeriesOfSet's %+v", got.Series, want.Series)
	}
	for i, m := range got.Mocks {
		w := want.Mocks[i]
		if m.Name != w.Name || m.Kind != w.Kind || !m.Spec.ReqTimestampMock.Equal(w.Spec.ReqTimestampMock) || !m.Spec.ResTimestampMock.Equal(w.Spec.ResTimestampMock) {
			t.Fatalf("recording %d is %q at %v; want %q at %v", i, m.Name, m.Spec.ReqTimestampMock, w.Name, w.Spec.ReqTimestampMock)
		}
		if kept[m.Name] != (m == w) {
			t.Errorf("recording %q kept as visited: %v; want %v", m.Name, m == w, kept[m.Name])
		}
		if !kept[m.Name] && m.Spec.Metadata != nil {
			t.Errorf("stand-in %q carries the recording's request (%v)", m.Name, m.Spec.Metadata)
		}
	}
	boom := errors.New("store closed")
	if got, err := SeriesOfRange(func(func(*models.Mock) bool) error { return boom },
		func(*models.Mock) bool { return true }, nil); !errors.Is(err, boom) || got.Series != nil || got.Mocks != nil {
		t.Fatalf("a failed read returned %+v, %v; want no series and its error", got, err)
	}
}

// liveHeap is the heap's live bytes, collected first.
func liveHeap() uint64 {
	runtime.GC()
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	return ms.HeapAlloc
}

// SeriesOfRange holds in memory, while it reads, only the requests of the
// operations a recording it keeps has (kind, key and fixed parts): a set's
// other calls, however many and however large, are never read into memory
// together (founder rule: no component OOMs). A store of 256 writes of 64 KiB
// each beside a timer's claims, only the claims kept: the live heap while it
// reads stays under a quarter of the writes' size, and the claims' series is
// what SeriesOfSet reads of the claims alone.
func TestSeriesOfRangeHoldsNoRequestOfACallNoKeptOneShares(t *testing.T) {
	const writes, size = 256, 64 << 10
	defer RegisterSeriesShape(seriesKind, func(m *models.Mock) (Shape, bool) {
		s, ok := readSeriesRow(m)
		if ok && m.Spec.Metadata["big"] != "" {
			s.Leaves["body.big"] = Leaf{Path: "body.big", Noise: "body.big", Type: String, Value: m.Spec.Metadata["big"]}
		}
		return s, ok
	})()
	base := time.Date(2026, 10, 29, 10, 0, 0, 0, time.UTC)
	var claims []*models.Mock
	for i := 0; i <= 6; i++ {
		at := base.Add(time.Duration(2*i)*time.Second + 500*time.Millisecond)
		claims = append(claims,
			rowMock("http-"+strconv.Itoa(i), models.HTTP, at.Add(-700*time.Millisecond), at.Add(-600*time.Millisecond), nil),
			rowMock("claim-"+strconv.Itoa(i), seriesKind, at, at.Add(3*time.Millisecond),
				map[string]string{"op": "claim", "at": strconv.FormatInt(at.Add(-time.Millisecond).Unix(), 10)}))
	}
	want := SeriesOfSet(claims, nil)
	before := liveHeap()
	var peak uint64
	got, err := SeriesOfRange(func(visit func(*models.Mock) bool) error {
		for _, m := range claims {
			if !visit(m) {
				return nil
			}
		}
		for i := 0; i < writes; i++ {
			at := base.Add(20*time.Second + time.Duration(i)*37*time.Millisecond) // after the claims: their spans stay
			// Each write is decoded as it is visited, as the on-disk store
			// does: the range holds none of them.
			if !visit(rowMock("write-"+strconv.Itoa(i), seriesKind, at, at.Add(time.Millisecond),
				map[string]string{"op": "write-" + strconv.Itoa(i), "at": "7", "big": strings.Repeat(string(rune('a'+i%26)), size)})) {
				return nil
			}
		}
		if h := liveHeap(); h > peak {
			peak = h
		}
		return nil
	}, func(m *models.Mock) bool { return strings.HasPrefix(m.Name, "claim-") }, nil)
	if err != nil {
		t.Fatal(err)
	}
	if held := int64(peak) - int64(before); held > writes*size/4 {
		t.Fatalf("SeriesOfRange held %d KiB live while it read %d writes of %d KiB none it keeps shares an operation with; want under %d KiB",
			held>>10, writes, size>>10, writes*size/4>>10)
	}
	if len(got.Series) != 1 || !reflect.DeepEqual(got.Series, want.Series) {
		t.Fatalf("SeriesOfRange read %d series %+v; want the claims' one, SeriesOfSet's of the claims %+v", len(got.Series), got.Series, want.Series)
	}
	for _, m := range got.Mocks {
		if !strings.HasPrefix(m.Name, "claim-") {
			t.Errorf("SeriesOfRange read %q, which shares no operation with a recording it keeps", m.Name)
		}
	}
}

// Recordings of two kinds are never one series, even where their readings
// give them one key and one shape: a call of one parser is never answered
// from another's recording, and judging never learns one series across them.
func TestSeriesOfSetNeverReadsTwoKindsIntoOneSeries(t *testing.T) {
	const otherKind models.Kind = "SeriesRowOtherKind"
	defer RegisterSeriesShape(seriesKind, readSeriesRow)()
	defer RegisterSeriesShape(otherKind, readSeriesRow)()
	base := time.Date(2026, 10, 29, 10, 0, 0, 0, time.UTC)
	var recorded []*models.Mock
	for i := 0; i <= 6; i++ {
		at := base.Add(time.Duration(2*i)*time.Second + 500*time.Millisecond)
		kind := seriesKind
		if i%2 == 1 {
			kind = otherKind
		}
		recorded = append(recorded,
			rowMock("http-"+strconv.Itoa(i), models.HTTP, at.Add(-700*time.Millisecond), at.Add(-600*time.Millisecond), nil),
			rowMock("claim-"+strconv.Itoa(i), kind, at, at.Add(3*time.Millisecond),
				map[string]string{"op": "claim", "at": strconv.FormatInt(at.Add(-time.Millisecond).Unix(), 10)}))
	}
	got := SeriesOfSet(recorded, nil)
	for _, s := range got.Series {
		kinds := map[models.Kind]bool{}
		for _, i := range s.Members {
			kinds[got.Mocks[i].Kind] = true
		}
		if len(kinds) != 1 {
			t.Errorf("a series %q holds recordings of %d kinds: %v", s.Key, len(kinds), kinds)
		}
	}
	if len(got.Series) != 2 {
		t.Fatalf("got %d series; want one of each kind's claims", len(got.Series))
	}
}

// A set's first call whose epoch leaf the claims' offset would place after
// the call itself is no claim of theirs: it is a series of its own, and the
// claims after it still read their leaf as the clock, every one a member.
// It never unlearns theirs.
func TestSeriesOfSetSplitsOffAFirstCallThatIsNoReadOfTheirClock(t *testing.T) {
	defer RegisterSeriesShape(seriesKind, readSeriesRow)()
	base := time.Date(2026, 10, 29, 10, 0, 0, 0, time.UTC)
	claim := func(name string, at time.Time, v time.Time) *models.Mock {
		return rowMock(name, seriesKind, at, at.Add(3*time.Millisecond),
			map[string]string{"op": "claim", "at": strconv.FormatInt(v.Unix(), 10)})
	}
	first := base.Add(500 * time.Millisecond)
	recorded := []*models.Mock{claim("first", first, first.Add(time.Hour))}
	for i := 1; i <= 5; i++ {
		at := base.Add(time.Duration(2*i)*time.Second + 500*time.Millisecond)
		recorded = append(recorded, claim("claim-"+strconv.Itoa(i), at, at.Add(-time.Millisecond)))
	}
	got := SeriesOfSet(recorded, nil)
	if len(got.Mocks) != len(recorded) || len(got.Series) != 2 {
		t.Fatalf("read %d recording(s) into %d series, want all %d into two: %+v", len(got.Mocks), len(got.Series), len(recorded), got.Series)
	}
	for _, s := range got.Series {
		switch {
		case len(s.Members) == 1 && got.Mocks[s.Members[0]].Name == "first":
			if _, clock := s.Clock["body.at"]; clock {
				t.Errorf("the first call alone reads its leaf as the clock: %+v", s)
			}
		case len(s.Members) == len(recorded)-1:
			if _, clock := s.Clock["body.at"]; !clock || got.Mocks[s.Members[0]].Name != "claim-1" {
				t.Errorf("the claims' series is %+v; want body.at a read of the clock, from claim-1", s)
			}
		default:
			t.Errorf("a series of %d member(s) from %q: want the first call alone and the five claims", len(s.Members), got.Mocks[s.Members[0]].Name)
		}
	}
}

// The first call is split off only when the calls a call before bounds read
// the clock without it: in a series whose number tracks no clock (two values
// taken in turn, a value of the call), the first call is a recording of its
// value's call like any other, never a series of its own.
func TestSeriesOfSetKeepsAFirstCallInItsValuesSeriesWhenNoneReadsTheClock(t *testing.T) {
	defer RegisterSeriesShape(seriesKind, readSeriesRow)()
	base := time.Date(2026, 10, 29, 10, 0, 0, 0, time.UTC)
	const even, odd = "1793311200", "1793314800"
	var recorded []*models.Mock
	for i := 0; i <= 5; i++ {
		at := base.Add(time.Duration(2*i)*time.Second + 500*time.Millisecond)
		v := even
		if i%2 == 1 {
			v = odd
		}
		recorded = append(recorded, rowMock("claim-"+strconv.Itoa(i), seriesKind, at, at.Add(3*time.Millisecond),
			map[string]string{"op": "claim", "at": v}))
	}
	got := SeriesOfSet(recorded, nil)
	if len(got.Mocks) != len(recorded) || len(got.Series) != 2 {
		t.Fatalf("read %d recording(s) into %d series, want all %d into two (one per value): %+v", len(got.Mocks), len(got.Series), len(recorded), got.Series)
	}
	for _, s := range got.Series {
		if _, clock := s.Clock["body.at"]; clock {
			t.Errorf("a series reads body.at as the clock: %+v", s)
		}
		if len(s.Members) != 3 {
			t.Errorf("a series of %d member(s) from %q: want three per value, the first call (claim-0) in the even one", len(s.Members), got.Mocks[s.Members[0]].Name)
		}
	}
	if s := got.Series[0]; len(s.Members) == 0 || got.Mocks[s.Members[0]].Name != "claim-0" {
		t.Errorf("the first series does not begin with the first call: %+v", got.Series)
	}
}
