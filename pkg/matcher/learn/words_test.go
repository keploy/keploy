package learn

import (
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"math/rand"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

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
	if ok, why := WordsAdmit(rec, live, e); !ok || why != "" {
		t.Fatalf("%q -> %q: the words learned of F do not admit F: %v %s", rec, live, ok, why)
	}
	if ok, why := WordsAdmit(rec, rec, e); !ok || why != "" {
		t.Fatalf("%q -> %q: the words learned do not admit the recording: %v %s", rec, live, ok, why)
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
		{" ORD-1 paid 100 ref=0c9d8e7f6a5b", false},
		{"\"ORD-1 paid 100 ref=0c9d8e7f6a5b", false},
		{"ORD-1 paid 100 ref=0c9d8e7f6a5b#", false},
		{"ORD-1 paid 100 ref=ünï", false},
		{"ORD-1 paid 100 ref=a.b-c_d", true}, // the run alone in its segment: any ASCII token (N1)
	} {
		if ok, why := WordsAdmit(rec, tc.live, e); ok != tc.ok || ok == (why != "") {
			t.Errorf("%q admitted %v (%s), want %v", tc.live, ok, why, tc.ok)
		}
	}
	// Each red value says what of it did not hold, read by the first way
	// its instants were placed.
	for live, want := range map[string]string{
		"ORD-1 paid 999 ref=0c9d": "a part of it that is not learned to change changed",
		"ORD-1 paid 100 ref=":     "it holds another number of parts than when it was recorded",
	} {
		if ok, why := WordsAdmit(rec, live, e); ok || why != want {
			t.Errorf("%q admitted %v (%q), want %q", live, ok, why, want)
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
			"id ab-99-cd.": false, "id ab-99-cd-": false, "id .ab-99-cd": false,
			"id ab-é9-cd": false, "id ab-9é-cd": false,
		}},
		// The segment's runs are counted: a run added after the slot, or
		// before it, is a word that was not there, never compared.
		// What stands after a segment's last run is fixed, as what stands
		// between its runs is.
		{"a run among others, the separator after its last fixed", "id ab-12-cd.", "id ab-34-cd.", "s1r1", map[string]bool{
			"id ab-99-cd.": true, "id ab-99-cd..": false, "id ab-99-cd": false, "id ab-99-cd.-": false, "id ab-99-cd-": false,
		}},
		// A value's parts are counted: one that ends in a structural character
		// holds no more parts after it.
		{"a value's parts counted after a trailing structural character", "ORD-1 ref=abc;", "ORD-1 ref=xyz;", "s2r0", map[string]bool{
			"ORD-1 ref=k9;": true, "ORD-1 ref=k9;DROP TABLE x": false, "ORD-1 ref=k9;x": false, "ORD-1 ref=k9; x": false, "ORD-1 ref=k9": false,
		}},
		{"a run among others, its segment's runs counted", "ORD-1 paid", "ORD-2 paid", "s0r1", map[string]bool{
			"ORD-9 paid": true, "ORD-9X paid": true, "ORD-9-X paid": false, "X-ORD-9 paid": false, "ORD paid": false,
		}},
		{"a run alone in its segment", "k=v1", "k=v2", "s1r0", map[string]bool{
			"k=v3": true, "k=a-b.c": true, "k=": false, "k=a b": false, "k=é": false, "j=v3": false,
		}},
		{"a run alone, separators around it fixed", "k=-v1-", "k=-v2-", "s1r0", map[string]bool{
			"k=-x_y-": true, "k=v3": false, "k=-v3": false, "k=--": false, "k=-": false, "k=-v3-x": false, "k=x-v3-": false, "k=-\xff-": false, "k=-\xffa-": false, "k=-\x80-": false,
		}},
		{"a segment whose runs drifted", "t V1StGXR8_Z5jdHi6B-myT", "t x9kLq-2mZp_Rt7YwXa3bN", "s1t", map[string]bool{
			"t abc": true, "t a-b_c.d": true, "t a b": false, "t ab/c": false, "u abc": false, "t ü": false, "t \xffbc": false, "t \xff": false, "t \x80": false, "t a\x80": false,
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
		// An instant after a separator that is no structural one ('-') is one
		// segment of its own: the text before it ends where it begins.
		{"an instant after a '-'", "iso8601", "ref x-" + wAt.Format(time.RFC3339), "ref x-" + later.Format(time.RFC3339), "s2i:iso8601:d4-d2-d2Td2:d2:d2FZ",
			[]string{"ref x-" + other.Format(time.RFC3339)},
			[]string{"ref y-" + other.Format(time.RFC3339), "ref x_" + other.Format(time.RFC3339)}},
		// An instant is read at its own segment alone: one that moved to the next
		// segment is red.
		{"an instant between words", "iso8601", "at " + wAt.Format(time.RFC3339) + " x", "at " + later.Format(time.RFC3339) + " x", "s1i:iso8601:d4-d2-d2Td2:d2:d2FZ",
			[]string{"at " + other.Format(time.RFC3339) + " x"},
			[]string{"at x " + other.Format(time.RFC3339), "x " + other.Format(time.RFC3339) + " at"}},
		// Two instants with nothing between them are two reads, in order.
		{"two instants with nothing between them", "jsdate", wAt.Format("Mon Jan 02 2006 15:04:05 GMT-0700") + " (Coordinated Universal Time)" + strconv.FormatInt(wAt.Unix(), 10),
			later.Format("Mon Jan 02 2006 15:04:05 GMT-0700") + " (Coordinated Universal Time)" + strconv.FormatInt(later.Unix(), 10),
			"s0i:jsdate:W M d2 d4 d2:d2:d2 GMT+d4 (N) s1i:epoch:d10F",
			[]string{other.Format("Mon Jan 02 2006 15:04:05 GMT-0700") + " (Coordinated Universal Time)" + strconv.FormatInt(other.Unix(), 10)},
			[]string{other.Format("Mon Jan 02 2006 15:04:05 GMT-0700") + " (Coordinated Universal Time) " + strconv.FormatInt(other.Unix(), 10)}},
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
			// A value with no instant of the layout where one was says so.
			if ok, why := WordsAdmit(tc.rec, "no instant here", e); ok || why == "" {
				t.Errorf("a value with no instant: admitted %v (%q)", ok, why)
			}
		})
	}
	e := learnedWords(t, "at "+wAt.Format(time.RFC3339), "at "+later.Format(time.RFC3339))
	if ok, why := WordsAdmit("at "+wAt.Format(time.RFC3339), "at soon", e); ok || why != "an instant of it is not where it was recorded, in its layout" {
		t.Errorf("no instant where one was recorded: admitted %v (%q)", ok, why)
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

// mixedCase is u with its last group's letters in upper case: a UUID that
// mixes cases, which uuidSkeleton reads as none.
func mixedCase(u string) string { return u[:24] + strings.ToUpper(u[24:]) }

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

// Each layout's skeleton, of every month's and every weekday's name: the
// names are classes, the digits their lengths, Unix date's padded day D
// (with a numeric offset too), a fraction F (a ',' as well as a '.').
func TestEachLayoutsSkeleton(t *testing.T) {
	for m := time.January; m <= time.December; m++ {
		for d := 1; d <= 7; d++ {
			at := time.Date(2026, m, d+9*(d%2), 3, 4, 5, 0, time.UTC)
			for name, tc := range map[string][2]string{
				"unixdate": {at.Format(time.UnixDate), "W M D d2:d2:d2 N d4"},
				"rfc1123":  {at.Format(time.RFC1123), "W, d2 M d4 d2:d2:d2 N"},
				"jsdate":   {at.Format("Mon Jan 02 2006 15:04:05 GMT-0700") + " (Coordinated Universal Time)", "W M d2 d4 d2:d2:d2 GMT+d4 (N)"},
			} {
				lay := clockLayouts[layoutNamed(name)]
				if m := lay.re.FindString(tc[0]); m != tc[0] {
					t.Fatalf("%s: %q reads %q", name, tc[0], m)
				}
				if got := lay.skeleton(tc[0]); got != tc[1] {
					t.Errorf("%s: skeleton of %q is %q, want %q", name, tc[0], got, tc[1])
				}
			}
		}
	}
	for _, tc := range []struct{ layout, m, want string }{
		{"unixdate", "Wed Oct 21 10:28:02 +0000 2026", "W M D d2:d2:d2 +d4 d4"},
		{"unixdate", "Wed Oct  1 10:28:02 2026", "W M D d2:d2:d2 d4"},
		{"iso8601", "2026-10-21T10:28:02,123Z", "d4-d2-d2Td2:d2:d2FZ"},
		{"iso8601", "2026-10-21T10:28:02.5+01:00", "d4-d2-d2Td2:d2:d2F+d2:d2"},
		{"iso8601", "2026-10-21 10:28:02 +0100 BST", "d4-d2-d2 d2:d2:d2F +d4 N"},
		{"epoch", "1761042482.25", "d10F"},
		{"epoch", "1761042482250", "d13"},
		{"timeofday", "10:28:02.5", "d2:d2:d2F"},
		{"timeofday", "10:28", "d2:d2"},
	} {
		lay := clockLayouts[layoutNamed(tc.layout)]
		if m := lay.re.FindString(tc.m); m != tc.m {
			t.Fatalf("%s: %q reads %q", tc.layout, tc.m, m)
		}
		if got := lay.skeleton(tc.m); got != tc.want {
			t.Errorf("%s: skeleton of %q is %q, want %q", tc.layout, tc.m, got, tc.want)
		}
	}
	for u, want := range map[string]string{
		"0192b3c4-5d6e-7abc-a123-456789abcdef": "x8-x4-7x3-vx3-x12",
		"0192B3C4-5D6E-7ABC-A123-456789ABCDEF": "X8-X4-7X3-VX3-X12",
		"01923334-5162-7123-8123-456789012345": "x8-x4-7x3-vx3-x12",
		"a192B3C4-5D6E-7ABC-A123-456789ABCDEF": "",
		"0192B3C4-5D6E-7ABC-A123-456789ABCDEf": "",
		"f0000000-0000-7000-8000-00000000000A": "",
		"F0000000-0000-1000-8000-00000000000a": "",
	} {
		if got := uuidSkeleton(u); got != want {
			t.Errorf("uuidSkeleton(%q) = %q, want %q", u, got, want)
		}
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

// WordsAdmit tries at most maxCollapses ways to place a live value's
// instants; a value with more is refused, and says so: a way it did not try
// might have admitted it, so no way it tried says why it is not.
func TestAValueOfTooManyInstantsToPlaceIsRefusedSayingSo(t *testing.T) {
	later := wAt.Add(FullRoundLead)
	at := func(t time.Time) string { return t.Format(time.RFC3339) }
	rec := "from " + at(wAt) + " to " + at(wAt.Add(time.Second))
	e := learnedWords(t, rec, "from "+at(later)+" to "+at(later.Add(time.Second)))
	if got := slotsOf(e); got != "s1i:iso8601:d4-d2-d2Td2:d2:d2FZ s3i:iso8601:d4-d2-d2Td2:d2:d2FZ" {
		t.Fatalf("slots %s", got)
	}
	// 24 instants hold C(24, 2) = 276 ways to place two.
	many := make([]string, 24)
	for i := range many {
		many[i] = at(later.Add(time.Duration(i) * time.Minute))
	}
	if ok, why := WordsAdmit(rec, strings.Join(many, " "), e); ok || why != whyTooMany {
		t.Fatalf("24 instants admitted %v (%s), want refused: %s", ok, why, whyTooMany)
	}
	// A few are placed, and judged by the ways tried.
	if ok, why := WordsAdmit(rec, strings.Join(many[:3], " "), e); ok || why == whyTooMany {
		t.Fatalf("3 instants admitted %v (%s), want refused by a way tried", ok, why)
	}
}

// The words format's guards are a format (models.WordsVersion 1): the
// digest and the layout of a recorded value, and the grammar the layout is
// read under (the structural characters, the declared zone names and the
// clock layouts' names), stand in every mock file F wrote. A change to any
// of them voids every entry already written, so each is pinned here: a
// change that moves one is a new version of the format, never a quiet one.
func TestTheWordsGuardsAreStable(t *testing.T) {
	if got := hex.EncodeToString(wordsGrammar); got != "17773c72a7389b6a9858deddbbc13a39ff4793215bcb4bdc82a53fab16c81c1f" {
		t.Errorf("the words grammar hashes to %s: a new words format, not version %d", got, models.WordsVersion)
	}
	later := wAt.Add(FullRoundLead)
	for _, tc := range []struct {
		rec, f, digest, layout, slots string
	}{
		{"ORD-1 paid 100 ref=3f2b8c1e9a4d", "ORD-1 paid 100 ref=a7c4e2d10b3f", "b4ee399075dabe99", "c3322fff20d9bf6e", "s4r0"},
		{"ORD-1 paid 100 at " + wAt.Format(time.RFC3339), "ORD-1 paid 100 at " + later.Format(time.RFC3339), "f096738e7176bdce", "f01da47dec79058a", "s4i:iso8601:d4-d2-d2Td2:d2:d2FZ"},
		{"订单A7K 已发货 x1", "订单A7K 已发货 x2", "e2a8763b0616aece", "e48fb7b0934866df", "s2r0"},
	} {
		e := learnedWords(t, tc.rec, tc.f)
		if e.Digest != tc.digest || e.Layout != tc.layout || slotsOf(e) != tc.slots {
			t.Errorf("%q: digest %s layout %s slots %s, want %s %s %s", tc.rec, e.Digest, e.Layout, slotsOf(e), tc.digest, tc.layout, tc.slots)
		}
	}
}

// What a declared layout reads across the edge of an accepted read is no
// instant of its own: "+01:00:30" after an RFC 3339 instant holds a time
// of day ("01:00:30") that begins inside the read, and the word after it
// that changed is learned. A clock read right after an escape is still
// one, and a UUID-shaped word that reads as no instant (its clock before
// 2000) is plain words, learned where they changed.
func TestWhatStandsBesideAClockReadIsLearned(t *testing.T) {
	later := wAt.Add(FullRoundLead)
	plus1 := time.FixedZone("", 3600)
	for _, tc := range []struct{ name, rec, f, slots string }{
		{"a time of day across an instant's edge", wAt.In(plus1).Format(time.RFC3339) + ":30", later.In(plus1).Format(time.RFC3339) + ":45", "s0i:iso8601:d4-d2-d2Td2:d2:d2F+d2:d2 s1r0"},
		{"a time of day across an instant's edge, the words before it of another length", "a " + wAt.In(plus1).Format(time.RFC3339) + ":30", "abcdefghijklmnopqrstuvwxyzabcdefgh " + later.In(plus1).Format(time.RFC3339) + ":45", "s0r0 s1i:iso8601:d4-d2-d2Td2:d2:d2F+d2:d2 s2r0"},
		{"a token that kept one of its words", "x ab-cd", "x cd", "s1t"},
		{"an instant after a backslash", `x\` + wAt.Format(time.RFC3339), `x\` + later.Format(time.RFC3339), "s1i:iso8601:d4-d2-d2Td2:d2:d2FZ"},
		{"a UUID that reads as no instant", "id 00000000-0000-7000-8000-00000000000a x", "id 00000000-0000-7000-8000-00000000000b x", "s1r4"},
		{"a UUID glued to a word that did not change", "idX" + uuid7Of(wAt, "abc123456789abcdef", 'a', false) + " ref=a1", "idX" + uuid7Of(wAt, "abc123456789abcdef", 'a', false) + " ref=b2", "s2r0"},
		// An instant other than a UUID's is a clock read, which carries no piece: a
		// word that changed as its digits did is learned beside it.
		{"a word that changed as an instant's digits did", "at " + wAt.Format(time.RFC3339) + " n 28", "at " + later.Format(time.RFC3339) + " n 29", "s1i:iso8601:d4-d2-d2Td2:d2:d2FZ s3r0"},
		{"a word that changed as an instant's digits did, before it", "n 28 at " + wAt.Format(time.RFC3339), "n 29 at " + later.Format(time.RFC3339), "s1r0 s3i:iso8601:d4-d2-d2Td2:d2:d2FZ"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := slotsOf(learnedWords(t, tc.rec, tc.f)); got != tc.slots {
				t.Fatalf("slots %s, want %s", got, tc.slots)
			}
		})
	}
}

// An instant that is no accepted read is refused only where a changed word
// stands inside it: a token that begins right after it (a JS date's ')')
// is learned.
func TestAWordBesideAnInstantThatDidNotMoveIsLearned(t *testing.T) {
	static := wAt.Add(-time.Hour).Format("Mon Jan 02 2006 15:04:05 GMT-0700") + " (Coordinated Universal Time)"
	e := learnedWords(t, static+"x-1_a", static+"y-22__b")
	if got := slotsOf(e); got != "s12t" {
		t.Fatalf("slots %s, want the token after the date alone", got)
	}
	if ok, _ := WordsAdmit(static+"x-1_a", wAt.Format("Mon Jan 02 2006 15:04:05 GMT-0700")+" (Coordinated Universal Time)x-1_a", e); ok {
		t.Error("the date that did not move admitted to change")
	}
}

// What is learned is what a later replay can place (WordsAdmit): a value
// whose recorded instants are too many to try every way of placing its
// instant slots is refused when it is learned, saying so, never learned to
// go red on every later run.
func TestAValueWhoseInstantsCannotBePlacedIsRefusedAtLearn(t *testing.T) {
	later := wAt.Add(FullRoundLead)
	at := func(t time.Time) string { return t.Format(time.RFC3339) }
	var static []string
	for i := 0; i < 25; i++ {
		static = append(static, at(wAt.Add(-time.Duration(i+1)*time.Hour)))
	}
	rec := strings.Join(static, " ") + " from " + at(wAt) + " to " + at(wAt.Add(time.Second))
	f := strings.Join(static, " ") + " from " + at(later) + " to " + at(later.Add(time.Second))
	if e, why := NewDriftCache().Words(rec, f, nil); why != whyTooMany {
		t.Fatalf("learned %s (%q); want refused: %s", slotsOf(e), why, whyTooMany)
	}
	// A few static instants beside them are placed, and learned.
	rec = strings.Join(static[:3], " ") + " from " + at(wAt) + " to " + at(wAt.Add(time.Second))
	f = strings.Join(static[:3], " ") + " from " + at(later) + " to " + at(later.Add(time.Second))
	learnedWords(t, rec, f)
}

// The structural characters are the closed class the design declares, each
// one a split between two segments, and no other byte is one.
func TestTheStructuralCharactersAreTheDeclaredOnes(t *testing.T) {
	const declared = " \t\n\r\f\v\"'`{}[]():,;&<>|\\=/?#+"
	if structuralSet != declared {
		t.Fatalf("structuralSet %q, want %q", structuralSet, declared)
	}
	for i := 0; i < 256; i++ {
		c := byte(i)
		want := strings.IndexByte(declared, c) >= 0
		if structural(c) != want {
			t.Errorf("structural(%q) = %v, want %v", c, !want, want)
		}
		if c >= utf8.RuneSelf || isWord(c) {
			continue
		}
		if segs := len(tokenize("a"+string(c)+"b", nil).segs); want && segs != 2 || !want && segs != 1 {
			t.Errorf("%q splits a value between two words into %d segments", c, segs)
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
		{"words that moved within their part", "ab-cd x1", "cd-ef x2", whyMoved, nil},
		{"a token that moved to another part", "t1 p-q_r", "p-q_r t2", whyMoved, nil},
		{"an instant's text that moved to another part", "1761042482 a-b_1", strconv.FormatInt(1761042482+int64(FullRoundLead/time.Second), 10) + " 1761042482", whyMoved, nil},
		{"after a backslash escape", `q=a\ref1`, `q=a\ref2`, whyEscape, nil},
		{"a UUID glued to a word at its end", uuid7Of(wAt, "abc123456789abcdef", 'a', false) + "x", uuid7Of(later, "def0123456789abcde", 'b', false) + "x", whyGluedUUID, nil},
		// A UUID's random part is no clock's: a word that carries a group of it
		// is glued inside it.
		{"a word glued inside a UUID's random part", "id " + uuid7Of(wAt, "abc123456789abcdef", 'a', false) + " ref a123", "id " + uuid7Of(later, "def0123456789abcde", 'b', false) + " ref b012", whyGlued, nil},
		{"only the case", "ref=ab12", "ref=AB12", whyCase, nil},
		{"beyond ASCII in a run", "id café1", "id café2", whyNotASCII, nil},
		{"beyond ASCII in a run, recorded only", "id café1", "id cafe2", whyNotASCII, nil},
		{"beyond ASCII in a run, in F only", "id cafe1", "id café2", whyNotASCII, nil},
		{"a run that changed its script", "id 订单-A7", "id XY-A7", whyScript, nil},
		{"beyond ASCII where the runs drifted", "id 订单A7", "id 订单-B8x", whyScript, nil},
		{"after a percent escape", "q=a%20ref1", "q=a%20ref2", whyEscape, nil},
		{"a token holding a percent", "t a%b-c_1", "t a%b_c-d_2", whyEscape, nil},
		{"a token holding a percent recorded only", "t a%b-c_1", "t ab_c-d_2", whyEscape, nil},
		{"a token holding a percent in F only", "t ab-c_1", "t a%b_c-d_2", whyEscape, nil},
		{"beyond ASCII where the runs drifted, recorded only", "id 订单A7", "id x-B8x", whyScript, nil},
		{"beyond ASCII where the runs drifted, in F only", "id A7-x", "id 订单B8", whyScript, nil},
		{"an instant whose zone changed", "at " + wAt.Format(time.RFC3339), "at " + later.In(plus1).Format(time.RFC3339), whyZone, nil},
		{"an instant that did not move as F's clock does", "at " + wAt.Format(time.RFC3339), "at " + wAt.Add(3*time.Hour).Format(time.RFC3339), whyInstant, nil},
		{"an epoch that moved otherwise", "ts " + strconv.FormatInt(wAt.Unix(), 10), "ts " + strconv.FormatInt(wAt.Unix()+3600, 10), whyInstant, nil},
		{"an instant written otherwise", strconv.FormatInt(wAt.Unix(), 10), strconv.FormatInt(later.UnixMilli(), 10), whySkeleton, nil},
		{"a changed word that repeats", "id=x1 ref=x1", "id=y2 ref=y2", whyRepeat, nil},
		{"a changed word that repeats recorded only", "id=x1 ref=x1", "id=y2 ref=z3", whyRepeat, nil},
		{"a time-ordered UUID that repeats", "a " + uuid7Of(wAt, "abc123456789abcdef", 'a', false) + " b " + uuid7Of(wAt, "abc123456789abcdef", 'a', false), "a " + uuid7Of(later, "def0123456789abcde", 'b', false) + " b " + uuid7Of(later, "def0123456789abcde", 'b', false), whyRepeat, nil},
		{"a separator before its first part changed", "a b", " a c", whySeparators, nil},
		{"a separator after its last part changed", "a b", "a c ", whySeparators, nil},
		{"a changed word that repeats in F only", "id=x1 ref=y2", "id=z3 ref=z3", whyRepeat, nil},
		// As many parts, one of them an instant on one side and plain text on
		// the other: its clock read stands elsewhere.
		{"a clock read that stands in another part", "ab " + wAt.Format(time.RFC3339) + " cd", "ab cd " + later.Format(time.RFC3339), whyParts, []string{"ab", "cd"}},
		{"a time-ordered UUID mixing cases", "id " + mixedCase(uuid7Of(wAt, "abc123456789abcdef", 'a', false)), "id " + mixedCase(uuid7Of(later, "def0123456789abcde", 'b', false)), whyUUIDCase, nil},
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
		{"ab-cd", "ab-ef", "cd", "ef"},
		{"ab-cd", "cd", "ab-cd", "cd"},
		{"cd", "ab-cd", "cd", "ab-cd"},
		{"x.1", "x_1", ".", "_"},
		{"AZaz09", "AZbz09", "AZaz", "AZbz"},
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

// A word that did not change on one side is empty there has no piece.
func TestAnEmptySideHasNoPiece(t *testing.T) {
	for _, tc := range [][2]string{{"a", ""}, {"", "a"}} {
		if _, ok := changedPiece(tc[0], tc[1]); ok {
			t.Errorf("%q -> %q has a piece", tc[0], tc[1])
		}
	}
}

// A byte's class within a changed piece: a digit, a letter (ASCII, either
// case), or any other byte, the bytes beside each range included.
func TestEachBytesClassInAPiece(t *testing.T) {
	for i := 0; i < 256; i++ {
		c := byte(i)
		want := 0
		switch {
		case c >= '0' && c <= '9':
			want = 2
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z':
			want = 1
		}
		if got := byteClass(c); got != want {
			t.Errorf("byteClass(%q) = %d, want %d", c, got, want)
		}
	}
	for _, s := range []string{"/09:", "@AZ[", "`az{"} {
		if got := subRuns(s); len(got) != 3 || got[0] != s[:1] || got[2] != s[3:] {
			t.Errorf("subRuns(%q) = %q: the bytes beside a range are of another class", s, got)
		}
	}
}

// pieceIn reads no empty piece, and none longer than its container.
func TestAnEmptyOrLongerPieceIsInNothing(t *testing.T) {
	c := run{"ab12", "cd34"}
	for _, p := range []run{{"", "34"}, {"12", ""}, {"", ""}, {"ab123", "cd34"}, {"ab12", "cd345"}} {
		if pieceIn(c, p) {
			t.Errorf("%q lies inside %q", p, c)
		}
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
		// At one offset from the start, the recorded container ending there; at
		// one from the end, the recorded one beginning there.
		{"start-aligned at the recorded end", [2]string{"P1042", "Q5678zz"}, [2]string{"1042", "5678"}, true},
		{"end-aligned at the recorded start", [2]string{"1042P", "zz5678Q"}, [2]string{"1042", "5678"}, true},
		{"no glue, the container longer live", [2]string{"1042P", "zz5678Q"}, [2]string{"1043", "5679"}, false},
		{"no glue, the container longer recorded", [2]string{"zz5678Q", "1042P"}, [2]string{"5679", "1043"}, false},
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
	b64u := func(s string) string { return base64.RawURLEncoding.EncodeToString([]byte(s)) }
	for _, tc := range []struct {
		name  string
		a, b  [2]string
		want  Tie
		class string
	}{
		{"ORD123 and 123", [2]string{"ORD123", "ORD456"}, [2]string{"123", "456"}, TiePartial, models.RefusedContainment},
		{"INV1042 and 1042", [2]string{"INV1042", "INV2077"}, [2]string{"1042", "2077"}, TiePartial, models.RefusedContainment},
		{"INV1042 and ORD1042", [2]string{"INV1042", "INV2077"}, [2]string{"ORD1042", "ORD2077"}, TiePartial, models.RefusedContainment},
		{"1042A and ORD1042", [2]string{"1042A", "2077A"}, [2]string{"ORD1042", "ORD2077"}, TiePartial, models.RefusedContainment},
		{"p12x1042 beside 1042", [2]string{"p12x1042", "p9x5678"}, [2]string{"1042", "5678"}, TiePartial, models.RefusedContainment},
		{"a template key INV1042 and a slot ORD1042", [2]string{"INV1042", "INV2077"}, [2]string{"note ORD1042", "note ORD2077"}, TiePartial, models.RefusedContainment},
		// A segment whose runs drifted (one token slot) has the segment's
		// changed piece: "INV1042x_y" -> "INV2077x__y" is "1042x_" ->
		// "2077x__", which holds "1042" -> "2077" at a class boundary.
		{"a token slot's piece INV1042x_y and ORD1042", [2]string{"ref INV1042x_y", "ref INV2077x__y"}, [2]string{"ORD1042", "ORD2077"}, TiePartial, models.RefusedContainment},
		// A value whose parts do not line up (another number of them) has
		// the value's changed piece whole: "/orders/1042" -> "/orders/2077/v2"
		// holds "1042" -> "2077" from its start.
		{"a value whose parts do not line up, /orders/1042 and ORD1042", [2]string{"/orders/1042", "/orders/2077/v2"}, [2]string{"ORD1042", "ORD2077"}, TiePartial, models.RefusedContainment},
		// Beside an accepted clock read, a changed word is no instant: the
		// two are not exempt, and the glued piece ties them.
		{"INV1042 and ORD1042, each beside an instant", [2]string{"at " + wAt.Format(time.RFC3339) + " INV1042", "at " + later.Format(time.RFC3339) + " INV2077"},
			[2]string{"at " + wAt.Format(time.RFC3339) + " ORD1042", "at " + later.Format(time.RFC3339) + " ORD2077"}, TiePartial, models.RefusedContainment},
		// A glue only the decoded form shows: "INV10%3442" is "INV10442",
		// whose changed piece is "10442"; as it stands its changed piece is
		// "10", inside no class boundary of "10442".
		{"an over-encoded INV10442 and 10442", [2]string{"INV10%3442", "INV20%3442"}, [2]string{"10442", "20442"}, TiePartial, models.RefusedContainment},
		{"ORD 12347 beside 1234 (N17)", [2]string{"ORD 12347", "ORD 56785"}, [2]string{"1234", "5678"}, TieNone, ""},
		{"seq=17 beside a fresh hex", [2]string{"seq=17", "seq=18"}, [2]string{"3f2b8c1e9a4d70a1", "a7c4e2d10b3f4c2e"}, TieNone, ""},
		{"createdAt and updatedAt", [2]string{wAt.Format(time.RFC3339), later.Format(time.RFC3339)}, [2]string{wAt.Format(time.RFC3339), later.Format(time.RFC3339)}, TieExempt, ""},
		{"an instant beside text and the same instant", [2]string{"at " + wAt.Format(time.RFC3339), "at " + later.Format(time.RFC3339)}, [2]string{wAt.Format(time.RFC3339), later.Format(time.RFC3339)}, TieExempt, ""},
		{"a UUID v7 in a path and a body", [2]string{v7, v7f}, [2]string{v7, v7f}, TieWhole, models.RefusedTie},
		{"one id at two places", [2]string{"o-1111", "o-9999"}, [2]string{"o-1111", "o-9999"}, TieWhole, models.RefusedTie},
		{"two fresh ids", [2]string{"r-3f2b8c", "r-a7c4e2"}, [2]string{"n-91d0e3", "n-5be7aa"}, TieNone, ""},
		// Instants in F written with a fraction they lacked recorded are still
		// instants alone.
		{"createdAt and updatedAt, F's with a fraction", [2]string{"at " + wAt.Format(time.RFC3339) + " x", "at " + later.Format("2006-01-02T15:04:05.000Z") + " x"}, [2]string{wAt.Format(time.RFC3339), later.Format("2006-01-02T15:04:05.000Z")}, TieExempt, ""},
		// A value beside an instant that gained a word, or changed its last
		// separator, is no instant alone.
		{"an instant beside a word F added", [2]string{"at " + wAt.Format(time.RFC3339), "at " + later.Format(time.RFC3339) + " now"}, [2]string{wAt.Format(time.RFC3339), later.Format(time.RFC3339)}, TieNone, ""},
		{"an instant whose last separator changed", [2]string{wAt.Format(time.RFC3339) + " ", later.Format(time.RFC3339) + ";"}, [2]string{wAt.Format(time.RFC3339), later.Format(time.RFC3339)}, TieNone, ""},
		// A UUID's random part is no clock's: a value that carries a part of it is
		// glued inside it.
		{"a word inside a UUID's random group", [2]string{v7, v7f}, [2]string{"x123", "x012"}, TiePartial, models.RefusedContainment},
		// A whole epoch second standing whole in another's base64 text (a JWT's
		// iat) is a whole value repeated, which no glued piece finds: a tie.
		{"a JWT whose iat is an epoch second at another place", [2]string{"1760436042", "1760436103"},
			[2]string{"Bearer " + b64u(`{"alg":"HS256"}`) + "." + b64u(`{"sub":"svc","iat":1760436042}`) + "." + b64u("\x01\x02\x03\xfe\xff"), "Bearer " + b64u(`{"alg":"HS256"}`) + "." + b64u(`{"sub":"svc","iat":1760436103}`) + "." + b64u("\x07\x08\x09\xfa\xfb")},
			TiePartial, models.RefusedTie},
	} {
		if got := NewDriftCache().TieOf(tc.a, tc.b, nil); got != tc.want {
			t.Errorf("%s: %v, want %v", tc.name, got, tc.want)
		}
		if got := NewDriftCache().TieOf(tc.b, tc.a, nil); got != tc.want {
			t.Errorf("%s, the other order: %v, want %v", tc.name, got, tc.want)
		}
		// The rule a refusal of the pair names (models.UnmatchedCall.Refused):
		// containment only where the glued test alone ties them.
		if got := NewDriftCache().TieClass(tc.a, tc.b, nil); got != tc.class {
			t.Errorf("%s: class %q, want %q", tc.name, got, tc.class)
		}
		if got := NewDriftCache().TieClass(tc.b, tc.a, nil); got != tc.class {
			t.Errorf("%s, the other order: class %q, want %q", tc.name, got, tc.class)
		}
	}
}

// TieOf reads the clock reads in the call's window: two values whose only
// change is an instant read within it are exempt there, and are not by the
// lead alone.
func TestTieOfReadsTheCallsWindow(t *testing.T) {
	boot := wAt.Add(4 * time.Minute)
	w := &Window{Rec: Span{From: wAt.Add(-time.Second), To: wAt.Add(time.Second)}, F: Span{From: boot.Add(-time.Second), To: boot.Add(time.Second)}}
	a := [2]string{wAt.Format(time.RFC3339), boot.Format(time.RFC3339)}
	b := [2]string{"at " + wAt.Format(time.RFC3339), "at " + boot.Format(time.RFC3339)}
	if got := NewDriftCache().TieOf(a, b, w); got != TieExempt {
		t.Errorf("in the window: %v, want exempt", got)
	}
	if got := NewDriftCache().TieOf(b, a, w); got != TieExempt {
		t.Errorf("in the window, the other order: %v, want exempt", got)
	}
	if got := NewDriftCache().TieClass(a, b, w); got != "" {
		t.Errorf("in the window: class %q, want none", got)
	}
	if got := NewDriftCache().TieOf(a, b, nil); got == TieExempt {
		t.Errorf("by the lead alone: exempt, want not")
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
	if got[0].Kind != KindVariable || got[0].From.Whole || !got[0].From.Glued {
		t.Fatalf("learned %+v, want a variable glued inside mock-0's value", got[0])
	}
	// As a word of the earlier value, it is found by the token test: no glue.
	earlier[0].Seen[0].Leaf.Value, earlier[0].Seen[0].Live = "INV-1042", "INV-2077"
	rec.Leaves["body.order"], live.Leaves["body.order"] = Leaf{Path: "body.order", Type: String, Value: "1042"}, Leaf{Path: "body.order", Type: String, Value: "2077"}
	got = Classify(nil, []string{"body.order"}, live, rec, wAt.Add(time.Second), earlier, nil, nil)
	if got[0].Kind != KindVariable || got[0].From.Whole || got[0].From.Glued {
		t.Fatalf("learned %+v, want a variable inside mock-0's value, not glued", got[0])
	}
}

// RefusalClass names the tie rules behind the reasons Words gives: a changed
// word that repeats is a tie, one glued inside another containment, and
// every other reason no tie rule.
func TestRefusalClass(t *testing.T) {
	for why, want := range map[string]string{whyRepeat: models.RefusedTie, whyGlued: models.RefusedContainment, whyParts: "", whyGluedUUID: "", whyMoved: "", "": ""} {
		if got := RefusalClass(why); got != want {
			t.Errorf("RefusalClass(%q) = %q, want %q", why, got, want)
		}
	}
	// WholeDrift's reasons are named by it too: a changed word that repeats
	// in the leaf is a tie, every other reason a leaf is not whole is none.
	for _, tc := range []struct {
		rec, live, want string
	}{
		{"ab ab", "cd cd", models.RefusedTie},
		{"ab cd", "ef ef", models.RefusedTie},
		{"ORD-1 paid 100 ref=a1", "ORD-1 paid 100 ref=b2", ""},
		{"--", "---", ""},
		{"a", "a", ""},
	} {
		if _, why := WholeDrift(tc.rec, tc.live, nil); RefusalClass(why) != tc.want {
			t.Errorf("WholeDrift(%q, %q) refuses for %q, which RefusalClass names %q, want %q", tc.rec, tc.live, why, RefusalClass(why), tc.want)
		}
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
		{"a token slot beside a run slot", rec, with(func(c *models.WordsEntry) {
			c.Slots = append(c.Slots, models.WordSlot{Seg: c.Slots[0].Seg, Kind: models.WordSlotToken})
		}), whyRange},
		{"a run slot twice", rec, with(func(c *models.WordsEntry) { c.Slots = append(c.Slots, c.Slots[0]) }), whyRange},
		{"a negative segment", rec, with(func(c *models.WordsEntry) { c.Slots[0].Seg = -1 }), whyRange},
		{"a segment one past the last", rec, with(func(c *models.WordsEntry) { c.Slots[0].Seg = 7 }), whyRange},
		{"a negative run", rec, with(func(c *models.WordsEntry) { c.Slots[0].Run = -1 }), whyRange},
		{"an instant slot of a negative segment", rec, with(func(c *models.WordsEntry) { c.Slots[1].Seg = -1 }), whyRange},
		{"an instant slot naming a run", rec, with(func(c *models.WordsEntry) { c.Slots[1].Run = 1 }), whyGuard},
		{"a token slot naming a run", rec, with(func(c *models.WordsEntry) { c.Slots[0].Kind, c.Slots[0].Run = models.WordSlotToken, 1 }), whyGuard},
		{"a run slot with a layout", rec, with(func(c *models.WordsEntry) { c.Slots[0].Layout = "iso8601" }), whyGuard},
		{"a run slot with a skeleton", rec, with(func(c *models.WordsEntry) { c.Slots[0].Skeleton = "d4" }), whyGuard},
		{"a token slot with a layout", rec, with(func(c *models.WordsEntry) { c.Slots[0].Kind, c.Slots[0].Layout = models.WordSlotToken, "iso8601" }), whyGuard},
		{"an instant slot with no skeleton", rec, with(func(c *models.WordsEntry) { c.Slots[1].Skeleton = "" }), whyKind},
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

// A value that is red says what of it did not hold: an instant where one
// was recorded with none, and a run alone in its segment left empty between
// its separators.
func TestWhyAValueIsRed(t *testing.T) {
	later := wAt.Add(FullRoundLead)
	other := wAt.Add(5 * time.Hour)
	ep := func(t time.Time) string { return strconv.FormatInt(t.Unix(), 10) }
	for _, tc := range []struct{ rec, f, live, why string }{
		{"t ab-1 at " + ep(wAt), "t ab_c-2 at " + ep(later), "t " + ep(other) + " at q", "it holds an instant where it was recorded with none"},
		{"k=-v1-", "k=-v2-", "k=--", "a word of it learned to change holds more than an ASCII token"},
		{"k=-v1-", "k=-v2-", "k=-x", "a separator beside a word of it learned to change is not the one recorded"},
		{"id ab-12-cd", "id ab-34-cd", "id ab-é9-cd", "a word of it learned to change holds more than ASCII letters and digits"},
		{"id ab-12-cd", "id ab-34-cd", "id ab-99-ce", "a word of it that is not learned to change changed"},
		{"id ab-12-cd", "id ab-34-cd", "id ab-99_cd", "a separator beside a word of it is not the one recorded"},
		{"id ab-12-cd.", "id ab-34-cd.", "id ab-99-cd!", "a separator beside a word of it is not the one recorded"},
	} {
		e := learnedWords(t, tc.rec, tc.f)
		if ok, why := WordsAdmit(tc.rec, tc.live, e); ok || why != tc.why {
			t.Errorf("%q (recorded %q): admitted %v (%q), want %q", tc.live, tc.rec, ok, why, tc.why)
		}
	}
}

// The ways WordsAdmit tries to place a value's instants count only the
// matches a later layout's earlier choices left free: sixteen instants with
// a time of day inside each, and one more time of day, place an instant and
// a time of day in 16 x 16 = 256 ways, the last of them the recorded one's.
func TestTheWaysToPlaceInstantsSkipWhatAnEarlierLayoutTook(t *testing.T) {
	later := wAt.Add(FullRoundLead)
	var static []string
	for i := 0; i < 15; i++ {
		static = append(static, wAt.Add(-time.Duration(i+1)*time.Hour).Format(time.DateTime))
	}
	pre := strings.Join(static, " / ") + " / "
	rec := pre + wAt.Format(time.DateTime) + " / at " + wAt.Format(time.TimeOnly)
	f := pre + later.Format(time.DateTime) + " / at " + later.Format(time.TimeOnly)
	e := learnedWords(t, rec, f)
	if got := slotsOf(e); got != "s60i:iso8601:d4-d2-d2 d2:d2:d2F s62i:timeofday:d2:d2:d2F" {
		t.Fatalf("slots %s", got)
	}
}

// An instant slot names the layout its segment collapses by: one moved to a
// plain segment, even of the first layout's name, and two whose layouts
// swapped, are void.
func TestAnInstantSlotNamesItsSegmentsLayout(t *testing.T) {
	later := wAt.Add(FullRoundLead)
	rec, f := "ORD-1 at "+wAt.Format(time.RFC1123), "ORD-1 at "+later.Format(time.RFC1123)
	e := learnedWords(t, rec, f)
	if got := slotsOf(e); got != "s2i:rfc1123:W, d2 M d4 d2:d2:d2 N" {
		t.Fatalf("slots %s", got)
	}
	moved := e.Clone()
	moved.Slots[0].Seg = 0
	if ok, why := WordsAdmit(rec, f, moved); ok || why != whyRange {
		t.Errorf("an rfc1123 slot at a plain segment: admitted %v (%q), want void %q", ok, why, whyRange)
	}
	rec2, f2 := "from "+wAt.Format(time.RFC3339)+" ts "+strconv.FormatInt(wAt.Unix(), 10), "from "+later.Format(time.RFC3339)+" ts "+strconv.FormatInt(later.Unix(), 10)
	e2 := learnedWords(t, rec2, f2)
	if got := slotsOf(e2); got != "s1i:iso8601:d4-d2-d2Td2:d2:d2FZ s3i:epoch:d10F" {
		t.Fatalf("slots %s", got)
	}
	swapped := e2.Clone()
	swapped.Slots[0].Layout, swapped.Slots[1].Layout = swapped.Slots[1].Layout, swapped.Slots[0].Layout
	swapped.Slots[0].Skeleton, swapped.Slots[1].Skeleton = swapped.Slots[1].Skeleton, swapped.Slots[0].Skeleton
	if ok, why := WordsAdmit(rec2, f2, swapped); ok || why != whyRange {
		t.Errorf("two instant slots whose layouts swapped: admitted %v (%q), want void %q", ok, why, whyRange)
	}
	// Live, an instant of another layout at a slot's segment is not where
	// the slot's was recorded.
	other := wAt.Add(5 * time.Hour)
	live := "from " + strconv.FormatInt(other.Unix(), 10) + " ts " + other.Format(time.RFC3339)
	if ok, why := WordsAdmit(rec2, live, e2); ok || why != "an instant of it is not where it was recorded, in its layout" {
		t.Errorf("two instants of two layouts swapped: admitted %v (%q)", ok, why)
	}
}

// WordsAdmit tries maxCollapses ways at most: a recorded value whose read is
// the 256th way of placing it is learned, and served; one more is refused at
// learn. A value of many more instants than its slots is refused promptly,
// the ways it did not try never walked.
func TestTheWaysToPlaceAnInstantAreBounded(t *testing.T) {
	later := wAt.Add(FullRoundLead)
	at := func(t time.Time) string { return t.Format(time.RFC3339) }
	static := func(n int) string {
		out := make([]string, n)
		for i := range out {
			out[i] = at(wAt.Add(-time.Duration(i+1) * time.Hour))
		}
		return strings.Join(out, " ")
	}
	learnedWords(t, static(maxCollapses-1)+" at "+at(wAt), static(maxCollapses-1)+" at "+at(later))
	if _, why := NewDriftCache().Words(static(maxCollapses)+" at "+at(wAt), static(maxCollapses)+" at "+at(later), nil); why != whyTooMany {
		t.Errorf("one way more than WordsAdmit tries: %q, want %q", why, whyTooMany)
	}
	var rec, f []string
	for i := 0; i < 5; i++ {
		rec = append(rec, at(wAt.Add(time.Duration(i)*time.Second)))
		f = append(f, at(later.Add(time.Duration(i)*time.Second)))
	}
	e := learnedWords(t, strings.Join(rec, " / "), strings.Join(f, " / "))
	many := make([]string, 200)
	for i := range many {
		many[i] = at(later.Add(time.Duration(i) * time.Minute))
	}
	done := make(chan string, 1)
	go func() { _, why := WordsAdmit(strings.Join(rec, " / "), strings.Join(many, " / "), e); done <- why }()
	select {
	case why := <-done:
		if why != whyTooMany {
			t.Errorf("200 instants for 5 slots: %q, want %q", why, whyTooMany)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("200 instants for 5 slots: WordsAdmit walked past the ways it tries")
	}
}

// Every instant a value's entry collapses is one instant slot's: an entry
// whose two instant slots name one instant of a value of two (malformed: the
// learn step never writes it) is void, for F's value and the recording
// alike, and never reaches the instant that has no slot.
func TestEachInstantOfAnEntryIsOneSlots(t *testing.T) {
	later := wAt.Add(FullRoundLead)
	at := func(t time.Time) string { return t.Format(time.RFC3339) }
	rec := "from " + at(wAt) + " to " + at(wAt.Add(time.Second))
	f := "from " + at(later) + " to " + at(later.Add(time.Second))
	e := learnedWords(t, rec, f)
	if got := slotsOf(e); got != "s1i:iso8601:d4-d2-d2Td2:d2:d2FZ s3i:iso8601:d4-d2-d2Td2:d2:d2FZ" {
		t.Fatalf("slots %s", got)
	}
	for _, seg := range []int{1, 3} {
		c := e.Clone()
		c.Slots[0].Seg, c.Slots[1].Seg = seg, seg
		for _, live := range []string{f, rec} {
			if ok, why := WordsAdmit(rec, live, c); ok || why != whyRange {
				t.Errorf("both instant slots at segment %d, %q: admitted %v (%q), want void %q", seg, live, ok, why, whyRange)
			}
		}
	}
	// A token or a run slot at an instant is void too.
	for _, kind := range []string{models.WordSlotToken, models.WordSlotRun} {
		c := e.Clone()
		c.Slots = append(c.Slots, models.WordSlot{Seg: 1, Kind: kind})
		if ok, why := WordsAdmit(rec, f, c); ok || why != whyRange {
			t.Errorf("a %q slot beside an instant's: admitted %v (%q), want void %q", kind, ok, why, whyRange)
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

// Each rune's class, as the runs read it: ASCII letters and digits and the
// Latin script's letters are Latin, every other script its own, Common's
// letters and digits (a fullwidth digit, ー, µ) Common; a mark of any kind,
// or a rune of the Inherited script (a joiner), joins the class before it;
// any other rune (a symbol, a space, a control) is no word.
func TestEachRunesClass(t *testing.T) {
	for _, tc := range []struct {
		r           rune
		class       string
		word, joins bool
	}{
		{'a', "Latin", true, false},
		{'Z', "Latin", true, false},
		{'5', "Latin", true, false},
		{'é', "Latin", true, false},
		{'Ж', "Cyrillic", true, false},
		{'订', "Han", true, false},
		{'ナ', "Katakana", true, false},
		{'１', "Common", true, false},
		{'ー', "Common", true, false},
		{'µ', "Common", true, false},
		{'\u0301', "", false, true}, // a combining acute: Mn, Inherited
		{'\u0e31', "", false, true}, // a Thai vowel mark: Mn, the Thai script
		{'\u093f', "", false, true}, // a Devanagari vowel sign: Mc
		{'\u20dd', "", false, true}, // an enclosing circle: Me
		{'\u200d', "", false, true}, // the zero-width joiner: Cf, Inherited
		{'-', "", false, false},
		{'€', "", false, false},
		{'—', "", false, false},
		{'\u00a0', "", false, false},
		{'\u0080', "", false, false},
		{'\u007f', "", false, false},
	} {
		class, word, joins := runeClass(tc.r)
		if class != tc.class || word != tc.word || joins != tc.joins {
			t.Errorf("runeClass(%U) = %q %v %v, want %q %v %v", tc.r, class, word, joins, tc.class, tc.word, tc.joins)
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
		{"cafe\u0301x", []string{"cafe\u0301x:Latin"}},
		{"a.b@c~d!e*f$g^h%i", []string{"a:Latin", "b:Latin", "c:Latin", "d:Latin", "e:Latin", "f:Latin", "g:Latin", "h:Latin", "i:Latin"}},
		// A mark joins the run before it, of whatever class; one that
		// follows no run starts none.
		{"cafe\u0301x", []string{"cafe\u0301x:Latin"}},
		{"ก\u0e31x", []string{"ก\u0e31:Thai", "x:Latin"}},
		{"a\u200db", []string{"a\u200db:Latin"}},
		{"\u0301ab", []string{"ab:Latin"}},
		{"a-\u0301b", []string{"a:Latin", "b:Latin"}},
		{"a€b—c", []string{"a:Latin", "b:Latin", "c:Latin"}},
		{"ab１２cd", []string{"ab:Latin", "１２:Common", "cd:Latin"}},
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
