package learn

import (
	"fmt"
	"math/rand"
	"strconv"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
)

// wAt is the instant the word-level probes record; F reads it moved by
// the full round's lead.
var wAt = time.Date(2026, 10, 21, 10, 28, 2, 0, time.UTC)

// learnedWords is what F learns of the words of (rec, live), by the lead.
func learnedWords(t *testing.T, rec, live string) models.WordsEntry {
	t.Helper()
	e, why := NewDriftCache().Words(rec, live, nil)
	if why != "" {
		t.Fatalf("%q -> %q: no words learned: %s", rec, live, why)
	}
	if ok, why := WordsAdmit(rec, live, e); !ok {
		t.Fatalf("%q -> %q: the words learned of F do not admit F: %s", rec, live, why)
	}
	if ok, why := WordsAdmit(rec, rec, e); !ok {
		t.Fatalf("%q -> %q: the words learned do not admit the recording: %s", rec, live, why)
	}
	return e
}

// slotsOf is e's slots, written compactly: "s4" a run slot's segment and run
// ("s4r0"), "s3t" a token, "s7i:iso8601:<skeleton>" an instant.
func slotsOf(e models.WordsEntry) string {
	var out []string
	for _, s := range e.Slots {
		switch s.Kind {
		case models.WordSlotRun:
			out = append(out, fmt.Sprintf("s%dr%d", s.Seg, s.Run))
		case models.WordSlotToken:
			out = append(out, fmt.Sprintf("s%dt", s.Seg))
		case models.WordSlotInstant:
			out = append(out, fmt.Sprintf("s%di:%s:%s", s.Seg, s.Layout, s.Skeleton))
		}
	}
	return strings.Join(out, " ")
}

// The founder's case: of "ORD-1 paid 100 ref=<hex>", only the ref changed in
// F, so only the ref may change; "paid 999" is a regression keploy catches,
// and a fresh ref is no difference.
func TestOnlyTheWordsThatChangedMayChange(t *testing.T) {
	rec, f := "ORD-1 paid 100 ref=3f2b8c1e9a4d", "ORD-1 paid 100 ref=a7c4e2d10b3f"
	e := learnedWords(t, rec, f)
	if got := slotsOf(e); got != "s4r0" {
		t.Fatalf("slots %s, want the ref's alone", got)
	}
	for _, tc := range []struct {
		live string
		ok   bool
	}{
		{"ORD-1 paid 100 ref=0c9d8e7f6a5b", true},
		{"ORD-1 paid 100 ref=x", true},
		{"ORD-1 paid 999 ref=0c9d8e7f6a5b", false},
		{"ORD-2 paid 100 ref=0c9d8e7f6a5b", false},
		{"ORD-1 paid 100 ref=", false},
		{"ORD-1 paid 100 ref=0c9d 8e7f", false},
		{"ORD-1 paid 100  ref=0c9d8e7f6a5b", false},
		{"ORD-1 paid 100 ref=0c9d8e7f6a5b ", false},
		{"ORD-1 paid 100 ref=ünï", false},
		{"ORD-1 paid 100 ref=a.b-c_d", true}, // the run alone in its segment: any ASCII token (N1)
	} {
		if ok, why := WordsAdmit(rec, tc.live, e); ok != tc.ok {
			t.Errorf("%q admitted %v (%s), want %v", tc.live, ok, why, tc.ok)
		}
	}
}

// What each kind of slot admits at serve: a run among others only a run of
// ASCII letters and digits, its separators fixed; a run alone in its
// segment, or a segment whose runs drifted, any ASCII token.
func TestWhatEachSlotAdmits(t *testing.T) {
	for _, tc := range []struct {
		name, rec, f, slots string
		live                map[string]bool
	}{
		{"a run among others", "id ab-12-cd", "id ab-34-cd", "s1r1", map[string]bool{
			"id ab-99-cd": true, "id ab-x9Y-cd": true, "id ab-9.9-cd": false, "id ab-9-9-cd": false, "id ab--cd": false, "id ab-99-ce": false, "id ab_99-cd": false,
		}},
		{"a run alone in its segment", "k=v1", "k=v2", "s1r0", map[string]bool{
			"k=v3": true, "k=a-b.c": true, "k=": false, "k=a b": false, "k=é": false, "j=v3": false,
		}},
		{"a run alone, separators around it fixed", "k=-v1-", "k=-v2-", "s1r0", map[string]bool{
			"k=-x_y-": true, "k=v3": false, "k=-v3": false,
		}},
		{"a segment whose runs drifted", "t V1StGXR8_Z5jdHi6B-myT", "t x9kLq-2mZp_Rt7YwXa3bN", "s1t", map[string]bool{
			"t abc": true, "t a-b_c.d": true, "t a b": false, "t ab/c": false, "u abc": false, "t ü": false,
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := learnedWords(t, tc.rec, tc.f)
			if got := slotsOf(e); got != tc.slots {
				t.Fatalf("slots %s, want %s", got, tc.slots)
			}
			for live, want := range tc.live {
				if ok, why := WordsAdmit(tc.rec, live, e); ok != want {
					t.Errorf("%q admitted %v (%s), want %v", live, ok, why, want)
				}
			}
		})
	}
}

// A clock read is one instant slot, by its segment, its layout and its
// skeleton: an instant in the layout, written alike, may stand there; text
// beside it is still compared.
func TestAnAcceptedClockReadIsOneInstantSlot(t *testing.T) {
	later := wAt.Add(FullRoundLead)
	other := wAt.Add(37 * time.Hour)
	for _, tc := range []struct {
		name, layout, rec, f, slots string
		ok, red                     []string
	}{
		{"text beside an RFC 3339 instant", "iso8601",
			"ORD-1 paid 100 at " + wAt.Format(time.RFC3339), "ORD-1 paid 100 at " + later.Format(time.RFC3339),
			"s4i:iso8601:d4-d2-d2Td2:d2:d2FZ",
			[]string{"ORD-1 paid 100 at " + other.Format(time.RFC3339), "ORD-1 paid 100 at " + other.Format("2006-01-02T15:04:05.000000Z")},
			[]string{"ORD-1 paid 999 at " + other.Format(time.RFC3339), "ORD-1 paid 100 at " + other.Format("2006-01-02T15:04Z"), "ORD-1 paid 100 at " + other.Format("2006-01-02T15:04:05+01:00"), "ORD-1 paid 100 at 2026-13-45T10:28:02Z"}},
		{"an HTTP date", "rfc1123", wAt.Format(time.RFC1123), later.Format(time.RFC1123), "s0i:rfc1123:W, d2 M d4 d2:d2:d2 N",
			[]string{other.Format(time.RFC1123), other.Format("Mon, 02 Jan 2006 15:04:05") + " GMT", other.Format("Mon, 02 Jan 2006 15:04:05") + " BST"},
			[]string{other.Format(time.RFC1123Z), other.Format("Mon, 02 Jan 2006 15:04:05") + " INFO"}},
		{"Unix date, its day padded", "unixdate", wAt.Format(time.UnixDate), later.Format(time.UnixDate), "s0i:unixdate:W M D d2:d2:d2 N d4",
			[]string{time.Date(2026, 11, 1, 3, 4, 5, 0, time.UTC).Format(time.UnixDate), other.Format("Mon Jan _2 15:04:05") + " CEST " + other.Format("2006")},
			[]string{other.Format("Mon Jan _2 15:04:05") + " ERROR " + other.Format("2006")}},
		{"JavaScript's Date#toString", "jsdate", wAt.Format("Mon Jan 02 2006 15:04:05 GMT-0700") + " (Coordinated Universal Time)", later.Format("Mon Jan 02 2006 15:04:05 GMT-0700") + " (Coordinated Universal Time)",
			"s0i:jsdate:W M d2 d4 d2:d2:d2 GMT+d4 (N)",
			[]string{other.In(time.FixedZone("", 3600)).Format("Mon Jan 02 2006 15:04:05 GMT-0700") + " (British Summer Time)"},
			[]string{other.Format("Mon Jan 02 2006 15:04:05 GMT-0700") + " (Acme Account Time)"}},
		{"Go's Time.String: a spaced offset and a zone's name", "iso8601", wAt.Format("2006-01-02 15:04:05 -0700 MST"), later.Format("2006-01-02 15:04:05 -0700 MST"),
			"s0i:iso8601:d4-d2-d2 d2:d2:d2F +d4 N",
			[]string{"2026-11-02 11:00:00 +0100 BST", "2026-11-02 11:00:00.5 +0000 GMT"},
			[]string{"2026-11-02 11:00:00 +0000 INFO", "2026-11-02 11:00:00 -0100 UTC"}},
		{"a word after a spaced offset that is no zone's name", "iso8601", wAt.Format("2006-01-02 15:04:05 -0700") + " INFO", later.Format("2006-01-02 15:04:05 -0700") + " INFO",
			"s0i:iso8601:d4-d2-d2 d2:d2:d2F +d4",
			[]string{"2026-11-02 11:00:00 +0000 INFO"},
			[]string{"2026-11-02 11:00:00 +0000 ERROR", "2026-11-02 11:00:00 +0000 info"}},
		{"epoch seconds", "epoch", strconv.FormatInt(wAt.Unix(), 10), strconv.FormatInt(later.Unix(), 10), "s0i:epoch:d10F",
			[]string{strconv.FormatInt(other.Unix(), 10), strconv.FormatInt(other.Unix(), 10) + ".25"},
			[]string{strconv.FormatInt(other.UnixMilli(), 10), "9999999999"}},
		{"epoch milliseconds", "epoch", strconv.FormatInt(wAt.UnixMilli(), 10), strconv.FormatInt(later.UnixMilli()+500, 10), "s0i:epoch:d13",
			[]string{strconv.FormatInt(other.UnixMilli(), 10)},
			[]string{strconv.FormatInt(other.Unix(), 10)}},
		{"a time of day beside text", "timeofday", "at " + wAt.Format("15:04:05") + " sharp", "at " + later.Format("15:04:05") + " sharp", "s1i:timeofday:d2:d2:d2F",
			[]string{"at 23:59:59.999 sharp"},
			[]string{"at 23:59 sharp", "at 23:59:59 late"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := learnedWords(t, tc.rec, tc.f)
			if got := slotsOf(e); got != tc.slots {
				t.Fatalf("slots %s, want %s", got, tc.slots)
			}
			for _, live := range tc.ok {
				if ok, why := WordsAdmit(tc.rec, live, e); !ok {
					t.Errorf("%q refused (%s), want admitted", live, why)
				}
			}
			for _, live := range tc.red {
				if ok, _ := WordsAdmit(tc.rec, live, e); ok {
					t.Errorf("%q admitted, want red", live)
				}
			}
		})
	}
}

// uuid7Of is a UUID v7 of instant at with the given random tail, in upper
// case, its variant nibble v.
func uuid7Of(at time.Time, rand string, variant byte, upper bool) string {
	h := fmt.Sprintf("%012x", at.UnixMilli())
	u := h[:8] + "-" + h[8:12] + "-7" + rand[:3] + "-" + string(variant) + rand[3:6] + "-" + rand[6:18]
	if upper {
		return strings.ToUpper(u)
	}
	return u
}

// A time-ordered UUID is an instant slot in its letters' case, its version
// and a variant of 8, 9, a or b, either case (SF2); a recorded variant
// outside them is refused (N18), and so is one glued to a word.
func TestAUUIDInstantKeepsItsCaseVersionAndVariant(t *testing.T) {
	later := wAt.Add(FullRoundLead)
	other := wAt.Add(3 * time.Hour)
	for _, upper := range []bool{false, true} {
		rec, f := uuid7Of(wAt, "abc123456789abcdef", 'a', upper), uuid7Of(later, "def0123456789abcde", 'b', upper)
		e := learnedWords(t, rec, f)
		want := "s0i:uuid:x8-x4-7x3-vx3-x12"
		if upper {
			want = "s0i:uuid:X8-X4-7X3-VX3-X12"
		}
		if got := slotsOf(e); got != want {
			t.Fatalf("upper %v: slots %s, want %s", upper, got, want)
		}
		for v, ok := range map[byte]bool{'8': true, '9': true, 'a': !upper, 'b': !upper, 'A': upper, 'B': upper, 'c': false, 'C': false, '7': false} {
			live := uuid7Of(other, "0123456789abcdef01", 'a', upper)
			live = live[:19] + string(v) + live[20:]
			if got, why := WordsAdmit(rec, live, e); got != ok {
				t.Errorf("upper %v, variant %c: admitted %v (%s), want %v", upper, v, got, why, ok)
			}
		}
		// The same UUID in the other letter case.
		flipped := uuid7Of(other, "0123456789abcdef01", 'a', !upper)
		if ok, _ := WordsAdmit(rec, flipped, e); ok {
			t.Errorf("upper %v: a UUID in the other case admitted", upper)
		}
		v4 := uuid7Of(other, "0123456789abcdef01", 'a', upper)
		v4 = v4[:14] + "4" + v4[15:]
		if ok, _ := WordsAdmit(rec, v4, e); ok {
			t.Errorf("upper %v: a UUID of another version admitted", upper)
		}
	}
	for _, v := range []byte{'c', 'C', '0', 'f'} {
		rec, f := uuid7Of(wAt, "abc123456789abcdef", v, false), uuid7Of(later, "def0123456789abcde", v, false)
		if _, why := NewDriftCache().Words(rec, f, nil); !strings.Contains(why, "variant") {
			t.Errorf("recorded variant %c: learned (%q), want refused (N18)", v, why)
		}
	}
	glued := "x" + uuid7Of(wAt, "abc123456789abcdef", 'a', false)
	if _, why := NewDriftCache().Words(glued, "x"+uuid7Of(later, "def0123456789abcde", 'a', false), nil); why != whyGluedUUID {
		t.Errorf("a UUID glued to a word: %q, want %q", why, whyGluedUUID)
	}
}

// An instant slot is selected by its segment and layout, never by which
// match of its layout it is: a neighbour that reads as an epoch in one run
// and not in another (an id of 18 or 19 digits) moves no slot.
func TestAnInstantSlotIsSelectedBySegmentNotByMatch(t *testing.T) {
	later := wAt.Add(FullRoundLead)
	rec := "id 123456789012345678 at " + strconv.FormatInt(wAt.UnixMilli(), 10)
	f := "id 123456789012345679 at " + strconv.FormatInt(later.UnixMilli(), 10)
	e := learnedWords(t, rec, f)
	if got := slotsOf(e); got != "s1r0 s3i:epoch:d13" {
		t.Fatalf("slots %s", got)
	}
	for live, want := range map[string]bool{
		"id 1234567890123456789 at " + strconv.FormatInt(later.UnixMilli(), 10): true,
		"id 12 at " + strconv.FormatInt(later.UnixMilli(), 10):                  true,
		"id 12 at 99": false,
	} {
		if ok, why := WordsAdmit(rec, live, e); ok != want {
			t.Errorf("%q admitted %v (%s), want %v", live, ok, why, want)
		}
	}
}

// What is refused, each with its reason: a value whose parts changed, a word
// that moved or changed only its case, one beyond ASCII, one after an escape,
// one inside an instant that is no accepted clock read (its zone changed, or
// it moved otherwise than F's clock), and, between the slots of one value, a
// changed word that repeats or is glued inside another.
func TestWhatWordsCannotBeLearned(t *testing.T) {
	later := wAt.Add(FullRoundLead)
	plus1 := time.FixedZone("", 3600)
	for _, tc := range []struct {
		name, rec, live, why string
		values               []string
	}{
		{"another number of parts", "a b c", "a b c d", whyParts, nil},
		{"a separator changed", "a, b", "a ,b", whySeparators, nil},
		{"two clock reads swapped places", wAt.Format(time.RFC3339) + " " + strconv.FormatInt(wAt.Unix(), 10), strconv.FormatInt(later.Unix(), 10) + " " + later.Format(time.RFC3339), whyReadOrder, nil},
		{"swapped ids", "A7K B8L x1", "B8L A7K x2", whyMoved, []string{"A7K", "B8L"}},
		{"only the case", "ref=ab12", "ref=AB12", whyCase, nil},
		{"beyond ASCII in a run", "id café1", "id café2", whyNotASCII, nil},
		{"beyond ASCII where the runs drifted", "id 订单A7", "id 订单-B8x", whyScript, nil},
		{"after a percent escape", "q=a%20ref1", "q=a%20ref2", whyEscape, nil},
		{"a token holding a percent", "t a%b-c_1", "t a%b_c-d_2", whyEscape, nil},
		{"an instant whose zone changed", "at " + wAt.Format(time.RFC3339), "at " + later.In(plus1).Format(time.RFC3339), whyZone, nil},
		{"an instant that did not move as F's clock does", "at " + wAt.Format(time.RFC3339), "at " + wAt.Add(3*time.Hour).Format(time.RFC3339), whyInstant, nil},
		{"an epoch that moved otherwise", "ts " + strconv.FormatInt(wAt.Unix(), 10), "ts " + strconv.FormatInt(wAt.Unix()+3600, 10), whyInstant, nil},
		{"an instant written otherwise", strconv.FormatInt(wAt.Unix(), 10), strconv.FormatInt(later.UnixMilli(), 10), whySkeleton, nil},
		{"a changed word that repeats", "id=x1 ref=x1", "id=y2 ref=y2", whyRepeat, nil},
		{"a changed word glued inside another", "Invoice INV1042 for order 1042", "Invoice INV2077 for order 2077", whyGlued, []string{"1042", "2077"}},
		{"two pieces glued, one each", "Invoice INV1042 for order ORD1042", "Invoice INV2077 for order ORD2077", whyGlued, nil},
		{"it did not change", "a", "a", whySame, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, why := NewDriftCache().Words(tc.rec, tc.live, nil)
			if why != tc.why {
				t.Fatalf("%q -> %q: %q, want %q", tc.rec, tc.live, why, tc.why)
			}
			for _, v := range tc.values {
				if ContainsToken(why, v) {
					t.Errorf("the reason %q holds %q of the value", why, v)
				}
			}
		})
	}
}

// Two clock reads other than UUIDs in one value may repeat (N2): they are no
// tie, and each is its own instant slot.
func TestTwoInstantsOfOneValueAreNoRepeat(t *testing.T) {
	later := wAt.Add(FullRoundLead)
	rec := wAt.Format(time.RFC3339) + " / " + wAt.Format(time.RFC3339)
	f := later.Format(time.RFC3339) + " / " + later.Format(time.RFC3339)
	if got := slotsOf(learnedWords(t, rec, f)); got != "s0i:iso8601:d4-d2-d2Td2:d2:d2FZ s1i:iso8601:d4-d2-d2Td2:d2:d2FZ" {
		t.Fatalf("slots %s", got)
	}
}

// The changed piece strips a word's unchanged letter or digit sub-runs from
// either end, one left on each side at least.
func TestTheChangedPiece(t *testing.T) {
	for _, tc := range []struct{ r, f, wr, wf string }{
		{"INV1042", "INV2077", "1042", "2077"},
		{"1042A", "2077A", "1042", "2077"},
		{"p12x1042", "p9x5678", "12x1042", "9x5678"},
		{"3f2b8c1e9a4d", "a7c4e2d10b3f", "3f2b8c1e9a4d", "a7c4e2d10b3f"},
		{"INV1042", "INV1042A", "1042", "1042A"},
		{"a", "b", "a", "b"},
		{"seq17", "seq18", "17", "18"},
	} {
		p, ok := changedPiece(tc.r, tc.f)
		if !ok || p.rec != tc.wr || p.live != tc.wf {
			t.Errorf("piece of %q -> %q is %q -> %q (%v), want %q -> %q", tc.r, tc.f, p.rec, p.live, ok, tc.wr, tc.wf)
		}
	}
	if _, ok := changedPiece("a", "a"); ok {
		t.Error("a word that did not change has a piece")
	}
}

// Glued containment (B1): either value's changed piece lies inside the
// other's, starting and ending at a class boundary of the container, at one
// offset recorded and live, from its start or its end.
func TestGluedContainment(t *testing.T) {
	for _, tc := range []struct {
		name string
		a, b [2]string
		want bool
	}{
		{"ORD123 and 123", [2]string{"ORD123", "ORD456"}, [2]string{"123", "456"}, true},
		{"INV1042 and 1042", [2]string{"INV1042", "INV2077"}, [2]string{"1042", "2077"}, true},
		{"INV1042 and ORD1042", [2]string{"INV1042", "INV2077"}, [2]string{"ORD1042", "ORD2077"}, true},
		{"1042A and ORD1042", [2]string{"1042A", "2077A"}, [2]string{"ORD1042", "ORD2077"}, true},
		{"end-aligned: p12x1042 and 1042", [2]string{"p12x1042", "p9x5678"}, [2]string{"1042", "5678"}, true},
		{"start-aligned: 1042x12q and 1042", [2]string{"1042x12q", "5678x9q"}, [2]string{"1042", "5678"}, true},
		{"either order", [2]string{"123", "456"}, [2]string{"ORD123", "ORD456"}, true},
		{"no class boundary (N17)", [2]string{"12347", "56785"}, [2]string{"1234", "5678"}, false},
		{"another offset live", [2]string{"x1042y", "xy5678"}, [2]string{"1042", "5678"}, false},
		{"unrelated", [2]string{"3f2b8c1e9a4d", "a7c4e2d10b3f"}, [2]string{"17", "18"}, false},
	} {
		pa, _ := changedPiece(tc.a[0], tc.a[1])
		pb, _ := changedPiece(tc.b[0], tc.b[1])
		if got := gluedPieces(pa, pb); got != tc.want {
			t.Errorf("%s: glued %v, want %v", tc.name, got, tc.want)
		}
	}
}

// How two places of one call stand (TieOf): the instant check first, then
// sources, which the glued test is part of; r9 §8's rows.
func TestTieOf(t *testing.T) {
	later := wAt.Add(FullRoundLead)
	v7, v7f := uuid7Of(wAt, "abc123456789abcdef", 'a', false), uuid7Of(later, "def0123456789abcde", 'b', false)
	for _, tc := range []struct {
		name string
		a, b [2]string
		want Tie
	}{
		{"ORD123 and 123", [2]string{"ORD123", "ORD456"}, [2]string{"123", "456"}, TiePartial},
		{"INV1042 and 1042", [2]string{"INV1042", "INV2077"}, [2]string{"1042", "2077"}, TiePartial},
		{"INV1042 and ORD1042", [2]string{"INV1042", "INV2077"}, [2]string{"ORD1042", "ORD2077"}, TiePartial},
		{"1042A and ORD1042", [2]string{"1042A", "2077A"}, [2]string{"ORD1042", "ORD2077"}, TiePartial},
		{"p12x1042 beside 1042", [2]string{"p12x1042", "p9x5678"}, [2]string{"1042", "5678"}, TiePartial},
		{"a template key INV1042 and a slot ORD1042", [2]string{"INV1042", "INV2077"}, [2]string{"note ORD1042", "note ORD2077"}, TiePartial},
		{"ORD 12347 beside 1234 (N17)", [2]string{"ORD 12347", "ORD 56785"}, [2]string{"1234", "5678"}, TieNone},
		{"seq=17 beside a fresh hex", [2]string{"seq=17", "seq=18"}, [2]string{"3f2b8c1e9a4d70a1", "a7c4e2d10b3f4c2e"}, TieNone},
		{"createdAt and updatedAt", [2]string{wAt.Format(time.RFC3339), later.Format(time.RFC3339)}, [2]string{wAt.Format(time.RFC3339), later.Format(time.RFC3339)}, TieExempt},
		{"an instant beside text and the same instant", [2]string{"at " + wAt.Format(time.RFC3339), "at " + later.Format(time.RFC3339)}, [2]string{wAt.Format(time.RFC3339), later.Format(time.RFC3339)}, TieExempt},
		{"a UUID v7 in a path and a body", [2]string{v7, v7f}, [2]string{v7, v7f}, TieWhole},
		{"one id at two places", [2]string{"o-1111", "o-9999"}, [2]string{"o-1111", "o-9999"}, TieWhole},
		{"two fresh ids", [2]string{"r-3f2b8c", "r-a7c4e2"}, [2]string{"n-91d0e3", "n-5be7aa"}, TieNone},
	} {
		if got := NewDriftCache().TieOf(tc.a, tc.b, nil); got != tc.want {
			t.Errorf("%s: %v, want %v", tc.name, got, tc.want)
		}
		if got := NewDriftCache().TieOf(tc.b, tc.a, nil); got != tc.want {
			t.Errorf("%s, the other order: %v, want %v", tc.name, got, tc.want)
		}
	}
}

// Across calls, the glued test is part of sources: a value glued inside an
// earlier call's is a variable inside a longer value, which keploy cannot
// follow, never noise.
func TestAGluedValueOfAnEarlierCallIsFound(t *testing.T) {
	earlier := []Earlier{{Mock: "mock-0", Own: true, Seen: []Seen{{Side: Request, Leaf: Leaf{Path: "body.invoice", Type: String, Value: "INV1042"}, Live: "INV2077", Place: "body.invoice"}}}}
	live := Shape{Leaves: map[string]Leaf{"body.order": {Path: "body.order", Type: String, Value: "ORD2077"}}}
	rec := Shape{Leaves: map[string]Leaf{"body.order": {Path: "body.order", Type: String, Value: "ORD1042"}}}
	got := Classify(nil, []string{"body.order"}, live, rec, wAt.Add(time.Second), earlier, nil, nil)
	if got[0].Kind != KindVariable || got[0].From.Whole {
		t.Fatalf("learned %+v, want a variable from inside mock-0's value", got[0])
	}
}

// The seeded rates of N16: a changed piece of one or two digits lands,
// class-bounded and anchored, inside a fresh 32-hex value at the rates the
// design states (about 3.4e-3 and 1.1e-5 per value). Counted over seeded
// values, so the count is the same on every run.
func TestN16CoincidenceRates(t *testing.T) {
	r := rand.New(rand.NewSource(16))
	hexOf := func() string {
		const digits = "0123456789abcdef"
		b := make([]byte, 32)
		for i := range b {
			b[i] = digits[r.Intn(16)]
		}
		return string(b)
	}
	const n = 200000
	one, two := 0, 0
	for i := 0; i < n; i++ {
		c := run{hexOf(), hexOf()}
		if gluedPieces(c, run{"7", "8"}) {
			one++
		}
		if gluedPieces(c, run{"17", "18"}) {
			two++
		}
	}
	rateOne, rateTwo := float64(one)/n, float64(two)/n
	if rateOne < 2e-3 || rateOne > 5e-3 {
		t.Errorf("a one-digit piece hits %.2e of fresh 32-hex values, want about 3.4e-3", rateOne)
	}
	if rateTwo > 1e-4 {
		t.Errorf("a two-digit piece hits %.2e of fresh 32-hex values, want about 1.1e-5", rateTwo)
	}
	t.Logf("N16: one digit %.2e, two digits %.2e, over %d seeded values", rateOne, rateTwo, n)
}

// An entry that does not hold is void, with its reason: it fails closed.
func TestAnEntryThatDoesNotHoldIsVoid(t *testing.T) {
	later := wAt.Add(FullRoundLead)
	rec, f := "ORD-1 paid 100 ref=a1 at "+wAt.Format(time.RFC3339), "ORD-1 paid 100 ref=b2 at "+later.Format(time.RFC3339)
	e := learnedWords(t, rec, f)
	with := func(change func(*models.WordsEntry)) models.WordsEntry {
		c := e.Clone()
		change(&c)
		return c
	}
	for _, tc := range []struct {
		name string
		rec  string
		e    models.WordsEntry
		why  string
	}{
		{"another recorded value", "ORD-2 paid 100 ref=a1 at " + wAt.Format(time.RFC3339), e, whyDigest},
		{"a digest guard missing", rec, with(func(c *models.WordsEntry) { c.Digest = "" }), whyGuard},
		{"a layout guard malformed", rec, with(func(c *models.WordsEntry) { c.Layout = "XYZ" }), whyGuard},
		{"another layout", rec, with(func(c *models.WordsEntry) { c.Layout = "0000000000000000" }), whyLayoutGuard},
		{"a void entry", rec, models.VoidWords(e.Digest), whyVoidMerged},
		{"a segment out of range", rec, with(func(c *models.WordsEntry) { c.Slots[0].Seg = 99 }), whyRange},
		{"a run out of range", rec, with(func(c *models.WordsEntry) { c.Slots[0].Run = 7 }), whyRange},
		{"a kind it does not know", rec, with(func(c *models.WordsEntry) { c.Slots[0].Kind = "shape" }), whyKind},
		{"a layout it does not know", rec, with(func(c *models.WordsEntry) { c.Slots[1].Layout = "iso9999" }), whyKind},
		{"a skeleton that is not the recorded one", rec, with(func(c *models.WordsEntry) { c.Slots[1].Skeleton = "d4-d2-d2" }), whyRecSkeleton},
		{"an instant slot where none is", rec, with(func(c *models.WordsEntry) { c.Slots[1].Seg = 0 }), whyRange},
	} {
		ok, why := WordsAdmit(tc.rec, f, tc.e)
		if ok || why != tc.why {
			t.Errorf("%s: admitted %v (%q), want void %q", tc.name, ok, why, tc.why)
		}
		if ok, _ := WordsAdmit(tc.rec, tc.rec, tc.e); ok && tc.rec == rec {
			t.Errorf("%s: a void entry admits even the recording", tc.name)
		}
	}
}

// WholeDrift judges by runs: a leaf is whole when every run of it changed
// and no changed run repeats (N14: a one-run value that changed anywhere).
func TestWholeDriftByRuns(t *testing.T) {
	later := wAt.Add(FullRoundLead)
	for _, tc := range []struct {
		name, rec, live string
		whole           bool
	}{
		{"a fresh id", "3f2b8c1e9a4d", "a7c4e2d10b3f", true},
		{"a fresh UUID v4", "1b4e28ba-2fa1-41d2-883f-0016d3cca427", "6fa459ea-ee8a-4ca4-894e-db77e160355e", true},
		{"one run glued (N14)", "ORD123paid100", "ORD124paid100", true},
		{"a lone instant", wAt.Format(time.RFC3339), later.Format(time.RFC3339), true},
		{"a timestamps: true field", wAt.Format("2006-01-02T15:04:05.000Z"), later.Format("2006-01-02T15:04:05.000Z"), true},
		{"a partial note", "ORD-1 paid 100 ref=a1", "ORD-1 paid 100 ref=b2", false},
		{"text beside an instant", "created " + wAt.Format(time.RFC3339), "created " + later.Format(time.RFC3339), false},
		{"a run that repeats", "ab ab", "cd cd", false},
		{"two runs that became one", "ab cd", "ef ef", false},
		{"a sign", "-5", "5", false},
		{"no word", "--", "---", false},
		{"unchanged", "a", "a", false},
	} {
		whole, why := WholeDrift(tc.rec, tc.live, nil)
		if whole != tc.whole || whole == (why != "") {
			t.Errorf("%s: whole %v (%q), want %v", tc.name, whole, why, tc.whole)
		}
	}
	// Within the call's window, an instant the lead does not read is one.
	boot := wAt.Add(4 * time.Minute)
	w := &Window{Rec: Span{From: wAt.Add(-time.Second), To: wAt.Add(time.Second)}, F: Span{From: boot.Add(-time.Second), To: boot.Add(time.Second)}}
	if whole, _ := WholeDrift("at "+wAt.Format(time.RFC3339), "at "+boot.Format(time.RFC3339), w); whole {
		t.Error("text beside an instant read within the window is whole")
	}
	if whole, why := WholeDrift(wAt.Format(time.RFC3339), boot.Format(time.RFC3339), w); !whole {
		t.Errorf("an instant read within the window is not whole: %s", why)
	}
}

// A value is read the same in every run, and the words learned of it are a
// function of the two values alone.
func TestWordsAreDeterministic(t *testing.T) {
	later := wAt.Add(FullRoundLead)
	rec, f := "ORD-1 paid 100 ref=a1 at "+wAt.Format(time.RFC3339)+" via 订单A7", "ORD-1 paid 100 ref=b2 at "+later.Format(time.RFC3339)+" via 订单A7"
	first := learnedWords(t, rec, f)
	for i := 0; i < 20; i++ {
		e, why := NewDriftCache().Words(rec, f, nil)
		if why != "" || e.Digest != first.Digest || e.Layout != first.Layout || slotsOf(e) != slotsOf(first) {
			t.Fatalf("read %d: %+v %q, first %+v", i, e, why, first)
		}
	}
}

// The classes split runs as declared: a script change is a run boundary,
// Common is its own class, and a mark joins its letter.
func TestRunsSplitByClass(t *testing.T) {
	for _, tc := range []struct {
		s    string
		want []string
	}{
		{"ab12-cd_9", []string{"ab12:Latin", "cd:Latin", "9:Latin"}},
		{"订单A7K已发货", []string{"订单:Han", "A7K:Latin", "已发货:Han"}},
		{"ナー12", []string{"ナ:Katakana", "ー:Common", "12:Latin"}},
		{"µ5", []string{"µ:Common", "5:Latin"}},
		{"caféx", []string{"caféx:Latin"}},
		{"a.b@c~d!e*f$g^h%i", []string{"a:Latin", "b:Latin", "c:Latin", "d:Latin", "e:Latin", "f:Latin", "g:Latin", "h:Latin", "i:Latin"}},
	} {
		var got []string
		for _, r := range runsOf(tc.s, 0, len(tc.s)) {
			got = append(got, tc.s[r.from:r.to]+":"+r.class)
		}
		if strings.Join(got, " ") != strings.Join(tc.want, " ") {
			t.Errorf("runs of %q: %v, want %v", tc.s, got, tc.want)
		}
	}
	segs := tokenize(`a{"b": [c, d]} e=f/g?h#i+j|k;l&m<n>o\p'q`+"`r", nil).segs
	if len(segs) != 18 {
		t.Errorf("%d segments, want one per word between structural characters", len(segs))
	}
}

// Every declared zone abbreviation is an N where a layout writes a zone's
// name, read at offset 0 the same on every host; a word outside the list is
// no zone.
func TestEveryDeclaredZoneNameIsN(t *testing.T) {
	unix, rfc := clockLayouts[layoutNamed("unixdate")], clockLayouts[layoutNamed("rfc1123")]
	for _, z := range zoneAbbrevs {
		s := "Wed Oct 21 10:28:02 " + z + " 2026"
		if m := unix.re.FindString(s); m != s || unix.skeleton(m) != "W M D d2:d2:d2 N d4" {
			t.Errorf("%q: Unix date reads %q, skeleton %q", s, m, unix.skeleton(m))
		}
		if tm, _, ok := unix.read(s); !ok || !tm.Equal(wAt) {
			t.Errorf("%q read as %v (%v), want at offset 0", s, tm, ok)
		}
		h := "Wed, 21 Oct 2026 10:28:02 " + z
		if m := rfc.re.FindString(h); m != h || rfc.skeleton(m) != "W, d2 M d4 d2:d2:d2 N" {
			t.Errorf("%q: RFC 1123 reads %q", h, m)
		}
		if tm, _, ok := rfc.read(h); !ok || !tm.Equal(wAt) {
			t.Errorf("%q read as %v (%v), want at offset 0", h, tm, ok)
		}
	}
	js := clockLayouts[layoutNamed("jsdate")]
	for _, n := range zoneLongNames {
		s := "Wed Oct 21 2026 10:28:02 GMT+0000 (" + n + ")"
		if m := js.re.FindString(s); m != s || js.skeleton(m) != "W M d2 d4 d2:d2:d2 GMT+d4 (N)" {
			t.Errorf("%q: JavaScript reads %q", s, m)
		}
	}
	for _, s := range []string{"Wed Oct 21 10:28:02 INFO 2026", "Wed, 21 Oct 2026 10:28:02 ERROR"} {
		if m := unix.re.FindString(s); m == s {
			t.Errorf("%q: a word outside the list read as a zone", s)
		}
		if m := rfc.re.FindString(s); m == s {
			t.Errorf("%q: a word outside the list read as a zone", s)
		}
	}
	if m := js.re.FindString("Wed Oct 21 2026 10:28:02 GMT+0000 (Acme Time)"); strings.Contains(m, "Acme") {
		t.Errorf("a name outside the list read as a zone: %q", m)
	}
}

// An entry this build cannot read (written by a newer keploy, or with no
// version) admits nothing, and says why.
func TestAnEntryThisBuildCannotReadAdmitsNothing(t *testing.T) {
	slots := []models.WordSlot{{Seg: 0}}
	for _, e := range []models.WordsEntry{
		{Digest: "9f2c41d07a3be815", Layout: "4be1c09a5d3e7f21", Slots: slots},
		{V: models.WordsVersion + 1},
		{V: models.WordsVersion, Digest: "9f2c41d07a3be815", Layout: "4be1c09a5d3e7f21", Slots: slots, Unknown: `{"only":[1]}`},
	} {
		if ok, why := WordsAdmit("ORD-1 ref=a1", "ORD-1 ref=b2", e); ok || why == "" || why != e.Unreadable() {
			t.Errorf("WordsAdmit(%+v) = %v, %q: want a refusal saying %q", e, ok, why, e.Unreadable())
		}
	}
}
