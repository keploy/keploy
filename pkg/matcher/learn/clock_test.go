package learn

import (
	"net/http"
	"strconv"
	"strings"
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
			if c := clockIn(rec, now, w, true); !c.whole {
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
			if c := clockIn(of(tc.rec), of(tc.live), w, true); c.whole {
				t.Errorf("%s: %s -> %s read as a clock read in the call's window", tc.name, of(tc.rec), of(tc.live))
			}
		}
	}
	// A window with no previous call bounds nothing, nor a span with one
	// bound.
	for _, open := range []*Window{nil, {F: w.F}, {Rec: w.Rec}, {Rec: Span{To: w.Rec.To}, F: w.F}, {Rec: w.Rec, F: Span{From: w.F.From}}} {
		if c := clockIn(millis(recRead), millis(live), open, true); c.whole {
			t.Errorf("window %+v read a clock read", open)
		}
	}
	// A time of day names no day to place in a window.
	if c := clockIn(recRead.Format("15:04:05"), live.Format("15:04:05"), w, true); c.whole {
		t.Error("a time of day was placed in the window")
	}
}

// The SQS sample's start-up insert end to end, as record's full round met
// it. Its processed_at is noise only where its provenance shows it is a
// clock read: read in the insert's window. Read by the lead alone (a boot the
// lead does not cover), nothing shows it is one, and it is read word by word
// as any value is, whether it is the value whole or stands inside a longer
// one: its minute and second are words of the receive's Date header, a
// value that may have flowed from the receive, refused (a false red that
// fails closed, naming the receive). What its text looks like decides
// nothing. A constant, and a read before the receive, are no clock read in
// the window either; and an instant recorded answers carry is followed,
// whatever the window.
func TestClassifyInTheStartUpInsertsInstantIsNoiseInItsWindow(t *testing.T) {
	iso := func(t time.Time) string { return t.Format("2006-01-02T15:04:05.000Z07:00") }
	inText := func(t time.Time) string { return "at " + iso(t) + " by w-1" }
	sent := recReceive.Add(-time.Millisecond).Truncate(time.Millisecond)
	raw := []Raw{{rRead: true, mock: "mock-0", at: recReceive, rText: "200 Date: " + recReceive.Format(http.TimeFormat) +
		` {"Messages":[{"MessageId":"135dcb85-3c73-4bb1-8569-a5c7092b89e8","Attributes":{"SentTimestamp":"` + iso(sent) + `"},"Body":"Test message 1"}]}`}}
	w, live := fWindow(2 * time.Second)
	classify := func(form func(time.Time) string, rec, now time.Time, w *Window) Learned {
		r := body(t, "k", `{"event":"Test message 1","processed_at":"`+form(rec)+`"}`)
		l := body(t, "k", `{"event":"Test message 1","processed_at":"`+form(now)+`"}`)
		return ClassifyIn(nil, []string{"body.processed_at"}, l, r, recInsert, nil, raw, nil, w)[0]
	}
	for fname, form := range map[string]func(time.Time) string{"the value whole": iso, "inside a longer value": inText} {
		for _, tc := range []struct {
			name      string
			rec, live time.Time
			// noiseIn: noise in the call's window; elsewhere, and by the
			// lead alone, a variable from the receive.
			noiseIn bool
		}{
			{"the insert's clock read", recRead, live, true},
			{"a constant", recRead, time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), false},
			{"read before the previous call", recRead, w.F.From.Add(-time.Millisecond), false},
			{"an instant recorded answers carry", sent, sent.Add(-5 * time.Millisecond), false},
		} {
			for wname, win := range map[string]*Window{"in the call's window": w, "by the lead alone": nil} {
				ln := classify(form, tc.rec, tc.live, win)
				if tc.noiseIn && win != nil {
					if !noiseOrRead(ln) {
						t.Errorf("%s, %s, %s: %s (%s, from %q); want noise", fname, tc.name, wname, ln.Kind, ln.Why, ln.From.Mock)
					}
				} else if ln.Kind != KindVariable || ln.From.Mock != "mock-0" {
					t.Errorf("%s, %s, %s: %s (%s, from %q); want a variable from mock-0", fname, tc.name, wname, ln.Kind, ln.Why, ln.From.Mock)
				}
			}
		}
	}
	// A cache keeps one reading of a value per window: two windows that
	// differ in F's span alone read it apart, and so does none.
	shifted := &Window{Rec: w.Rec, F: Span{From: w.F.From.Add(time.Hour), To: w.F.To.Add(time.Hour)}}
	{
		dc := clockedDrifts()
		r := body(t, "k", `{"processed_at":"`+inText(recRead)+`"}`)
		l := body(t, "k", `{"processed_at":"`+inText(live)+`"}`)
		if ln := ClassifyIn(dc, []string{"body.processed_at"}, l, r, recInsert, nil, raw, nil, w)[0]; ln.Kind != KindNoise {
			t.Fatalf("in the window, cached: %s", ln.Kind)
		}
		if ln := ClassifyIn(dc, []string{"body.processed_at"}, l, r, recInsert, nil, raw, nil, shifted)[0]; ln.Kind == KindNoise {
			t.Fatal("a window whose F span misses the read was answered from another window's cached reading")
		}
	}
	dc := clockedDrifts()
	r := body(t, "k", `{"processed_at":"`+inText(recRead)+`"}`)
	l := body(t, "k", `{"processed_at":"`+inText(live)+`"}`)
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
// it; read by the lead alone, the boot's instant is no clock read, the value
// whole or inside a longer one alike, and the word is refused. A word of the
// value outside every instant is an echo (an id beside the stamp).
func TestClassifyInAClockWordInTheAnswerIsNoEcho(t *testing.T) {
	w, live := fWindow(2 * time.Second)
	iso := func(t time.Time) string { return t.Format("2006-01-02T15:04:05.000Z07:00") }
	resp := []Leaf{{Path: "body.note", Noise: "body.note", Type: String, Value: "seq " + recRead.Format("05") + " of o-41"}}
	for _, form := range []struct {
		of func(time.Time) string
		// whole: the value is one instant whole, a plain read of the
		// clock (KindClockRead), not a word of a longer value (noise).
		whole bool
	}{{iso, true}, {func(t time.Time) string { return "at " + iso(t) }, false}} {
		rec := body(t, "k", `{"at":"`+form.of(recRead)+`"}`)
		got := body(t, "k", `{"at":"`+form.of(live)+`"}`)
		for wname, win := range map[string]*Window{"in the call's window": w, "by the lead alone": nil} {
			want := KindNoise
			switch {
			case win == nil:
				want = KindUnaccounted
			case form.whole:
				want = KindClockRead
			}
			if ln := ClassifyIn(nil, []string{"body.at"}, got, rec, recInsert, nil, nil, resp, win)[0]; ln.Kind != want {
				t.Errorf("%q, %s: %s (%s); want %s", form.of(recRead), wname, ln.Kind, ln.Why, want)
			}
		}
	}
	rec := body(t, "k", `{"at":"o-41 at `+iso(recRead)+`"}`)
	got := body(t, "k", `{"at":"o-42 at `+iso(live)+`"}`)
	if ln := ClassifyIn(nil, []string{"body.at"}, got, rec, recInsert, nil, nil, resp, w)[0]; ln.Kind != KindUnaccounted || !strings.Contains(ln.Why, "comes back in the answer") {
		t.Fatalf("an id beside the stamp, which the answer holds: %s (%s); want refused", ln.Kind, ln.Why)
	}
}
