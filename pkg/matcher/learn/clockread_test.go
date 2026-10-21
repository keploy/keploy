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

// A leaf is learned as noise for a clock read only when the leaf is the
// clock read: noise excuses the whole leaf, so what holds beside the read
// would never be compared again. Each probe drifts only where a clock read
// is, or drifts with no read at all; none is noise, nor an echo, and the
// refusal names the part that is more than the read, never its value.
func TestOnlyALeafThatIsTheClockReadIsLearnedForIt(t *testing.T) {
	later := recAt.Add(FullRoundLead)
	b64 := func(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }
	url := func(s string) string { return strings.NewReplacer(" ", "%20", ":", "%3A").Replace(s) }
	for _, tc := range []struct {
		name, rec, live string
		// why is what the refusal says; values are words of the leaf it
		// must not hold.
		why    string
		values []string
	}{
		{"a sign flip", "-5", "5", "no word in it is new: only a sign or separator between its words changed", []string{"5"}},
		{"swapped ids", "A7K B8L", "B8L A7K", "no word in it is new: only the order of its words changed", []string{"A7K", "B8L"}},
		// Read decoded, as an escape's remnant glued to a word ("2CB8L" of
		// "%2CB8L") is no word of the value.
		{"swapped ids, percent-encoded", "A7K%2CB8L", "B8L%2CA7K", "no word in it is new: only the order of its words changed", []string{"A7K", "B8L"}},
		{"swapped ids after a JSON escape", `A7K\nB8L`, `B8L\nA7K`, "no word in it is new: only the order of its words changed", []string{"A7K", "B8L"}},
		{"a sign flip, percent-encoded", "%2D5", "5", "no word in it is new: only a sign or separator between its words changed", []string{"5"}},
		{"text beside a clock read", "ORD-1 paid 100 at " + iso(recAt), "ORD-1 paid 100 at " + iso(later),
			"it holds text before its clock read that did not change", []string{"ORD", "paid", "100"}},
		{"a sign flip beside a clock read", "-5 at " + iso(recAt), "5 at " + iso(later),
			"a sign or separator before its clock read changed too", []string{"5"}},
		{"a sequence beside a clock read", "seq=28 at " + iso(recAt), "seq=29 at " + iso(later),
			"a word before its clock read changed too", []string{"seq", "28", "29"}},
		{"swapped ids beside a clock read", "A7K B8L " + iso(recAt), "B8L A7K " + iso(later),
			"the order of the words before its clock read changed too", []string{"A7K", "B8L"}},
		{"text after a clock read", iso(recAt) + " by u1", iso(later) + " by u1",
			"it holds text after its clock read that did not change", []string{"u1"}},
		{"a separator between two clock reads", iso(recAt) + ", " + iso(recAt), iso(later) + " ," + iso(later),
			"a sign or separator between two of its clock reads changed too", nil},
		// A layout's zone is a zone's name, a region or an offset: free text
		// where one stands is text beside the read.
		{"free text in a JavaScript date's zone name", recAt.Format("Mon Jan 02 2006 15:04:05 GMT-0700") + " (ORD-1 paid 100 to acct 4417)", later.Format("Mon Jan 02 2006 15:04:05 GMT-0700") + " (ORD-1 paid 100 to acct 4417)",
			"it holds text after its clock read that did not change", []string{"ORD", "paid", "4417"}},
		{"text in brackets after an instant", iso(recAt) + "[ORD-1]", iso(later) + "[ORD-1]",
			"it holds text after its clock read that did not change", []string{"ORD"}},
		{"a path in brackets after an instant", iso(recAt) + "[Orders/ORD-4417]", iso(later) + "[Orders/ORD-4417]",
			"it holds text after its clock read that did not change", []string{"Orders", "ORD", "4417"}},
		{"a path of words in brackets after an instant", iso(recAt) + "[Orders/Paid]", iso(later) + "[Orders/Paid]",
			"it holds text after its clock read that did not change", []string{"Orders", "Paid"}},
		{"words ending in Time where a JavaScript zone name stands", recAt.Format("Mon Jan 02 2006 15:04:05 GMT-0700") + " (Acme Customer Alice Smith Account Time)", later.Format("Mon Jan 02 2006 15:04:05 GMT-0700") + " (Acme Customer Alice Smith Account Time)",
			"it holds text after its clock read that did not change", []string{"Acme", "Alice", "Smith"}},
		// An offset is of hours 00 to 14: a signed number after a time is
		// text beside it.
		{"a signed number after a time", recAt.Format("2006-01-02 15:04:05") + " -1530", later.Format("2006-01-02 15:04:05") + " -1530",
			"it holds text after its clock read that did not change", []string{"1530"}},
		{"a word where a Unix date's zone stands", recAt.Format("Mon Jan _2 15:04:05 SENT 2006"), later.Format("Mon Jan _2 15:04:05 SENT 2006"),
			"it holds text before its clock read that did not change", []string{"SENT", "Oct", "2026"}},
		// A time-ordered UUID's other groups are no part of its instant:
		// kept, they stand beside the read as text does.
		{"a UUID v7 whose random groups did not change", uuid7(recAt, "abc123456789abcdef"), uuid7(later, "abc123456789abcdef"),
			"a word of it that changed is no part of the instant of a clock read in it", []string{"abc", "123456789abcdef"}},
		// Every form that drifted is read: text beside a clock read whose
		// escapes the decoded form reads (a URL's query), or inside base64,
		// is text beside it still.
		{"text beside a percent-encoded clock read", url("ORD-1 paid 100 at " + iso(recAt)), url("ORD-1 paid 100 at " + iso(later)),
			"read decoded, it holds text before its clock read that did not change", []string{"ORD", "paid", "100"}},
		{"text beside a clock read, base64-encoded", b64("ORD-1 paid 100 at " + iso(recAt)), b64("ORD-1 paid 100 at " + iso(later)),
			"keploy reads no clock read where it changed (it reads one in the text its encoded runs decode to)", []string{"ORD", "paid"}},
		// Read as it is, the value holds no clock read where its
		// quoted-printable form does ("=20" glued to the year): what one
		// form cannot read as the clock, noise would stop comparing.
		{"a clock read after a quoted-printable escape", "=20" + iso(recAt), "=20" + iso(later),
			"keploy reads no clock read where it changed (it reads one as quoted-printable)", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := classifyNote(t, tc.rec, tc.live)
			if got.Kind != KindUnaccounted {
				t.Fatalf("%q -> %q learned as %s; only a leaf that is the clock read is learned for it", tc.rec, tc.live, got.Kind)
			}
			if !strings.Contains(got.Why, tc.why) || !strings.Contains(got.Why, "noise would stop comparing all of it") {
				t.Errorf("refused as %q, want it to say %q and why noise cannot be learned", got.Why, tc.why)
			}
			for _, v := range tc.values {
				if ContainsToken(got.Why, v) {
					t.Errorf("refused as %q, which holds %q of the value", got.Why, v)
				}
			}
			// What decides is the two values alone: the same values, read
			// in a cache another call filled, decide the same.
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

// A leaf that is nothing but clock reads is still learned: an instant, in
// the layouts keploy reads (each whole: its date, zone and year too), alone
// or two with only their separator between, and one whose escapes the
// decoded form reads ("%3A" of its colons, again "%253A", "\u003A").
func TestALeafThatIsTheClockReadIsStillLearned(t *testing.T) {
	later := recAt.Add(FullRoundLead)
	for _, tc := range []struct{ name, rec, live string }{
		{"an RFC 3339 instant", iso(recAt), iso(later)},
		{"one with milliseconds", recAt.Format("2006-01-02T15:04:05.000Z"), later.Format("2006-01-02T15:04:05.000Z")},
		{"epoch milliseconds", "1792578482000", "1792578543500"},
		{"two instants and their separator", iso(recAt) + " / " + iso(recAt), iso(later) + " / " + iso(later)},
		{"an HTTP date", recAt.Format(time.RFC1123), later.Format(time.RFC1123)},
		{"a percent-encoded instant", strings.ReplaceAll(iso(recAt), ":", "%3A"), strings.ReplaceAll(iso(later), ":", "%3A")},
		{"an instant in a JSON string's escapes", `\"` + iso(recAt) + `\"`, `\"` + iso(later) + `\"`},
		{"a percent-encoded instant, encoded again", strings.ReplaceAll(iso(recAt), ":", "%253A"), strings.ReplaceAll(iso(later), ":", "%253A")},
		{"an instant's colons as JSON \\u escapes", strings.ReplaceAll(iso(recAt), ":", `\u003A`), strings.ReplaceAll(iso(later), ":", `\u003A`)},
		{"Ruby's Time#to_s", recAt.Format("2006-01-02 15:04:05 -0700"), later.Format("2006-01-02 15:04:05 -0700")},
		{"Java's Date#toString", recAt.Format(time.UnixDate), later.Format(time.UnixDate)},
		{"asctime", recAt.Format(time.ANSIC), later.Format(time.ANSIC)},
		{"Go's RubyDate", recAt.Format(time.RubyDate), later.Format(time.RubyDate)},
		{"JavaScript's Date#toString", recAt.Format("Mon Jan 02 2006 15:04:05 GMT-0700") + " (Coordinated Universal Time)", later.Format("Mon Jan 02 2006 15:04:05 GMT-0700") + " (Coordinated Universal Time)"},
		{"Java's ZonedDateTime", recAt.Format("2006-01-02T15:04:05.000Z07:00") + "[UTC]", later.Format("2006-01-02T15:04:05.000Z07:00") + "[UTC]"},
		{"Java's ZonedDateTime in a region", recAt.In(time.FixedZone("", 7200)).Format("2006-01-02T15:04:05+02:00") + "[Europe/Paris]", later.In(time.FixedZone("", 7200)).Format("2006-01-02T15:04:05+02:00") + "[Europe/Paris]"},
		{"Go's Time.String", recAt.Add(123456789).Format("2006-01-02 15:04:05.999999999 -0700 MST"), later.Add(456789).Format("2006-01-02 15:04:05.999999999 -0700 MST")},
		{"an offset after a space", recAt.Format("2006-01-02 15:04:05 -07:00"), later.Format("2006-01-02 15:04:05 -07:00")},
		{"a form-encoded datetime", strings.ReplaceAll(recAt.Format("2006-01-02+15:04:05"), ":", "%3A"), strings.ReplaceAll(later.Format("2006-01-02+15:04:05"), ":", "%3A")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := classifyNote(t, tc.rec, tc.live); got.Kind != KindNoise {
				t.Fatalf("%q -> %q learned as %s (%s); a clock read is noise", tc.rec, tc.live, got.Kind, got.Why)
			}
		})
	}
}

// A value that holds a clock read F's clock moved is learned for it only
// when it is nothing but its clock reads, fresh beside it or not: a token
// the app mints with its clock in it (a JWT's iat beside a fresh jti, a
// UUID v7's random groups) is refused, where the same token with no clock
// read is a fresh value, noise (TestClassifyLearnsFreshValuesThatShareWhatDidNotChange).
// F's clock accounts for the read, and nothing it saw says what of the rest
// may change.
func TestAValueThatHoldsAClockReadAndMoreIsRefused(t *testing.T) {
	later := recAt.Add(FullRoundLead)
	b64u := func(s string) string { return base64.RawURLEncoding.EncodeToString([]byte(s)) }
	jwt := func(jti string, iat int64) string {
		return "Bearer " + b64u(`{"alg":"HS256","typ":"JWT"}`) + "." + b64u(`{"jti":"`+jti+`","iat":`+strconv.FormatInt(iat, 10)+`}`) + "." + b64u("\x01\x02"+jti[:3]+"\xfe\xff")
	}
	for _, tc := range []struct{ name, rec, live string }{
		{"a JWT whose iat F's clock moved, beside a fresh jti", jwt("Hq6Ys0Bc5D", recAt.Unix()), jwt("Mw2Xr8Ue3F", later.Unix())},
		{"a UUID v7 with fresh random groups", uuid7(recAt, "abc123456789abcdef"), uuid7(later, "def0123456789abcde")},
		{"a fresh id beside a clock read", "req-3f2b8c1e9a4d " + iso(recAt), "req-a7c4e2d10b3f " + iso(later)},
		{"a lone instant, base64-encoded", base64.StdEncoding.EncodeToString([]byte(iso(recAt))), base64.StdEncoding.EncodeToString([]byte(iso(later)))},
		// A '+' joins a date and a time only before its seconds ("+05:00"
		// after a date is its offset, an xs:date): a form-encoded time to
		// the minute is not read whole, and its date stands beside the read.
		{"a form-encoded time to the minute", strings.ReplaceAll(recAt.Format("2006-01-02+15:04"), ":", "%3A"), strings.ReplaceAll(later.Format("2006-01-02+15:04"), ":", "%3A")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := classifyNote(t, tc.rec, tc.live); got.Kind != KindUnaccounted {
				t.Fatalf("%q -> %q learned as %s; a value that is more than its clock read is not learned for it", tc.rec, tc.live, got.Kind)
			}
		})
	}
	// The same JWT whose iat did not move: a fresh value, noise.
	if got := classifyNote(t, jwt("Hq6Ys0Bc5D", recAt.Unix()), jwt("Mw2Xr8Ue3F", recAt.Unix())); got.Kind != KindNoise {
		t.Fatalf("a fresh JWT issued at one instant learned as %s (%s)", got.Kind, got.Why)
	}
}

// What the value is beside its clock reads (besideReads) is said of the
// part, read in the reads' places, recorded and live.
func TestBesideReads(t *testing.T) {
	later := recAt.Add(FullRoundLead)
	for _, tc := range []struct{ name, rec, live, want string }{
		{"nothing but the read", iso(recAt), iso(later), ""},
		{"two reads and a separator", iso(recAt) + "/" + iso(recAt), iso(later) + "/" + iso(later), ""},
		{"text before the first of two", "a " + iso(recAt) + "/" + iso(recAt), "a " + iso(later) + "/" + iso(later), "it holds text before its first clock read that did not change"},
		{"a word after the last of two", iso(recAt) + "/" + iso(recAt) + " v1", iso(later) + "/" + iso(later) + " v2", "a word after its last clock read changed too"},
		{"reads of two layouts that swapped places", iso(recAt) + " " + strconv.FormatInt(recAt.Unix(), 10), strconv.FormatInt(later.Unix(), 10) + " " + iso(later), "its clock reads stand in another order than when it was recorded"},
	} {
		c := clockOf(tc.rec, tc.live)
		if !c.read {
			t.Fatalf("%s: no clock read moved in %q -> %q", tc.name, tc.rec, tc.live)
		}
		if c.beyond != tc.want || c.alone != (tc.want == "") {
			t.Errorf("%s: alone %v, beyond %q; want %q", tc.name, c.alone, c.beyond, tc.want)
		}
	}
}

// What the ISO layout reads of a value: an offset after a space only after
// a time, a '+' between the date and a time to the second only, and a
// region only of an IANA area.
func TestTheISOLayoutReadsAnInstantsOwnParts(t *testing.T) {
	var iso8601 clockLayout
	for _, lay := range clockLayouts {
		if lay.re.MatchString("2026-10-21T10:28:02Z") && lay.re.FindString("2026-10-21T10:28:02Z") == "2026-10-21T10:28:02Z" {
			iso8601 = lay
			break
		}
	}
	for _, tc := range []struct{ in, want string }{
		{"2026-10-21 10:28:02 +0000 UTC", "2026-10-21 10:28:02 +0000 UTC"},
		{"2026-10-21 10:28 -2026-10-21", "2026-10-21 10:28"},
		{"2026-10-21 +1234", "2026-10-21"},
		{"2026-10-21 10:28:02 -1530", "2026-10-21 10:28:02"},
		{"2026-10-21+10:28:02", "2026-10-21+10:28:02"},
		{"2026-10-21+05:00", "2026-10-21+05:00"},
		{"2026-10-21T10:28:02+02:00[Europe/Paris]", "2026-10-21T10:28:02+02:00[Europe/Paris]"},
		{"2026-10-21T10:28:02Z[Orders/ORD-1]", "2026-10-21T10:28:02Z"},
		{"2026-10-21T10:28:02Z[Etc/ORD-4417]", "2026-10-21T10:28:02Z"},
		{"2026-10-21T10:28:02Z[Orders/Paid]", "2026-10-21T10:28:02Z"},
		{"2026-10-21T10:28:02Z[America/ORD-4417]", "2026-10-21T10:28:02Z"},
		{"2026-10-21T10:28:02-03:00[America/Argentina/Buenos_Aires]", "2026-10-21T10:28:02-03:00[America/Argentina/Buenos_Aires]"},
		{"2026-10-21T10:28:02+05:00[Etc/GMT-5]", "2026-10-21T10:28:02+05:00[Etc/GMT-5]"},
		{"2026-10-21T10:28:02Z[Etc/UTC]", "2026-10-21T10:28:02Z[Etc/UTC]"},
		{"2026-10-21T10:28:02Z[Etc/GMT0]", "2026-10-21T10:28:02Z[Etc/GMT0]"},
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
	if ClockReadWhole("2026-10-21+05:00", "2026-10-21+05:01", nil, true) {
		t.Errorf("an xs:date whose offset changed is a clock read")
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
		{`10\u00413A41Z`, "u00413A41Z", false},
	} {
		set := map[string]bool{"41Z": true}
		escapeClock(tc.s, set)
		if set[tc.word] != tc.clock {
			t.Errorf("escapeClock(%q): %q a clock word %v, want %v", tc.s, tc.word, set[tc.word], tc.clock)
		}
	}
}

// ClockReadWhole is the rule ClassifyIn learns noise for a clock read by:
// true of every value it still learns for its clock read, false of every
// probe it refuses.
func TestClockReadWholeIsClassifysRule(t *testing.T) {
	later := recAt.Add(FullRoundLead)
	for _, tc := range [][2]string{
		{iso(recAt), iso(later)},
		{"1792578482000", "1792578543500"},
		{iso(recAt) + " / " + iso(recAt), iso(later) + " / " + iso(later)},
		{recAt.Format(time.RFC1123), later.Format(time.RFC1123)},
		{strings.ReplaceAll(iso(recAt), ":", "%3A"), strings.ReplaceAll(iso(later), ":", "%3A")},
	} {
		if !ClockReadWhole(tc[0], tc[1], nil, true) {
			t.Errorf("%q -> %q, which Classify learns for its clock read, is not whole", tc[0], tc[1])
		}
		if got := classifyNote(t, tc[0], tc[1]); got.Kind != KindNoise {
			t.Errorf("%q -> %q learned as %s (%s)", tc[0], tc[1], got.Kind, got.Why)
		}
	}
	for _, tc := range [][2]string{
		{"-5", "5"},
		{"ORD-1 paid 100 at " + iso(recAt), "ORD-1 paid 100 at " + iso(later)},
		{"seq=28 at " + iso(recAt), "seq=29 at " + iso(later)},
		{"A7K B8L " + iso(recAt), "B8L A7K " + iso(later)},
		{uuid7(recAt, "abc123456789abcdef"), uuid7(later, "abc123456789abcdef")},
		{iso(recAt), iso(recAt)},
		// What the declared readings cannot read (a '%' that is no escape)
		// is refused whatever else it holds.
		{iso(recAt) + "%", iso(later) + "%"},
	} {
		if ClockReadWhole(tc[0], tc[1], nil, true) {
			t.Errorf("%q -> %q, which Classify refuses, is whole", tc[0], tc[1])
		}
		if tc[0] != tc[1] {
			if got := classifyNote(t, tc[0], tc[1]); got.Kind != KindUnaccounted {
				t.Errorf("%q -> %q learned as %s", tc[0], tc[1], got.Kind)
			}
		}
	}
}

// A read the lead alone accepts (it moved by FullRoundLead, outside the
// call's window) is no clock read when the lead is not taken (lead false:
// a pass over mocks no test owns, whose calls ran on no clock F set): only
// a read within the call's window is.
func TestLeadOnlyReadIsNoClockReadWithoutLead(t *testing.T) {
	later := recAt.Add(FullRoundLead)
	rec, live := iso(recAt), iso(later)
	if !ClockReadWhole(rec, live, nil, true) {
		t.Fatalf("%q -> %q: the lead reads no clock read", rec, live)
	}
	if ClockReadWhole(rec, live, nil, false) {
		t.Fatalf("%q -> %q: a read only the lead accepts is a clock read without the lead", rec, live)
	}
	// A window that holds neither instant: still none.
	away := &Window{Rec: Span{From: recAt.Add(-time.Hour), To: recAt.Add(-time.Minute)}, F: Span{From: later.Add(time.Minute), To: later.Add(time.Hour)}}
	if ClockReadWhole(rec, live, away, false) {
		t.Fatalf("%q -> %q: a read outside the call's window is a clock read without the lead", rec, live)
	}
	// Within the call's window it is one, by any amount: here four minutes,
	// which the lead does not read.
	boot := recAt.Add(4 * time.Minute)
	in := &Window{Rec: Span{From: recAt.Add(-time.Second), To: recAt.Add(time.Second)}, F: Span{From: boot.Add(-time.Second), To: boot.Add(time.Second)}}
	if !ClockReadWhole(rec, iso(boot), in, false) {
		t.Fatalf("%q -> %q: a read within the call's window is no clock read", rec, iso(boot))
	}
	if ClockReadWhole(rec, iso(boot), nil, true) {
		t.Fatalf("%q -> %q: the lead reads a read it did not move", rec, iso(boot))
	}
	// So in every form: a percent-encoded read, read decoded.
	pct := func(s string) string { return strings.ReplaceAll(s, ":", "%3A") }
	if ClockReadWhole(pct(rec), pct(live), nil, false) {
		t.Fatalf("%q -> %q: a percent-encoded read only the lead accepts is a clock read without the lead", pct(rec), pct(live))
	}
	if !ClockReadWhole(pct(rec), pct(iso(boot)), in, false) {
		t.Fatalf("%q -> %q: a percent-encoded read within the call's window is no clock read", pct(rec), pct(iso(boot)))
	}
}

// An instant that moved as F's clock moves it, but is written in another
// zone than when it was recorded, is no clock read: F's clock moves no zone.
// It is not learned for its clock read, alone or beside text, and the
// refusal says so.
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
			for _, lead := range []bool{true, false} {
				w := &Window{Rec: Span{From: recAt.Add(-time.Second), To: recAt.Add(time.Second)}, F: Span{From: later.Add(-time.Second), To: later.Add(time.Second)}}
				if ClockReadWhole(tc.rec, tc.live, w, lead) {
					t.Fatalf("%q -> %q (lead %v): a zone change is a clock read", tc.rec, tc.live, lead)
				}
			}
			for _, v := range [][2]string{{tc.rec, tc.live}, {"ORD-1 paid 100 at " + tc.rec, "ORD-1 paid 100 at " + tc.live}} {
				got := classifyNote(t, v[0], v[1])
				if got.Kind != KindUnaccounted || !strings.Contains(got.Why, "written in another zone") {
					t.Fatalf("%q -> %q learned as %s (%q); a zone change is no clock read", v[0], v[1], got.Kind, got.Why)
				}
			}
		})
	}
	// A zone that changed where only the decoded form reads it.
	pct := func(s string) string { return strings.NewReplacer(":", "%3A", "+", "%2B").Replace(s) }
	rec, live := pct(iso(recAt)), pct(later.In(plus1).Format(time.RFC3339))
	if got := classifyNote(t, rec, live); got.Kind != KindUnaccounted || !strings.Contains(got.Why, "written in another zone") {
		t.Fatalf("%q -> %q learned as %s (%q); a zone change is no clock read", rec, live, got.Kind, got.Why)
	}
	if ClockReadWhole(rec, live, nil, true) {
		t.Fatalf("%q -> %q: a zone change read decoded is a clock read", rec, live)
	}
	// The same zone, written alike: a clock read.
	if rec, live := recAt.In(plus1).Format(time.RFC3339), later.In(plus1).Format(time.RFC3339); !ClockReadWhole(rec, live, nil, true) {
		t.Fatalf("%q -> %q, one zone: no clock read", rec, live)
	}
}
