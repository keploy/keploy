package learn

import (
	"net/http"
	"regexp"
	"strconv"
	"testing"
	"time"
)

// The SQS sample's start-up, as record's full round replayed it: the app
// polled the queue (the receive, its previous data-plane call), read its
// clock, and inserted the event it took with that instant. F's clock runs
// FullRoundLead past the earliest recording from when keploy froze it,
// before the app started, so the insert's instant moved by the lead and by
// however much longer the app took to boot.
var (
	recReceive = time.Date(2026, 10, 20, 15, 49, 55, 270_875_648, time.UTC)
	recInsert  = recReceive.Add(4 * time.Millisecond)
	recRead    = recReceive.Add(3*time.Millisecond + 400*time.Microsecond)
)

// fWindow is the insert's window in a full round whose app took boot longer
// to reach the receive than the recording's.
func fWindow(boot time.Duration) (*Window, time.Time) {
	fReceive := recReceive.Add(FullRoundLead + boot)
	w := &Window{Rec: Span{From: recReceive, To: recInsert}, F: Span{From: fReceive, To: fReceive.Add(5 * time.Millisecond)}}
	return w, fReceive.Add(2*time.Millisecond + 700*time.Microsecond)
}

func millis(t time.Time) string { return strconv.FormatInt(t.UnixMilli(), 10) }

// An instant the app read in its call's window in both runs is a clock read,
// in any form and at its own resolution, however long the boot took; the
// lead alone reads none of them.
func TestAClockReadInItsCallsWindowIsAClockRead(t *testing.T) {
	for _, boot := range []time.Duration{2 * time.Second, 30 * time.Second} {
		w, live := fWindow(boot)
		for _, form := range []struct {
			name string
			of   func(time.Time) string
		}{
			{"epoch milliseconds", millis},
			{"epoch seconds", func(t time.Time) string { return strconv.FormatInt(t.Unix(), 10) }},
			{"RFC 3339 with milliseconds", func(t time.Time) string { return t.Format("2006-01-02T15:04:05.000Z07:00") }},
			{"RFC 3339 in seconds", func(t time.Time) string { return t.Format(time.RFC3339) }},
		} {
			rec, now := form.of(recRead), form.of(live)
			if c := clockIn(rec, now, w); !c.whole {
				t.Errorf("%v boot, %s: %q -> %q is no clock read in the call's window", boot, form.name, rec, now)
			}
			if c := clockOf(rec, now); c.whole {
				t.Errorf("%v boot, %s: the lead alone reads %q -> %q as a clock read", boot, form.name, rec, now)
			}
		}
	}
}

// What is not a read of each run's clock in the call's window is not read
// as one there: a constant, an instant of the recording's era a served
// answer carries, a read made before the call's previous call (in either
// run), or after the call reached keploy.
func TestWhatIsNoReadInTheCallsWindowIsNoClockRead(t *testing.T) {
	w, live := fWindow(2 * time.Second)
	for _, tc := range []struct {
		name      string
		rec, live time.Time
	}{
		{"a constant", recRead, time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)},
		{"an instant a recorded answer carries", recRead, recReceive.Add(-10 * time.Millisecond)},
		{"read before the previous call, recorded", recReceive.Add(-time.Millisecond), live},
		{"read before the previous call, in F", recRead, w.F.From.Add(-time.Millisecond)},
		{"after the call reached keploy, in F", recRead, w.F.To.Add(time.Millisecond)},
		{"after the call reached keploy, recorded", recInsert.Add(time.Millisecond), live},
	} {
		// In epoch milliseconds, and in RFC 3339 with milliseconds: each at
		// its own resolution, not the second's.
		for _, of := range []func(time.Time) string{millis, func(t time.Time) string { return t.Format("2006-01-02T15:04:05.000Z07:00") }} {
			if c := clockIn(of(tc.rec), of(tc.live), w); c.whole {
				t.Errorf("%s: %s -> %s read as a clock read in the call's window", tc.name, of(tc.rec), of(tc.live))
			}
		}
	}
	// A window with no previous call bounds nothing, nor a span with one
	// bound.
	for _, open := range []*Window{nil, {F: w.F}, {Rec: w.Rec}, {Rec: Span{To: w.Rec.To}, F: w.F}, {Rec: w.Rec, F: Span{From: w.F.From}}} {
		if c := clockIn(millis(recRead), millis(live), open); c.whole {
			t.Errorf("window %+v read a clock read", open)
		}
	}
	// A time of day names no day to place in a window.
	if c := clockIn(recRead.Format("15:04:05"), live.Format("15:04:05"), w); c.whole {
		t.Error("a time of day was placed in the window")
	}
}

// The SQS sample's start-up insert end to end, as record's full round met
// it: read by the lead alone (a boot the lead does not cover), the words of
// the insert's instant that changed (its minute and second) are words of the
// receive's Date header, so the instant reads as a value that flowed from
// the receive, and the insert is refused; read in the insert's window it is
// a clock read, and noise. What the window does not hold stays as the lead
// reads it, refused: a constant, an instant read before the receive, and an
// instant a recorded answer carries (the receive's own SentTimestamp,
// recorded, and another recorded answer's in F).
func TestClassifyInTheStartUpInsertsInstantIsNoise(t *testing.T) {
	iso := func(t time.Time) string { return t.Format("2006-01-02T15:04:05.000Z07:00") }
	sent := recReceive.Add(-time.Millisecond).Truncate(time.Millisecond)
	raw := []Raw{{Mock: "mock-0", At: recReceive, Text: "200 Date: " + recReceive.Format(http.TimeFormat) +
		` {"Messages":[{"MessageId":"135dcb85-3c73-4bb1-8569-a5c7092b89e8","Attributes":{"SentTimestamp":"` + iso(sent) + `"},"Body":"Test message 1"}]}`}}
	w, live := fWindow(2 * time.Second)
	classify := func(rec, now time.Time, w *Window) Learned {
		r := body(t, "k", `{"event":"Test message 1","processed_at":"`+iso(rec)+`"}`)
		l := body(t, "k", `{"event":"Test message 1","processed_at":"`+iso(now)+`"}`)
		return ClassifyIn(nil, []string{"body.processed_at"}, l, r, recInsert, nil, raw, nil, w)[0]
	}
	if ln := classify(recRead, live, nil); ln.Kind != KindVariable || ln.From.Mock != "mock-0" {
		t.Fatalf("by the lead alone: %s from %q; want a variable from mock-0, as the full round reads it today", ln.Kind, ln.From.Mock)
	}
	if ln := classify(recRead, live, w); ln.Kind != KindNoise {
		t.Fatalf("in the call's window: %s (%s, from %q); want noise", ln.Kind, ln.Why, ln.From.Mock)
	}
	for _, tc := range []struct {
		name      string
		rec, live time.Time
	}{
		{"a constant", recRead, time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)},
		{"read before the previous call", recRead, w.F.From.Add(-time.Millisecond)},
		{"an instant recorded answers carry", sent, sent.Add(-5 * time.Millisecond)},
	} {
		if ln := classify(tc.rec, tc.live, w); ln.Kind == KindNoise {
			t.Errorf("%s: learned as noise in the call's window", tc.name)
		}
	}
	// A cache keeps one reading of a value per window: two windows that
	// differ in F's span alone read it apart.
	shifted := &Window{Rec: w.Rec, F: Span{From: w.F.From.Add(time.Hour), To: w.F.To.Add(time.Hour)}}
	{
		dc := NewDriftCache()
		r := body(t, "k", `{"processed_at":"`+iso(recRead)+`"}`)
		l := body(t, "k", `{"processed_at":"`+iso(live)+`"}`)
		if ln := ClassifyIn(dc, []string{"body.processed_at"}, l, r, recInsert, nil, raw, nil, w)[0]; ln.Kind != KindNoise {
			t.Fatalf("in the window, cached: %s", ln.Kind)
		}
		if ln := ClassifyIn(dc, []string{"body.processed_at"}, l, r, recInsert, nil, raw, nil, shifted)[0]; ln.Kind == KindNoise {
			t.Fatal("a window whose F span misses the read was answered from another window's cached reading")
		}
	}
	dc := NewDriftCache()
	r := body(t, "k", `{"processed_at":"`+iso(recRead)+`"}`)
	l := body(t, "k", `{"processed_at":"`+iso(live)+`"}`)
	if ln := ClassifyIn(dc, []string{"body.processed_at"}, l, r, recInsert, nil, raw, nil, w)[0]; ln.Kind != KindNoise {
		t.Fatalf("in the window, cached: %s", ln.Kind)
	}
	if ln := ClassifyIn(dc, []string{"body.processed_at"}, l, r, recInsert, nil, raw, nil, nil)[0]; ln.Kind != KindVariable {
		t.Fatalf("by the lead, after the window's reading was cached: %s", ln.Kind)
	}
}

// Holds reads a pair of instants at their resolution.
func TestWindowHolds(t *testing.T) {
	w, live := fWindow(30 * time.Second)
	if !w.Holds(recRead.Truncate(time.Millisecond), live.Truncate(time.Millisecond), time.Millisecond) {
		t.Fatal("a millisecond read in the window is not held")
	}
	if w.Holds(recRead, recRead, time.Millisecond) || (*Window)(nil).Holds(recRead, live, time.Millisecond) {
		t.Fatal("an instant outside F's span, or no window, holds")
	}
	if w.Holds(live, live, time.Millisecond) {
		t.Fatal("a recorded instant outside the recording's span holds")
	}
}

// A word of what changed that the call's answer holds is no echo where it
// is a part of a clock read in the call's window, as where the lead moved
// it; read by the lead alone, the boot's instant is no clock read, and the
// word is refused.
func TestClassifyInAClockWordInTheAnswerIsNoEcho(t *testing.T) {
	w, live := fWindow(2 * time.Second)
	iso := func(t time.Time) string { return t.Format("2006-01-02T15:04:05.000Z07:00") }
	rec := body(t, "k", `{"at":"`+iso(recRead)+`"}`)
	got := body(t, "k", `{"at":"`+iso(live)+`"}`)
	resp := []Leaf{{Path: "body.note", Noise: "body.note", Type: String, Value: "seq " + recRead.Format("05")}}
	if ln := ClassifyIn(nil, []string{"body.at"}, got, rec, recInsert, nil, nil, resp, w)[0]; ln.Kind != KindNoise {
		t.Fatalf("in the call's window: %s (%s); want noise", ln.Kind, ln.Why)
	}
	if ln := ClassifyIn(nil, []string{"body.at"}, got, rec, recInsert, nil, nil, resp, nil)[0]; ln.Kind == KindNoise {
		t.Fatal("by the lead alone: learned as noise")
	}
}

// A zone alternation tries the longest name first, so a name is never read
// as a shorter one it begins with; names of one length come in their order.
func TestAnAlternationPutsTheLongestNameFirst(t *testing.T) {
	if got := alternation([]string{"B", "ES", "EST", "A"}); got != "EST|ES|A|B" {
		t.Fatalf("alternation %q", got)
	}
	if got := regexp.MustCompile(alternation([]string{"ES", "EST"})).FindString("at EST"); got != "EST" {
		t.Fatalf("read %q of EST", got)
	}
}

// Each layout reads an instant of the clock's range alone: [2000, 2100), in
// UTC. One outside it, and a text that is no instant, is no read.
func TestEachLayoutReadsOnlyTheClocksRange(t *testing.T) {
	in := time.Date(2026, 10, 21, 10, 28, 2, 0, time.UTC)
	for _, tc := range []struct {
		name string
		read func(string) (time.Time, time.Duration, bool)
		at   func(time.Time) string
	}{
		{"rfc1123", readRFC1123, func(t time.Time) string { return t.Format(time.RFC1123) }},
		{"rfc1123 with an offset", readRFC1123, func(t time.Time) string { return t.Format(time.RFC1123Z) }},
		{"unixdate", readUnixDate, func(t time.Time) string { return t.Format(time.UnixDate) }},
		{"jsdate", readJSDate, func(t time.Time) string {
			return t.Format("Mon Jan 02 2006 15:04:05 GMT-0700") + " (Coordinated Universal Time)"
		}},
		{"iso8601", readISO, func(t time.Time) string { return t.Format(time.RFC3339) }},
		{"compact", readCompact, func(t time.Time) string { return t.Format("20060102T150405Z") }},
		{"iso8601, its date and time joined by a '+'", readISO, func(t time.Time) string { return t.Format("2006-01-02+15:04:05") }},
	} {
		if got, _, ok := tc.read(tc.at(in)); !ok || !got.Equal(in) {
			t.Errorf("%s: %v %v of %q, want %v", tc.name, got, ok, tc.at(in), in)
		}
		for _, out := range []time.Time{in.AddDate(-30, 0, 0), in.AddDate(100, 0, 0), clockTo} {
			if got, _, ok := tc.read(tc.at(out)); ok {
				t.Errorf("%s: %q, outside the clock's range, reads as %v", tc.name, tc.at(out), got)
			}
		}
		if _, _, ok := tc.read(tc.at(clockFrom)); !ok {
			t.Errorf("%s: the range's first instant does not read", tc.name)
		}
		if got, _, ok := tc.read("not an instant"); ok {
			t.Errorf("%s: a text that is no instant reads as %v", tc.name, got)
		}
	}
}

// An instant that did not change is no clock read, even inside both spans
// of a window that holds it (the spans meet): no read is ever learned of a
// value's unchanged instant.
func TestAnUnchangedInstantIsNoClockRead(t *testing.T) {
	at := time.Date(2026, 10, 21, 10, 28, 2, 0, time.UTC)
	w := &Window{Rec: Span{From: at.Add(-time.Minute), To: at.Add(time.Minute)}, F: Span{From: at.Add(-time.Minute), To: at.Add(time.Minute)}}
	s := "at " + at.Format(time.RFC3339)
	if !w.holds(clockLayouts[layoutNamed("iso8601")], at.Format(time.RFC3339), at.Format(time.RFC3339)) {
		t.Fatal("the window does not hold the instant on both sides")
	}
	if c := clockInBy(s, s+" x", w, true); len(c.pairs) != 0 {
		t.Fatalf("pairs %+v, want none", c.pairs)
	}
}

// The accepted reads are paired in the order they stand in the recorded
// value, whatever their layouts' order: an ISO instant before an HTTP date
// is learned as two instant slots, in their order.
func TestReadsOfTwoLayoutsStandInTheirOrder(t *testing.T) {
	at := time.Date(2026, 10, 21, 10, 28, 2, 0, time.UTC)
	later := at.Add(FullRoundLead)
	rec, live := "at "+at.Format(time.RFC3339)+" or "+at.Format(time.RFC1123), "at "+later.Format(time.RFC3339)+" or "+later.Format(time.RFC1123)
	c := clockOf(rec, live)
	if len(c.pairs) != 2 || c.pairs[0].r.from > c.pairs[1].r.from {
		t.Fatalf("pairs %+v, want two in the recorded value's order", c.pairs)
	}
	e, why := NewDriftCache().Words(rec, live, nil)
	if why != "" || len(e.Slots) != 2 || e.Slots[0].Layout != "iso8601" || e.Slots[1].Layout != "rfc1123" {
		t.Fatalf("words %+v (%s), want an ISO slot then an HTTP date's", e, why)
	}
}
