package learn

import (
	"reflect"
	"strings"
	"testing"
	"time"
)

// at is t moved by F's lead, in layout.
func lead(t *testing.T, layout, s string) string {
	t.Helper()
	at, err := time.Parse(layout, s)
	if err != nil {
		t.Fatal(err)
	}
	return at.Add(FullRoundLead).Format(layout)
}

// A value that drifted only by F's clock is masked: its clock reads stand
// as any read of their layout, and what did not drift is still compared
// (founder decision 26).
// What holds no host's zone around a moved read is still masked: an id, a
// lower-case word, a key with digits, a sentence.
func TestClockMaskKeepsAMaskWhereNothingIsTheHosts(t *testing.T) {
	for _, tc := range []struct{ rec, live string }{
		{"order 1234 placed at 2026-10-14T10:00:41Z", "order 1234 placed at 2026-10-14T10:01:42Z"},
		{"user u-77 logged in at 2026-10-14T10:00:41+05:30 via sso", "user u-77 logged in at 2026-10-14T10:01:42+05:30 via sso"},
		{"cache:user-7:1760436041123", "cache:user-7:1760436102623"},
		{"run 3 of 5 began 2026-10-14 10:00:41", "run 3 of 5 began 2026-10-14 10:01:42"},
	} {
		if ClockMask(tc.rec, tc.live) == "" {
			t.Errorf("no mask for %q", tc.rec)
		}
	}
}

func TestClockMaskComparesWhatDidNotDrift(t *testing.T) {
	const iso = "2006-01-02T15:04:05Z07:00"
	rec := "order 1234 placed at 2026-10-14T10:00:41Z"
	live := "order 1234 placed at " + lead(t, iso, "2026-10-14T10:00:41Z")
	mask := ClockMask(rec, live)
	if mask == "" {
		t.Fatalf("no mask for %q -> %q", rec, live)
	}
	for _, v := range []string{rec, live,
		"order 1234 placed at 2026-11-02T08:15:00Z",
		// A formatter that drops a fraction's trailing zeros, or adds one,
		// reads the clock all the same.
		"order 1234 placed at 2026-11-02T08:15:00.12Z",
		"order 1234 placed at 2026-11-02T08:15:00.123456789Z",
	} {
		if !Masked([]string{mask}, v, true) {
			t.Errorf("mask %q does not excuse %q", mask, v)
		}
	}
	for _, v := range []string{
		"order 9999 placed at 2026-11-02T08:15:00Z",
		"order 1234 placed at 2026-11-02T08:15:00Z!",
		"Order 1234 placed at 2026-11-02T08:15:00Z",
		"order 1234 placed at yesterday",
		"order 1234 placed at 2026-11-02T08:15:00Z and order 1",
	} {
		if Masked([]string{mask}, v, true) {
			t.Errorf("mask %q excuses %q, whose stable part changed", mask, v)
		}
	}
}

// Every layout of a whole instant is masked in place.
func TestClockMaskLayouts(t *testing.T) {
	const rfc = time.RFC1123Z
	recEpoch := "1760436041123"
	liveEpoch := "1760436102623"
	for _, tc := range []struct {
		name, rec, live string
		also, never     []string
	}{
		// A numeric offset reads the same on any host's zone: another one
		// is still a read of the layout.
		{"RFC 1123 with an offset, in a sentence", "sent Wed, 14 Oct 2026 10:00:41 +0000 by a", "sent " + lead(t, rfc, "Wed, 14 Oct 2026 10:00:41 +0000") + " by a",
			[]string{"sent Thu, 15 Oct 2026 14:30:00 +0530 by a"}, []string{"sent Thu, 15 Oct 2026 09:00:00 +0000 by b"}},
		{"ISO 8601 in another zone", "at 2026-10-14T10:00:41Z ok", "at 2026-10-14T10:01:42Z ok",
			[]string{"at 2026-10-14T15:31:42+05:30 ok", "at 2026-10-14 15:31:42 ok"}, []string{"at 2026-10-14T15:31:42+05:30 OK"}},
		{"epoch milliseconds, whole", recEpoch, liveEpoch,
			[]string{"1760999999999"}, []string{"abc", "1760436041123 "}},
		{"epoch inside a key", "cache:user-7:" + recEpoch, "cache:user-7:" + liveEpoch,
			[]string{"cache:user-7:1760999999999"}, []string{"cache:user-8:1760999999999"}},
		{"compact", "X-Amz-Date=20261014T100041Z;k=v", "X-Amz-Date=20261014T100142Z;k=v",
			[]string{"X-Amz-Date=20270101T000000Z;k=v"}, []string{"X-Amz-Date=20270101T000000Z;k=w"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mask := ClockMask(tc.rec, tc.live)
			if mask == "" {
				t.Fatalf("no mask for %q -> %q", tc.rec, tc.live)
			}
			for _, v := range append([]string{tc.rec, tc.live}, tc.also...) {
				if !Masked([]string{mask}, v, true) {
					t.Errorf("mask %q does not excuse %q", mask, v)
				}
			}
			for _, v := range tc.never {
				if Masked([]string{mask}, v, true) {
					t.Errorf("mask %q excuses %q", mask, v)
				}
			}
		})
	}
}

// uuidV1Lead is a version-1 UUID moved by F's lead.
func uuidV1Lead(t *testing.T, u string) string {
	t.Helper()
	at, _, ok := readUUIDTime(u)
	if !ok {
		t.Fatalf("not a time-ordered UUID: %s", u)
	}
	const gregorianToUnix = 122192928000000000
	ts := uint64(at.Add(FullRoundLead).UnixNano()/100 + gregorianToUnix)
	low, mid, hi := uint32(ts), uint16(ts>>32), uint16(ts>>48)&0x0fff|0x1000
	parts := strings.Split(u, "-")
	return strings.Join([]string{hex32(low), hex16(mid), hex16(hi), parts[3], parts[4]}, "-")
}

func hex32(v uint32) string { return hexN(uint64(v), 8) }
func hex16(v uint16) string { return hexN(uint64(v), 4) }
func hexN(v uint64, n int) string {
	const digits = "0123456789abcdef"
	b := make([]byte, n)
	for i := n - 1; i >= 0; i-- {
		b[i] = digits[v&0xf]
		v >>= 4
	}
	return string(b)
}

// No mask where the value drifted in more than its clock reads, where no
// clock read moved by the lead, where a clock read moved only in an escaped
// form, where a read that moved is only part of an instant (a time of day, a
// UUID), or where the value is longer than a mask holds: the value is then
// learned whole, as before.
func TestClockMaskOnlyForAClockOnlyDrift(t *testing.T) {
	long := strings.Repeat("x", maxMaskedValue)
	for _, tc := range []struct{ name, rec, live string }{
		{"a random token beside the clock", "at 2026-10-14T10:00:41Z tok=abc", "at 2026-10-14T10:01:42Z tok=def"},
		{"a clock read that did not move by the lead", "at 2026-10-14T10:00:41Z", "at 2026-10-14T10:05:00Z"},
		{"no clock read", "tok=abc", "tok=def"},
		{"an unmoved changed read beside a moved one", "a 2026-10-14T10:00:41Z b 2026-01-01", "a 2026-10-14T10:01:42Z b 2026-01-05"},
		{"moved only in its escaped form", "at=2026-10-14T10%3A00%3A41Z", "at=2026-10-14T10%3A01%3A42Z"},
		{"longer than a mask holds", long + " 2026-10-14T10:00:41Z", long + " 2026-10-14T10:01:42Z"},
		// A read that is only part of an instant (round-1 review): what is
		// beside it, kept literal, would refuse a later replay on another
		// date, in another zone, or on another host.
		{"a time of day", "slot 10:00:41 room 4", "slot 10:01:42 room 4"},
		{"Date.toString", "Wed Oct 14 10:00:41 UTC 2026", "Wed Oct 14 10:01:42 UTC 2026"},
		{"a date and a time of day apart", "on 2026-10-14 at 10:00:41", "on 2026-10-14 at 10:01:42"},
		{"RFC 1123 with a zone name", "Wed, 14 Oct 2026 10:00:41 PDT", "Wed, 14 Oct 2026 10:01:42 PDT"},
		{"a time of day beside a moved instant", "2026-10-14T10:00:41Z (10:00:41)", "2026-10-14T10:01:42Z (10:01:42)"},
		{"a UUID v1", "id a8a2f0b0-a7a5-11f1-8000-0242ac120002", "id " + uuidV1Lead(t, "a8a2f0b0-a7a5-11f1-8000-0242ac120002")},
		// A part of the instant beside the read that its layout's pattern
		// does not read, or a zone the read names (round-2 review): written
		// otherwise on a host in another zone.
		{"Go's Time.String in IST", "2026-10-14 10:00:41.123456789 +0530 IST", "2026-10-14 10:01:42.623456789 +0530 IST"},
		{"Go's Time.String in UTC", "2026-10-14 10:00:41.123456789 +0000 UTC", "2026-10-14 10:01:42.623456789 +0000 UTC"},
		{"Java's ZonedDateTime", "2026-10-14T10:00:41.123+05:30[Asia/Kolkata]", "2026-10-14T10:01:42.623+05:30[Asia/Kolkata]"},
		{"a region ID after a space", "2026-10-14 10:00:41 Asia/Kolkata", "2026-10-14 10:01:42 Asia/Kolkata"},
		{"Python's %Z in IST", "2026-10-14 10:00:41 IST", "2026-10-14 10:01:42 IST"},
		{"Python's %Z in UTC", "2026-10-14 10:00:41 UTC", "2026-10-14 10:01:42 UTC"},
		{"RFC 1123 in GMT", "sent Wed, 14 Oct 2026 10:00:41 GMT by a", "sent Wed, 14 Oct 2026 10:01:42 GMT by a"},
		{"RFC 5322 with a zone comment", "Wed, 14 Oct 2026 10:00:41 +0530 (IST)", "Wed, 14 Oct 2026 10:01:42 +0530 (IST)"},
		{"compact with an offset", "20261014T100041+0530", "20261014T100142+0530"},
		{"a day name before", "Wed 2026-10-14T00:30:41+05:30", "Wed 2026-10-14T00:31:42+05:30"},
		{"a day name after", "2026-10-14T10:00:41Z, Wednesday", "2026-10-14T10:01:42Z, Wednesday"},
		{"a month name before", "Oct. 2026-10-14", "Oct. 2026-10-15"},
		{"PM after", "2026-10-14 10:00:41 PM", "2026-10-14 10:01:42 PM"},
		// Host-dependent text anywhere in what the mask would keep literal
		// (round-3 review): a zone named elsewhere, a clock read F's lead
		// did not move (a local date), a day or month name elsewhere.
		{"a zone label elsewhere", "at 2026-10-14 02:00:41 tz=IST", "at 2026-10-14 02:01:42 tz=IST"},
		{"a region ID before", "zone=Asia/Kolkata at=2026-10-14T02:00:41+05:30", "zone=Asia/Kolkata at=2026-10-14T02:01:42+05:30"},
		{"a zone before, apart", "IST 2026-10-14 02:00:41", "IST 2026-10-14 02:01:42"},
		{"UTC with an offset elsewhere", "tz UTC+5:30; at 2026-10-14T02:00:41+05:30", "tz UTC+5:30; at 2026-10-14T02:01:42+05:30"},
		{"an offset elsewhere", "offset +05:30 at 2026-10-14T02:00:41Z", "offset +05:30 at 2026-10-14T02:01:42Z"},
		{"an unmoved local date before", "report 2026-10-14 generated 2026-10-14T02:00:41+05:30", "report 2026-10-14 generated 2026-10-14T02:01:42+05:30"},
		{"an unmoved local date after", "issued 2026-10-14T02:00:41+05:30, valid until 2026-10-14", "issued 2026-10-14T02:01:42+05:30, valid until 2026-10-14"},
		{"an unmoved time of day", "shift 09:00 to 17:00, clocked 2026-10-14T02:00:41Z", "shift 09:00 to 17:00, clocked 2026-10-14T02:01:42Z"},
		{"a day name elsewhere", "Wednesday digest, sent 2026-10-14T02:00:41+05:30", "Wednesday digest, sent 2026-10-14T02:01:42+05:30"},
		{"a month name in capitals elsewhere", "OCT batch: at 2026-10-14T02:00:41Z", "OCT batch: at 2026-10-14T02:01:42Z"},
	} {
		if mask := ClockMask(tc.rec, tc.live); mask != "" {
			t.Errorf("%s: mask %q", tc.name, mask)
		}
	}
}

// Classify masks noise that drifted only by F's clock, and learns any other
// noise whole.
func TestClassifyMasksAClockOnlyDrift(t *testing.T) {
	rec := body(t, "k", `{"note":"order 1234 placed at 2026-10-14T10:00:41Z","nonce":"n-1"}`)
	live := body(t, "k", `{"note":"order 1234 placed at 2026-10-14T10:01:42Z","nonce":"n-2"}`)
	got := Classify(nil, []string{"body.nonce", "body.note"}, live, rec, time.Now(), nil, nil, nil)
	if len(got) != 2 || got[0].Kind != KindNoise || got[1].Kind != KindNoise {
		t.Fatalf("learned %+v", got)
	}
	if got[0].Mask != "" {
		t.Errorf("a fresh nonce was masked: %q", got[0].Mask)
	}
	if got[1].Mask == "" || !Masked([]string{got[1].Mask}, "order 1234 placed at 2027-01-01T00:00:00Z", true) || Masked([]string{got[1].Mask}, "order 1235 placed at 2027-01-01T00:00:00Z", true) {
		t.Errorf("the clock-only note's mask is %q", got[1].Mask)
	}
}

// masked excuses each leaf by its patterns, as learned noise does.
type masked map[string][]string

func (k masked) Excused(l Leaf) (bool, bool) {
	pats, ok := k[l.Path]
	return ok && Masked(pats, l.Value, l.Type != 0 && l.Type != EmptyObj && l.Type != EmptyArray), true
}

// Compare honours a mask: the live value must be one it excuses, so a call
// that changed what did not drift is a drift again, never excused.
func TestCompareHonoursAMask(t *testing.T) {
	rec := body(t, "k", `{"note":"order 1234 placed at 2026-10-14T10:00:41Z"}`)
	mask := ClockMask("order 1234 placed at 2026-10-14T10:00:41Z", "order 1234 placed at 2026-10-14T10:01:42Z")
	k := masked{"body.note": {mask}}
	if d := Compare(body(t, "k", `{"note":"order 1234 placed at 2026-12-01T00:00:00Z"}`), rec, k); d.Structural != "" || len(d.Drift) != 0 {
		t.Fatalf("a clock-only drift was not excused: %+v", d)
	}
	if d := Compare(body(t, "k", `{"note":"order 9999 placed at 2026-12-01T00:00:00Z"}`), rec, k); d.Structural != "" || !reflect.DeepEqual(d.Drift, []string{"body.note"}) {
		t.Fatalf("a drift outside the clock read was excused: %+v", d)
	}
	// Learned whole, any value is excused.
	if d := Compare(body(t, "k", `{"note":"anything"}`), rec, masked{"body.note": {}}); len(d.Drift) != 0 {
		t.Fatalf("noise learned whole did not excuse a value: %+v", d)
	}
	// A mask keeps the type.
	if d := Compare(body(t, "k", `{"note":7}`), rec, k); d.Structural != "body.note changed type" {
		t.Fatalf("a masked leaf changed type: %+v", d)
	}
}

// MergeMasks: a leaf learned whole on either side stays whole; masks union;
// nothing is shared with the inputs.
func TestMergeMasks(t *testing.T) {
	existing := map[string][]string{"a": {"^x$"}, "b": {}, "c": {"^y$"}, "d": {"^z$"}}
	detected := map[string][]string{"a": {}, "b": {"^q$"}, "c": {"^y$", "^w$"}, "e": {"^v$"}}
	got := MergeMasks(existing, detected)
	want := map[string][]string{"a": {}, "b": {}, "c": {"^y$", "^w$"}, "d": {"^z$"}, "e": {"^v$"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("merged %v, want %v", got, want)
	}
	got["c"][0], got["e"][0] = "changed", "changed"
	if existing["c"][0] != "^y$" || detected["e"][0] != "^v$" {
		t.Fatal("the merge shares a slice with its inputs")
	}
	if !Masked(nil, "anything", false) || Masked([]string{"^x$"}, "x", false) || Masked([]string{"("}, "(", true) {
		t.Fatal("Masked: whole excuses all, a mask no container, a broken pattern nothing")
	}
}
