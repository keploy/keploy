package learn

import (
	"fmt"
	"math/rand"
	"net/http"
	"reflect"
	"regexp/syntax"
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

// clockWordInWhole is clockWordIn as it read both whole texts with every
// layout, before it read only where they differ: the outcome clockWordIn
// must keep (TestAClockWordIsFoundWhereTwoTextsDifferAsInTheWholeTexts).
func clockWordInWhole(rec, live string, w *Window) (before, rw, lw, after string, ok bool) {
	if w == nil {
		return "", "", "", "", false
	}
	matches := func(lay clockLayout, s string, taken []bool) [][]int {
		var out [][]int
		for _, m := range lay.re.FindAllStringIndex(s, -1) {
			if m[0] > 0 && isWord(s[m[0]-1]) && isWord(s[m[0]]) || m[1] < len(s) && isWord(s[m[1]-1]) && isWord(s[m[1]]) {
				continue
			}
			free := true
			for i := m[0]; i < m[1]; i++ {
				if taken[i] {
					free = false
					break
				}
			}
			if free {
				out = append(out, m)
			}
		}
		return out
	}
	takenR, takenL := make([]bool, len(rec)), make([]bool, len(live))
	for _, lay := range clockLayouts {
		rm, lm := matches(lay, rec, takenR), matches(lay, live, takenL)
		for j := 0; j < len(rm) && j < len(lm); j++ {
			rs, ls := rec[rm[j][0]:rm[j][1]], live[lm[j][0]:lm[j][1]]
			if rs == ls {
				continue
			}
			for i := rm[j][0]; i < rm[j][1]; i++ {
				takenR[i] = true
			}
			for i := lm[j][0]; i < lm[j][1]; i++ {
				takenL[i] = true
			}
			if ok {
				return "", "", "", "", false
			}
			before, rw, lw, after = rec[:rm[j][0]], rs, ls, rec[rm[j][1]:]
			if live[:lm[j][0]] != before || live[lm[j][1]:] != after || !w.holds(lay, rs, ls) {
				return "", "", "", "", false
			}
			ok = true
		}
	}
	return before, rw, lw, after, ok
}

// anyInstant is a window every instant 1970-2200 is a read in, so that
// whether clockWordIn finds a word turns on the texts alone.
var anyInstant = &Window{
	Rec: Span{From: time.Unix(0, 0), To: time.Date(2200, 1, 1, 0, 0, 0, 0, time.UTC)},
	F:   Span{From: time.Unix(0, 0), To: time.Date(2200, 1, 1, 0, 0, 0, 0, time.UTC)},
}

// clockFragments are the pieces the differential row builds texts of: an
// instant of each layout, the same with one byte changed (a letter of a
// UUID, the weekday or the zone of an RFC 1123 date), runs that chain
// instants across a place (times of day joined by colons, long runs of
// digits), separators, and bytes no layout holds.
var clockFragments = []string{
	"1793336961", "1793336962", "1793336961102639022", "1793336961102639023", "1793336961102", "1793336961.123", "1793336961.124",
	"2026-10-30", "2026-10-31", "2026-10-30T08:00:00Z", "2026-10-30T08:00:01Z", "2026-10-30 08:00:00.123+05:30", "2026-10-30T10Z", "2026-10-30 UTC",
	"20261030T080000Z", "20261030T080001Z",
	"0192ab3c-1d2e-7f00-8abc-0123456789ab", "0192ab3d-1d2e-7f00-8abc-0123456789ab", "6ba7b810-9dad-11d1-80b4-00c04fd430c8", "6ba7b810-9dad-11d1-80b4-00c04fd430c9",
	"Fri, 30 Oct 2026 08:00:00 GMT", "Sat, 30 Oct 2026 08:00:00 GMT", "Fri, 30 Oct 2026 08:00:00 UTC", "Fri, 30 Oct 2026 08:00:00 +0000",
	"08:00", "08:00:00", "08:00:00.5", "08:01:00", "12:34:56:78:90:12", "12:34:56:78:90:13",
	"1234567890123456789012345678901234567890", "1234567890123456789012345678901234567891",
	" ", "-", ":", ".", ",", "+", "\"", "{", "}", "/", "\n", "a", "b", "f", "x", "T", "Z", "GMT", "UTC", "Mon", "_", "é", "9", "99", "abc", "abd",
	"E2E Blocked Agent ", "[E2E] generate-stop-", "job ", " queued",
}

// clockSiblings pairs each instant of clockFragments with the one that
// stands next to it there, the same instant changed: the change a read of
// the clock makes.
var clockSiblings = func() map[string]string {
	out := map[string]string{}
	for _, p := range [][2]string{
		{"1793336961", "1793336962"}, {"1793336961102639022", "1793336961102639023"}, {"1793336961.123", "1793336961.124"},
		{"2026-10-30", "2026-10-31"}, {"2026-10-30T08:00:00Z", "2026-10-30T08:00:01Z"}, {"20261030T080000Z", "20261030T080001Z"},
		{"0192ab3c-1d2e-7f00-8abc-0123456789ab", "0192ab3d-1d2e-7f00-8abc-0123456789ab"}, {"6ba7b810-9dad-11d1-80b4-00c04fd430c8", "6ba7b810-9dad-11d1-80b4-00c04fd430c9"},
		{"Fri, 30 Oct 2026 08:00:00 GMT", "Sat, 30 Oct 2026 08:00:00 GMT"}, {"08:00:00", "08:01:00"}, {"12:34:56:78:90:12", "12:34:56:78:90:13"},
		{"1234567890123456789012345678901234567890", "1234567890123456789012345678901234567891"}, {"abc", "abd"},
	} {
		out[p[0]], out[p[1]] = p[1], p[0]
	}
	return out
}()

// clockTextPair is a text of n fragments, most of them apart, and the same
// text changed once: an instant turned into its sibling, a fragment
// replaced, inserted or dropped, or one byte of one changed.
func clockTextPair(rnd *rand.Rand, n int) (string, string) {
	const seps = "  --:,\"x/"
	parts := make([]string, 0, 2*n)
	for i := 0; i < n; i++ {
		parts = append(parts, clockFragments[rnd.Intn(len(clockFragments))])
		if rnd.Intn(4) > 0 {
			parts = append(parts, string(seps[rnd.Intn(len(seps))]))
		}
	}
	rec := strings.Join(parts, "")
	i := rnd.Intn(len(parts))
	switch r := rnd.Intn(10); {
	case r < 4:
		for k := 0; k < len(parts); k++ {
			if sib, ok := clockSiblings[parts[(i+k)%len(parts)]]; ok {
				parts[(i+k)%len(parts)] = sib
				break
			}
		}
	case r < 6:
		parts[i] = clockFragments[rnd.Intn(len(clockFragments))]
	case r < 7:
		parts = append(parts[:i], append([]string{clockFragments[rnd.Intn(len(clockFragments))]}, parts[i:]...)...)
	case r < 8:
		parts = append(parts[:i], parts[i+1:]...)
	default:
		b := []byte(parts[i])
		j := rnd.Intn(len(b))
		b[j] = "0123456789abcdefxTZ:-. "[rnd.Intn(23)]
		parts[i] = string(b)
	}
	return rec, strings.Join(parts, "")
}

// clockWordIn reads the layouts only where two texts differ, with a seam on
// each side no instant runs across (clockWordWindow), and finds what a
// reading of both whole texts finds (clockWordInWhole): the same word, the
// same text around it, or none, on every text pair here. They are the
// shapes the CPU review measured (texts of padding, of numbers and of JSON
// whose one changed word is no instant, or is the read), the client-test
// texts of the word rows, changes no digit is in (a UUID's letter, a
// weekday, a zone), texts in which one side holds more instants of a layout
// than the other (paired by a whole reading with the same instants after
// the change), runs that chain instants across every place near the change
// (where the whole text on that side is read), and random texts of
// instants, separators and other bytes, short ones (read whole) and ones
// inside a longer text the same in both (cut at a seam), each read in a
// window that holds every instant and in the call's window of the word
// rows.
func TestAClockWordIsFoundWhereTwoTextsDifferAsInTheWholeTexts(t *testing.T) {
	runWin := &Window{
		Rec: Span{From: wordRec.prev.Add(-time.Second), To: wordRec.call},
		F:   Span{From: wordF.prev.Add(-time.Second), To: wordF.call},
	}
	type pair struct{ name, rec, live string }
	var pairs []pair
	ns := func(name, format string) {
		pairs = append(pairs, pair{name, fmt.Sprintf(format, wordRec.read.UnixNano()), fmt.Sprintf(format, wordF.read.UnixNano())})
	}
	for _, f := range []string{"E2E Blocked Agent %d", "[E2E] generate-stop-%d", "job %d queued", "E2EHANDOFF%d", "%d", "a-%d b-1", "n-%d r-1111"} {
		ns(f, f)
	}
	pairs = append(pairs,
		pair{"two reads", fmt.Sprintf("a-%d b-%d", wordRec.read.UnixNano(), wordRec.read.UnixNano()), fmt.Sprintf("a-%d b-%d", wordF.read.UnixNano(), wordF.read.UnixNano())},
		pair{"word plus random", fmt.Sprintf("n-%d r-1111", wordRec.read.UnixNano()), fmt.Sprintf("n-%d r-2222", wordF.read.UnixNano())},
		pair{"a UUID's letter", "id 0192ab3c-1d2e-7f00-8abc-0123456789ab ok", "id 0192ab3d-1d2e-7f00-8abc-0123456789ab ok"},
		// The longest instant changed at both ends: a differing part as
		// long as clockWordMax is still read.
		pair{"a UUID changed at its first and last byte", "id e7a78000-d437-11f1-80b4-00c04fd430c8 ok", "id f7a78000-d437-11f1-80b4-00c04fd430c9 ok"},
		pair{"a weekday", "at Fri, 30 Oct 2026 08:00:00 GMT ok", "at Sat, 30 Oct 2026 08:00:00 GMT ok"},
		pair{"a zone", "at Fri, 30 Oct 2026 08:00:00 GMT ok", "at Fri, 30 Oct 2026 08:00:00 UTC ok"},
		pair{"one more instant, the same after", "a 2026-10-30 b " + strings.Repeat("2026-10-30 ", 40), "a x b " + strings.Repeat("2026-10-30 ", 40)},
		pair{"one more instant, another after", "a 2026-10-30 b " + strings.Repeat("2026-10-31 ", 40), "a x b " + strings.Repeat("2026-10-31 ", 40)},
		pair{"one fewer instant, the same after", "a x b " + strings.Repeat("1793336961 ", 40), "a 1793336961 b " + strings.Repeat("1793336961 ", 40)},
		pair{"one more instant, a changed one before", "a 2026-10-29 c 2026-10-30 b " + strings.Repeat("2026-10-30 ", 40), "a 2026-10-28 c x b " + strings.Repeat("2026-10-30 ", 40)},
		pair{"a chain of times before", strings.Repeat("12:34:", 200) + "56 1793336961 x", strings.Repeat("12:34:", 200) + "56 1793336962 x"},
		pair{"a chain of times after", "x 1793336961 " + strings.Repeat("12:34:", 200) + "56", "x 1793336962 " + strings.Repeat("12:34:", 200) + "56"},
		pair{"a run of digits", strings.Repeat("1793336961", 100) + " 08:00:00 " + strings.Repeat("1793336961", 100), strings.Repeat("1793336961", 100) + " 08:00:01 " + strings.Repeat("1793336961", 100)},
		pair{"a run of digits changed", strings.Repeat("1793336961", 100), strings.Repeat("1793336961", 50) + "2" + strings.Repeat("1793336961", 50)},
		// The changed fraction ends where one text's time of day starts
		// ("00:00:00" in rec) and the other's is the fraction's own end:
		// the whole reading pairs rec's with the next time after the
		// window, and refuses the text where that one differs.
		pair{"one text's instant after the change, another after the window", "at 1793336961.5:00:00:00 " + strings.Repeat("x", 600) + " 12:34:56", "at 1793336961.55:00:00:00 " + strings.Repeat("x", 600) + " 12:34:56"},
		pair{"the other text's instant after the change, another after the window", "at 1793336961.55:00:00:00 " + strings.Repeat("x", 600) + " 12:34:56", "at 1793336961.5:00:00:00 " + strings.Repeat("x", 600) + " 12:34:56"},
		pair{"one text's instant after the change, the same after the window", "at 1793336961.5:00:00:00 " + strings.Repeat("x", 600) + " 00:00:00", "at 1793336961.55:00:00:00 " + strings.Repeat("x", 600) + " 00:00:00"},
		// Epoch seconds with a fraction chained by dots: whether the last
		// one is read alone turns on how many stand before it, so no
		// seam lies near it and the whole text before it is read.
		pair{"a chain of 41 fractions", strings.Repeat("1793336961.", 41) + "1793336961", strings.Repeat("1793336961.", 41) + "1793336962"},
		pair{"a chain of 40 fractions", strings.Repeat("1793336961.", 40) + "1793336961", strings.Repeat("1793336961.", 40) + "1793336962"},
	)
	for _, size := range []int{64 << 10} {
		pad := strings.Repeat("x", size)
		nums := strings.Repeat("1793336961 ", size/11)
		js := strings.Repeat(`{"id":"abc","n":17,"t":"2026-10-30T08:00:00Z"},`, size/48)
		for name, text := range map[string]string{"pad": pad, "numbers": nums, "json": js} {
			pairs = append(pairs,
				pair{name + ", random change", text + `"aaaa"`, text + `"bbbb"`},
				pair{name + ", clock word", text + fmt.Sprintf(" %d", wordRec.read.UnixNano()), text + fmt.Sprintf(" %d", wordF.read.UnixNano())},
				pair{name + ", change in the middle", text[:size/2] + "aaaa" + text[size/2:], text[:size/2] + "bbbb" + text[size/2:]},
				pair{name + ", clock word in the middle", text[:size/2] + fmt.Sprintf(" %d ", wordRec.read.UnixNano()) + text[size/2:], text[:size/2] + fmt.Sprintf(" %d ", wordF.read.UnixNano()) + text[size/2:]},
			)
		}
	}
	rnd := rand.New(rand.NewSource(20261030))
	for i := 0; i < 20000; i++ {
		rec, live := clockTextPair(rnd, 1+rnd.Intn(8))
		pairs = append(pairs, pair{"random short", rec, live})
	}
	for i := 0; i < 1000; i++ {
		rec, live := clockTextPair(rnd, 1+rnd.Intn(8))
		before, _ := clockTextPair(rnd, 1+rnd.Intn(120))
		after, _ := clockTextPair(rnd, 1+rnd.Intn(120))
		pairs = append(pairs, pair{"random inside a long text", before + rec + after, before + live + after})
	}
	// Texts long enough on each side to be cut at a seam (clockSeamMin),
	// on one side or both.
	for i := 0; i < 3000; i++ {
		rec, live := clockTextPair(rnd, 1+rnd.Intn(8))
		before, _ := clockTextPair(rnd, 4+rnd.Intn(16))
		after, _ := clockTextPair(rnd, 4+rnd.Intn(16))
		pairs = append(pairs, pair{"random inside a text cut at a seam", before + rec + after, before + live + after})
	}
	found, parted := 0, 0
	for _, pc := range pairs {
		for _, w := range []*Window{anyInstant, runWin} {
			b, rw, lw, a, ok := clockWordIn(pc.rec, pc.live, w)
			wb, wrw, wlw, wa, wok := clockWordInWhole(pc.rec, pc.live, w)
			if ok != wok || b != wb || rw != wrw || lw != wlw || a != wa {
				t.Fatalf("%s, %q -> %q: clockWordIn = %v %q/%q (%d bytes before, %d after), the whole texts' = %v %q/%q (%d, %d)",
					pc.name, pc.rec, pc.live, ok, rw, lw, len(b), len(a), wok, wrw, wlw, len(wb), len(wa))
			}
			if ok {
				found++
			}
		}
		if q, er, _, near := clockWordWindow(pc.rec, pc.live); near && (q > 0 || er < len(pc.rec)) {
			parted++
		}
	}
	// Neither side of the row is idle: words are found, and texts are read
	// in part.
	if found < 2000 || parted < 5000 {
		t.Fatalf("%d of %d text pairs found a word, %d were read in part; the row reads too few of either", found, 2*len(pairs), parted)
	}
}

// clockWordIn's window has the outcome of the whole reading at its edges
// too: where an instant after the window starts at the window's very end,
// where an instant after it is glued to a word (and is no instant), and
// where a glued word stands before the window. The changed fraction of
// "at 1793336961.5:00:00:00" leaves one text a time of day more than the
// other, which a whole reading pairs with the next time after the window:
// a time that differs from it refuses the text, and a glued one is passed
// over. Each shape is padded by 0 to 80 bytes, so that the instant after the
// change lands on every place near the window's end, and stands alone, after
// a long text, before one, or both, so that both sides of the window are
// short enough to be read whole (clockSeamMin) and long enough to be cut at
// a seam.
func TestAClockWordWindowsEdgesReadAsTheWholeTexts(t *testing.T) {
	fill := strings.Repeat("z", clockSeamMin+2*clockWordMax)
	type pair struct{ rec, live string }
	var pairs []pair
	for k := 0; k <= 80; k++ {
		pad := strings.Repeat("x", k)
		for _, tail := range []string{"12:34", "12:34:56", " 12:34", "1793336961", "2026-10-30", "y12:34", "a1793336961"} {
			for _, sh := range [][2]string{
				{"at 1793336961.5:00:00:00 " + pad + tail, "at 1793336961.55:00:00:00 " + pad + tail},
				{"at 1793336961.55:00:00:00 " + pad + tail, "at 1793336961.5:00:00:00 " + pad + tail},
				{pad + tail + " " + pad + " 1793336961102639022", pad + tail + " " + pad + " 1793336961102639023"},
				{"1793336961102639022 " + pad + tail + pad, "1793336961102639023 " + pad + tail + pad},
			} {
				for _, f := range [][2]string{{"", ""}, {fill + " ", ""}, {"", " " + fill}, {fill + " ", " " + fill}} {
					pairs = append(pairs, pair{f[0] + sh[0] + f[1], f[0] + sh[1] + f[1]})
				}
			}
		}
	}
	found, cutBefore, cutAfter := 0, 0, 0
	for _, pc := range pairs {
		b, rw, lw, a, ok := clockWordIn(pc.rec, pc.live, anyInstant)
		wb, wrw, wlw, wa, wok := clockWordInWhole(pc.rec, pc.live, anyInstant)
		if ok != wok || b != wb || rw != wrw || lw != wlw || a != wa {
			t.Fatalf("%q -> %q: clockWordIn = %v %q/%q (%d bytes before, %d after), the whole texts' = %v %q/%q (%d, %d)",
				pc.rec, pc.live, ok, rw, lw, len(b), len(a), wok, wrw, wlw, len(wb), len(wa))
		}
		if ok {
			found++
		}
		if q, er, _, near := clockWordWindow(pc.rec, pc.live); near {
			if q > 0 {
				cutBefore++
			}
			if er < len(pc.rec) {
				cutAfter++
			}
		}
	}
	// The row reads words found and refused, and windows cut on each side.
	if found == 0 || found == len(pairs) || cutBefore == 0 || cutAfter == 0 {
		t.Fatalf("%d pairs: %d found a word, %d were cut before the change, %d after it", len(pairs), found, cutBefore, cutAfter)
	}
}

// A text's changed word is looked for in a bounded part of it, whatever its
// length: in a 1 MiB text of padding, of numbers or of JSON whose one
// changed word is no instant, or is the read, clockWordIn reads at most a
// few hundred bytes (clockWordWindow). A changed part longer than any
// instant (a UUID's 36 bytes) holds no one changed word, and no layout is
// read at all.
func TestAClockWordIsLookedForInABoundedPartOfALongText(t *testing.T) {
	if clockWordMax != 36 {
		t.Fatalf("clockWordMax = %d; the longest instant of the layouts is a UUID's 36 bytes", clockWordMax)
	}
	for k, s := range []string{"Fri, 30 Oct 2026 08:00:00 +0000", "2026-10-30 08:00:00.123456789+05:30", "20261030T080000Z", "0192ab3c-1d2e-7f00-8abc-0123456789ab", "1793336961.123456789", "08:00:00.123456789"} {
		if m := clockLayouts[k].re.FindString(s); m != s {
			t.Fatalf("layout %d matches %q of %q", k, m, s)
		}
		re, _ := syntax.Parse(clockLayouts[k].re.String(), syntax.Perl)
		if n, ok := maxMatchLen(re); !ok || n < len(s) {
			t.Fatalf("layout %d: maxMatchLen = %d, %v, below its instant %q of %d bytes", k, n, ok, s, len(s))
		}
	}
	for _, pat := range []string{`a+`, `a*`, `\d{2,}`} {
		re, _ := syntax.Parse(pat, syntax.Perl)
		if _, ok := maxMatchLen(re); ok {
			t.Fatalf("%s: maxMatchLen bounded", pat)
		}
	}
	const size = 1 << 20
	bound := 2*clockCutSearch + 3*clockWordMax
	for name, text := range map[string]string{
		"pad":     strings.Repeat("x", size),
		"numbers": strings.Repeat("1793336961 ", size/11),
		"json":    strings.Repeat(`{"id":"abc","n":17,"t":"2026-10-30T08:00:00Z"},`, size/48),
	} {
		for change, pc := range map[string][2]string{
			"random change at the end":    {text + `"aaaa"`, text + `"bbbb"`},
			"clock word at the end":       {text + fmt.Sprintf(" %d", wordRec.read.UnixNano()), text + fmt.Sprintf(" %d", wordF.read.UnixNano())},
			"random change in the middle": {text[:size/2] + "aaaa" + text[size/2:], text[:size/2] + "bbbb" + text[size/2:]},
		} {
			q, er, el, near := clockWordWindow(pc[0], pc[1])
			if !near || er-q > bound || el-q > bound {
				t.Fatalf("%s, %s: the part read is %d-%d of %d bytes (%v); want at most %d", name, change, q, er, len(pc[0]), near, bound)
			}
		}
		if _, _, _, near := clockWordWindow(text+strings.Repeat("a", 37), text+strings.Repeat("b", 37)); near {
			t.Fatalf("%s: a changed part of 37 bytes is read", name)
		}
	}
}

// clockPairsAfter pairs as a reading of the whole texts pairs: the
// window's left-over instants first, then the instants after the window,
// each against the next instant after the window, in order. Two left over
// that differ, followed by the same two again, are passed over; a later
// pair that differs refuses the text.
func TestClockPairsAfterPairsInTheWholeReadingsOrder(t *testing.T) {
	iso := clockLayouts[1]
	window := "w 2026-10-30 2026-10-31"
	extra := [][]int{{2, 12}, {13, 23}}
	if s := window + " 2026-10-30 2026-10-31 2026-10-30 2026-10-31"; !clockPairsAfter(iso, s, len(window), s, extra) {
		t.Fatal("the window's two instants, then the same two after it twice: refused")
	}
	if s := window + " 2026-10-30 2026-10-31 2026-10-30 2026-10-30"; clockPairsAfter(iso, s, len(window), s, extra) {
		t.Fatal("a fourth instant after the window that differs from the second: passed over")
	}
	if s := window + " x"; !clockPairsAfter(iso, s, len(window), s, extra) {
		t.Fatal("no instant after the window: refused")
	}
}

// clockWordWindow cuts both texts at seams: every layout's reading of
// rec[q:er] finds the instants a reading of the whole of rec finds there,
// and no instant of the whole reading runs across q or er; the same of
// live[q:el]. The instants before q are the same in both texts, as are
// those from er in rec and from el in live. The texts are random texts of
// instants and of runs that chain them (times of day joined by colons,
// fractions joined by dots), changed once, inside such runs.
func TestAClockWordWindowIsCutWhereNoInstantRunsAcross(t *testing.T) {
	rnd := rand.New(rand.NewSource(20261031))
	chains := []string{strings.Repeat("12:34:", 60), strings.Repeat("1793336961.", 30), strings.Repeat("1793336961", 30), strings.Repeat("ab-", 40), "x"}
	type place struct{ at, end int }
	matches := func(lay clockLayout, s string, from, to int) []place {
		var out []place
		for _, m := range lay.re.FindAllStringIndex(s[from:to], -1) {
			out = append(out, place{m[0] + from, m[1] + from})
		}
		return out
	}
	parted := 0
	for i := 0; i < 3000; i++ {
		rec, live := clockTextPair(rnd, 1+rnd.Intn(6))
		before, _ := clockTextPair(rnd, 1+rnd.Intn(30))
		after, _ := clockTextPair(rnd, 1+rnd.Intn(30))
		before += chains[rnd.Intn(len(chains))]
		after = chains[rnd.Intn(len(chains))] + after
		r, l := before+rec+after, before+live+after
		q, er, el, near := clockWordWindow(r, l)
		if !near {
			continue
		}
		if q > 0 || er < len(r) {
			parted++
		}
		for k, lay := range clockLayouts {
			var pre, post [2][]place
			for side, x := range []struct {
				s string
				e int
			}{{r, er}, {l, el}} {
				var in []place
				for _, m := range matches(lay, x.s, 0, len(x.s)) {
					switch {
					case m.at < q && m.end > q || m.at < x.e && m.end > x.e:
						t.Fatalf("%q -> %q, layout %d: %q at %d-%d runs across the window %d-%d", r, l, k, x.s[m.at:m.end], m.at, m.end, q, x.e)
					case m.end <= q:
						pre[side] = append(pre[side], m)
					case m.at >= x.e:
						post[side] = append(post[side], place{m.at - x.e, m.end - x.e})
					default:
						in = append(in, m)
					}
				}
				if got := matches(lay, x.s, q, x.e); fmt.Sprint(got) != fmt.Sprint(in) {
					t.Fatalf("%q -> %q, layout %d: the window %d-%d of %q reads %v, the whole text %v there", r, l, k, q, x.e, x.s, got, in)
				}
			}
			if fmt.Sprint(pre[0]) != fmt.Sprint(pre[1]) || fmt.Sprint(post[0]) != fmt.Sprint(post[1]) {
				t.Fatalf("%q -> %q, layout %d: the instants outside the window differ: before %v and %v, after %v and %v", r, l, k, pre[0], pre[1], post[0], post[1])
			}
		}
	}
	if parted < 500 {
		t.Fatalf("%d of 3000 text pairs were read in part; the row reads too few", parted)
	}
}

// A test's own call in keploy test has its window read by the offset rule
// alone (Window.OffsetsOnly): a value at the clock plus a constant offset
// (an expiry 30 minutes on, a cutoff 2 minutes back), typed or as an epoch's
// milliseconds, is learned as that offset, as a window that compares
// offsets learns it, where with no window the lead shows it no read made in
// its call's window. Every other value is learned exactly as with no window,
// by the lead alone: a plain read in the window, one made 2 s before it
// (an offset finer than a second), and a constant that changed. A nil
// window stays nil.
func TestAnOffsetsOnlyWindowIsReadByTheOffsetRuleAlone(t *testing.T) {
	const p = "body.expiresAt"
	c := newClockOffsetCall()
	ms := time.Millisecond
	only := c.w.OffsetsOnly()
	if (*Window)(nil).OffsetsOnly() != nil {
		t.Fatal("a nil window's OffsetsOnly is not nil")
	}
	typedMs := func(p string, t time.Time) Leaf { return InstantLeaf(p, p, t.Truncate(ms), ms) }
	in := func(leaf func(string, time.Time) Leaf, rec, live time.Time, w *Window) Learned {
		return ClassifyIn(clockedDrifts(), []string{p}, Shape{Leaves: map[string]Leaf{p: leaf(p, live)}}, Shape{Leaves: map[string]Leaf{p: leaf(p, rec)}}, c.call, nil, nil, nil, w)[0]
	}
	for _, form := range []struct {
		name string
		leaf func(string, time.Time) Leaf
	}{{"typed", typedMs}, {"epoch millis text", millisText}} {
		for _, off := range []time.Duration{30 * time.Minute, -2 * time.Minute} {
			rec, live := c.read.Add(off), c.fread.Add(off)
			got, want := in(form.leaf, rec, live, only), in(form.leaf, rec, live, c.w.ComparesOffsets())
			if got.Kind != KindClockOffset || got.Offset == nil || !reflect.DeepEqual(got, want) {
				t.Errorf("%s at clock %+v: %s (%s); want the clock offset a comparing window learns: %s (%s)", form.name, off, got.Kind, got.Why, want.Kind, want.Why)
			}
			if none := in(form.leaf, rec, live, nil); none.Kind == KindClockOffset {
				t.Errorf("%s at clock %+v: with no window learned %s (%s); the offset needs the window", form.name, off, none.Kind, none.Why)
			}
		}
		for _, v := range []struct {
			name      string
			rec, live time.Time
		}{
			{"a plain read in the window", c.read, c.fread},
			{"a read 2 s before the window", c.call.Add(-2 * time.Second), c.fcall.Add(-2*time.Second - 5*ms)},
			{"a constant that changed", time.Date(2025, 1, 1, 0, 0, 0, 123e6, time.UTC), time.Date(2025, 3, 7, 5, 6, 7, 456e6, time.UTC)},
		} {
			got, want := in(form.leaf, v.rec, v.live, only), in(form.leaf, v.rec, v.live, nil)
			if !reflect.DeepEqual(got, want) {
				t.Errorf("%s, %s: %s (%s) %+v; want it learned as with no window: %s (%s) %+v", form.name, v.name, got.Kind, got.Why, got.Read, want.Kind, want.Why, want.Read)
			}
		}
	}
}
