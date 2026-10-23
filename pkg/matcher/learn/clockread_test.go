package learn

import (
	"encoding/base64"
	"strconv"
	"strings"
	"testing"
	"time"
)

// recAt is the instant the probes' recorded values read; F reads it moved by
// the full round's lead, as F's clock does.
var recAt = time.Date(2026, 10, 21, 10, 28, 2, 0, time.UTC)

func iso(t time.Time) string { return t.Format(time.RFC3339) }

func stringLeaf(v string) Shape {
	return Shape{Key: "POST dest /1", Leaves: map[string]Leaf{"body.note": {Path: "body.note", Noise: "body.note", Type: String, Value: v}}}
}

// classifyNote is what Classify learns of body.note, recorded rec and live
// live, with no earlier place and no answer.
func classifyNote(t *testing.T, rec, live string) Learned {
	t.Helper()
	got := Classify(nil, []string{"body.note"}, stringLeaf(live), stringLeaf(rec), recAt, nil, nil, nil)
	if len(got) != 1 {
		t.Fatalf("%q -> %q learned %d leaves, want 1", rec, live, len(got))
	}
	return got[0]
}

// A drift that holds nothing new is refused (b95's (A), kept): only the order
// of its words, a sign or separator, or how many times it holds a word
// changed, in any reading, read decoded where it has escapes. The refusal
// names the part, never a value.
func TestADriftThatHoldsNothingNewIsRefused(t *testing.T) {
	for _, tc := range []struct {
		name, rec, live, why string
		values               []string
	}{
		{"a sign flip", "-5", "5", "no word in it is new: only a sign or separator between its words changed", []string{"5"}},
		{"swapped ids", "A7K B8L", "B8L A7K", "no word in it is new: only the order of its words changed", []string{"A7K", "B8L"}},
		{"swapped ids, percent-encoded", "A7K%2CB8L", "B8L%2CA7K", "no word in it is new: only the order of its words changed", []string{"A7K", "B8L"}},
		{"swapped ids after a JSON escape", `A7K\nB8L`, `B8L\nA7K`, "no word in it is new: only the order of its words changed", []string{"A7K", "B8L"}},
		{"a sign flip, percent-encoded", "%2D5", "5", "no word in it is new: only a sign or separator between its words changed", []string{"5"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := classifyNote(t, tc.rec, tc.live)
			if got.Kind != KindUnaccounted || !strings.Contains(got.Why, tc.why) {
				t.Fatalf("%q -> %q learned as %s (%q), want refused saying %q", tc.rec, tc.live, got.Kind, got.Why, tc.why)
			}
			for _, v := range tc.values {
				if ContainsToken(got.Why, v) {
					t.Errorf("refused as %q, which holds %q of the value", got.Why, v)
				}
			}
			dc := NewDriftCache()
			for i := 0; i < 2; i++ {
				again := Classify(dc, []string{"body.note"}, stringLeaf(tc.live), stringLeaf(tc.rec), recAt, nil, nil, nil)[0]
				if again.Kind != got.Kind || again.Why != got.Why {
					t.Fatalf("read again: %s %q, first %s %q", again.Kind, again.Why, got.Kind, got.Why)
				}
			}
		})
	}
}

// What b95 refused whole beside a clock read (its (B) gate, gone) is learned
// by its words now: the clock read is one instant slot, a word beside it that
// changed is a slot of its own, and every other byte is still compared. The
// leaf is noise to Classify; what it learns is its words (learnWords).
func TestTextBesideAClockReadLearnsOnlyItsWords(t *testing.T) {
	later := recAt.Add(FullRoundLead)
	b64u := func(s string) string { return base64.RawURLEncoding.EncodeToString([]byte(s)) }
	jwt := func(jti string, iat int64) string {
		return "Bearer " + b64u(`{"alg":"HS256","typ":"JWT"}`) + "." + b64u(`{"jti":"`+jti+`","iat":`+strconv.FormatInt(iat, 10)+`}`) + "." + b64u("\x01\x02"+jti[:3]+"\xfe\xff")
	}
	for _, tc := range []struct {
		name, rec, live, slots string
		// red is a live value the words learned must not admit.
		red string
	}{
		{"text beside a clock read", "ORD-1 paid 100 at " + iso(recAt), "ORD-1 paid 100 at " + iso(later), "s4i:iso8601:d4-d2-d2Td2:d2:d2FZ", "ORD-1 paid 999 at " + iso(later)},
		{"a sequence beside a clock read", "seq=28 at " + iso(recAt), "seq=29 at " + iso(later), "s1r0 s3i:iso8601:d4-d2-d2Td2:d2:d2FZ", "set=29 at " + iso(later)},
		{"text after a clock read", iso(recAt) + " by u1", iso(later) + " by u1", "s0i:iso8601:d4-d2-d2Td2:d2:d2FZ", iso(later) + " by u2"},
		{"text in brackets after an instant", iso(recAt) + "[ORD-1]", iso(later) + "[ORD-1]", "s0i:iso8601:d4-d2-d2Td2:d2:d2FZ", iso(later) + "[ORD-2]"},
		{"free text where a JavaScript zone name stands", recAt.Format("Mon Jan 02 2006 15:04:05 GMT-0700") + " (ORD-1 paid 100)", later.Format("Mon Jan 02 2006 15:04:05 GMT-0700") + " (ORD-1 paid 100)", "s0i:jsdate:W M d2 d4 d2:d2:d2 GMT+d4", later.Format("Mon Jan 02 2006 15:04:05 GMT-0700") + " (ORD-1 paid 999)"},
		{"a signed number after a time", recAt.Format("2006-01-02 15:04:05") + " -1530", later.Format("2006-01-02 15:04:05") + " -1530", "s0i:iso8601:d4-d2-d2 d2:d2:d2F", later.Format("2006-01-02 15:04:05") + " -1531"},
		{"a UUID v7 whose random groups did not change", uuid7(recAt, "abc123456789abcdef"), uuid7(later, "abc123456789abcdef"), "s0i:uuid:x8-x4-7x3-vx3-x12", "1b4e28ba-2fa1-41d2-883f-0016d3cca427"},
		{"a UUID v7 with fresh random groups", uuid7(recAt, "abc123456789abcdef"), uuid7(later, "def0123456789abcde"), "s0i:uuid:x8-x4-7x3-vx3-x12", "1b4e28ba-2fa1-41d2-883f-0016d3cca427"},
		{"a fresh id beside a clock read", "req-3f2b8c1e9a4d " + iso(recAt), "req-a7c4e2d10b3f " + iso(later), "s0r1 s1i:iso8601:d4-d2-d2Td2:d2:d2FZ", "rex-a7c4e2d10b3f " + iso(later)},
		{"a JWT whose iat F's clock moved, beside a fresh jti", jwt("Hq6Ys0Bc5D", recAt.Unix()), jwt("Mw2Xr8Ue3F", later.Unix()), "s1r1 s1r2", "Basic " + jwt("Mw2Xr8Ue3F", later.Unix())[7:]},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := classifyNote(t, tc.rec, tc.live); got.Kind != KindNoise {
				t.Fatalf("%q -> %q learned as %s (%s), want noise", tc.rec, tc.live, got.Kind, got.Why)
			}
			e := learnedWords(t, tc.rec, tc.live)
			if got := slotsOf(e); got != tc.slots {
				t.Fatalf("slots %s, want %s", got, tc.slots)
			}
			if ok, _ := WordsAdmit(tc.rec, tc.red, e); ok {
				t.Errorf("%q admitted: a byte outside the slots changed", tc.red)
			}
		})
	}
}

// What the ISO layout reads of a value: an offset after a space only after
// a time, a '+' between the date and a time to the second only, and a
// region only of an IANA area.
func TestTheISOLayoutReadsAnInstantsOwnParts(t *testing.T) {
	iso8601 := clockLayouts[layoutNamed("iso8601")]
	for _, tc := range []struct{ in, want string }{
		{"2026-10-21 10:28:02 +0000 UTC", "2026-10-21 10:28:02 +0000 UTC"},
		{"2026-10-21 10:28:02 +0100 BST", "2026-10-21 10:28:02 +0100 BST"},
		{"2026-10-21 10:28:02 +0000 INFO", "2026-10-21 10:28:02 +0000"},
		{"2026-10-21 10:28 -2026-10-21", "2026-10-21 10:28"},
		{"2026-10-21 +1234", "2026-10-21"},
		{"2026-10-21 10:28:02 -1530", "2026-10-21 10:28:02"},
		{"2026-10-21+10:28:02", "2026-10-21+10:28:02"},
		{"2026-10-21+05:00", "2026-10-21+05:00"},
		{"2026-10-21T10:28:02+02:00[Europe/Paris]", "2026-10-21T10:28:02+02:00[Europe/Paris]"},
		{"2026-10-21T10:28:02Z[Orders/ORD-1]", "2026-10-21T10:28:02Z"},
		{"2026-10-21T10:28:02Z[Etc/ORD-4417]", "2026-10-21T10:28:02Z"},
		{"2026-10-21T10:28:02Z[America/ORD-4417]", "2026-10-21T10:28:02Z"},
		{"2026-10-21T10:28:02-03:00[America/Argentina/Buenos_Aires]", "2026-10-21T10:28:02-03:00[America/Argentina/Buenos_Aires]"},
		{"2026-10-21T10:28:02+05:00[Etc/GMT-5]", "2026-10-21T10:28:02+05:00[Etc/GMT-5]"},
		{"2026-10-21T10:28:02Z[Etc/UTC]", "2026-10-21T10:28:02Z[Etc/UTC]"},
		{"2026-10-21T10:28:02-04:00[America/Port-au-Prince]", "2026-10-21T10:28:02-04:00[America/Port-au-Prince]"},
	} {
		if got := iso8601.re.FindString(tc.in); got != tc.want {
			t.Errorf("the ISO layout reads %q of %q, want %q", got, tc.in, tc.want)
		}
	}
	// An xs:date's offset is its zone, not a time: no instant at minutes.
	if tm, res, ok := readISO("2026-10-21+05:00"); ok && res < 24*time.Hour {
		t.Errorf("an xs:date read as %v at %v", tm, res)
	}
}

// An escape's remnant is a clock word where its escapes, peeled as they
// decode, leave one; an escape peels the next only where it stands for a
// '%' or a '\\'.
func TestEscapeClockPeelsOnlyEscapesThatStandForAnEscape(t *testing.T) {
	for _, tc := range []struct {
		s, word string
		clock   bool
	}{
		{"10%3A41Z", "3A41Z", true},
		{"10%253A41Z", "253A41Z", true},
		{`10\u003A41Z`, "u003A41Z", true},
		{`10%5Cn41Z`, "5Cn41Z", true},
		// "%41" is an "A": no escape after it.
		{"10%413A41Z", "413A41Z", false},
		// Up to four escapes are peeled, each standing for the next: five are not.
		{"10%25253A41Z", "25253A41Z", true},
		{"10%2525253A41Z", "2525253A41Z", true},
		{"10%252525253A41Z", "252525253A41Z", false},
		{`10\u00253A41Z`, "u00253A41Z", true},
		{`10\u005Cn41Z`, "u005Cn41Z", true},
		{`10%5Cu003A41Z`, "5Cu003A41Z", true},
		// Each escape is read after its own byte, in full.
		{`10\3A41Z`, "3A41Z", false},
		{"10%u003A41Z", "u003A41Z", false},
		{"10%n41Z", "n41Z", false},
		{"10-3A41Z", "3A41Z", false},
		{"10%A", "A", false},
		{`10\uG03A41Z`, "uG03A41Z", false},
		{`10\u0G3A41Z`, "u0G3A41Z", false},
		{`10\u00GA41Z`, "u00GA41Z", false},
		{`10\u003G41Z`, "u003G41Z", false},
		{`10\u003`, "u003", false},
		{`10\u00413A41Z`, "u00413A41Z", false},
	} {
		set := map[string]bool{"41Z": true}
		escapeClock(tc.s, set)
		if set[tc.word] != tc.clock {
			t.Errorf("escapeClock(%q): %q a clock word %v, want %v", tc.s, tc.word, set[tc.word], tc.clock)
		}
	}
}

// A read the lead alone accepts (it moved by FullRoundLead, outside the
// call's window) is no accepted read when the lead is not taken: only a read
// within the call's window is.
func TestLeadOnlyReadIsNoClockReadWithoutLead(t *testing.T) {
	later := recAt.Add(FullRoundLead)
	rec, live := iso(recAt), iso(later)
	if c := clockInBy(rec, live, nil, true); len(c.pairs) != 1 {
		t.Fatalf("%q -> %q: the lead reads %d clock reads, want 1", rec, live, len(c.pairs))
	}
	if c := clockInBy(rec, live, nil, false); len(c.pairs) != 0 {
		t.Fatalf("%q -> %q: a read only the lead accepts is one without the lead", rec, live)
	}
	away := &Window{Rec: Span{From: recAt.Add(-time.Hour), To: recAt.Add(-time.Minute)}, F: Span{From: later.Add(time.Minute), To: later.Add(time.Hour)}}
	if c := clockInBy(rec, live, away, false); len(c.pairs) != 0 {
		t.Fatalf("%q -> %q: a read outside the call's window is one without the lead", rec, live)
	}
	boot := recAt.Add(4 * time.Minute)
	in := &Window{Rec: Span{From: recAt.Add(-time.Second), To: recAt.Add(time.Second)}, F: Span{From: boot.Add(-time.Second), To: boot.Add(time.Second)}}
	if c := clockInBy(rec, iso(boot), in, false); len(c.pairs) != 1 {
		t.Fatalf("%q -> %q: a read within the call's window is no clock read", rec, iso(boot))
	}
	if c := clockInBy(rec, iso(boot), nil, true); len(c.pairs) != 0 {
		t.Fatalf("%q -> %q: the lead reads a read it did not move", rec, iso(boot))
	}
	// Within the window, its words are learned as an instant slot.
	e, why := NewDriftCache().Words("at "+rec, "at "+iso(boot), in)
	if why != "" || slotsOf(e) != "s1i:iso8601:d4-d2-d2Td2:d2:d2FZ" {
		t.Fatalf("within the window: %s (%s)", slotsOf(e), why)
	}
	if _, why := NewDriftCache().Words("at "+rec, "at "+iso(boot), nil); why != whyInstant {
		t.Fatalf("by the lead alone: %q, want %q", why, whyInstant)
	}
}

// An instant that moved as F's clock moves it, but is written in another
// zone than when it was recorded, is no clock read: F's clock moves no zone.
// Its words are not learned, alone or beside text, and the refusal says so.
func TestZoneMismatchIsNoClockRead(t *testing.T) {
	later := recAt.Add(FullRoundLead)
	plus1 := time.FixedZone("", 3600)
	for _, tc := range []struct{ name, rec, live string }{
		{"Z and +01:00", iso(recAt), later.In(plus1).Format(time.RFC3339)},
		{"Z and +00:00", iso(recAt), later.Format("2006-01-02T15:04:05+00:00")},
		{"GMT and UTC", recAt.Format("Mon, 02 Jan 2006 15:04:05 GMT"), later.Format("Mon, 02 Jan 2006 15:04:05 UTC")},
		{"a zone dropped", recAt.Format("20060102T150405Z"), later.Format("20060102T150405")},
		{"UTC and GMT after a SQL datetime", recAt.Format("2006-01-02 15:04:05") + " UTC", later.Format("2006-01-02 15:04:05") + " GMT"},
		{"an offset after a space", recAt.Format("2006-01-02 15:04:05 -0700"), later.In(plus1).Format("2006-01-02 15:04:05 -0700")},
		{"a JavaScript zone's name", recAt.Format("Mon Jan 02 2006 15:04:05 GMT-0700") + " (Coordinated Universal Time)", later.Format("Mon Jan 02 2006 15:04:05 GMT-0700") + " (Greenwich Mean Time)"},
		{"a region", iso(recAt) + "[UTC]", iso(later) + "[Etc/UTC]"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, v := range [][2]string{{tc.rec, tc.live}, {"ORD-1 paid 100 at " + tc.rec, "ORD-1 paid 100 at " + tc.live}} {
				w := &Window{Rec: Span{From: recAt.Add(-time.Second), To: recAt.Add(time.Second)}, F: Span{From: later.Add(-time.Second), To: later.Add(time.Second)}}
				for _, win := range []*Window{nil, w} {
					if _, why := NewDriftCache().Words(v[0], v[1], win); why != whyZone {
						t.Fatalf("%q -> %q: %q, want %q", v[0], v[1], why, whyZone)
					}
				}
				if whole, _ := WholeDrift(v[0], v[1], nil); whole && v[0] != tc.rec {
					t.Fatalf("%q -> %q: text beside a zone change is whole", v[0], v[1])
				}
			}
		})
	}
	// The same zone, written alike: a clock read, one instant slot.
	if rec, live := recAt.In(plus1).Format(time.RFC3339), later.In(plus1).Format(time.RFC3339); slotsOf(learnedWords(t, rec, live)) != "s0i:iso8601:d4-d2-d2Td2:d2:d2F+d2:d2" {
		t.Fatalf("%q -> %q, one zone: %s", rec, live, slotsOf(learnedWords(t, rec, live)))
	}
}
