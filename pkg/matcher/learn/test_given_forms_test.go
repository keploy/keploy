package learn

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"strconv"
	"strings"
	"testing"
	"time"
)

// givenRegimes are the three ways the test's clock runs in record's full
// round, as a function of when the test made its value (made): on real time
// (a day later), moved with the application's (by the lead and a few
// milliseconds), and moved by exactly the lead.
func givenRegimes(made time.Time) map[string]time.Time {
	return map[string]time.Time{
		"test clock on real time":              made.Add(26*time.Hour + 3*time.Second),
		"test clock moved with the app's":      made.Add(FullRoundLead + 5*time.Millisecond),
		"test clock moved exactly by the lead": made.Add(FullRoundLead),
	}
}

// learnsAsWithNoGiven fails t where the leaf p of shape, recorded at recAt
// and seen by record's full round at recAt plus the lead, learns otherwise
// beside gave than it does alone.
func learnsAsWithNoGiven(t *testing.T, label, p string, shape func(time.Time) Shape, recAt time.Time, gave Earlier) {
	t.Helper()
	at := recAt.Add(300 * time.Millisecond)
	alone := ClassifyIn(NewDriftCache(), []string{p}, shape(recAt.Add(FullRoundLead)), shape(recAt), at, nil, nil, nil, nil)[0]
	got := ClassifyIn(NewDriftCache(), []string{p}, shape(recAt.Add(FullRoundLead)), shape(recAt), at, []Earlier{gave}, nil, nil, nil)[0]
	if got.Kind != alone.Kind || got.Why != alone.Why || got.From != alone.From {
		t.Errorf("%s: beside %s %q -> %q it is %s from %+v (%s); want it as with no given: %s from %+v", label, gave.Seen[0].Leaf.Path, gave.Seen[0].Leaf.Value, gave.Seen[0].Live, got.Kind, got.From, got.Why, alone.Kind, alone.From)
	}
}

// The application's own clock read, in any form, learns beside a UnixNano
// nonce the test gave exactly as it does with no given: made 40 ms before
// createdAt or in its very second, in each test-clock regime.
func TestAGivenNonceLeavesEveryFormOfTheAppsClockAsItIs(t *testing.T) {
	recAt := time.Date(2026, 10, 22, 11, 14, 17, 0, time.UTC)
	const p = "body.createdAt"
	str := func(v string) Leaf { return Leaf{Path: p, Noise: p, Type: String, Value: v} }
	num := func(v string) Leaf { return Leaf{Path: p, Noise: p, Type: Number, Value: v} }
	forms := map[string]func(time.Time) Leaf{
		"unix s":         func(c time.Time) Leaf { return str(strconv.FormatInt(c.Unix(), 10)) },
		"unix ms":        func(c time.Time) Leaf { return str(strconv.FormatInt(c.UnixMilli(), 10)) },
		"unix ns":        func(c time.Time) Leaf { return str(strconv.FormatInt(c.Add(123*time.Microsecond).UnixNano(), 10)) },
		"unix s number":  func(c time.Time) Leaf { return num(strconv.FormatInt(c.Unix(), 10)) },
		"unix ms number": func(c time.Time) Leaf { return num(strconv.FormatInt(c.UnixMilli(), 10)) },
		"RFC 3339":       func(c time.Time) Leaf { return str(c.Format(time.RFC3339)) },
		"RFC 3339 ms": func(c time.Time) Leaf {
			return str(c.Add(7 * time.Millisecond).Format("2006-01-02T15:04:05.000Z07:00"))
		},
		"RFC 3339 ns":      func(c time.Time) Leaf { return str(c.Add(7123456 * time.Nanosecond).Format(time.RFC3339Nano)) },
		"typed at the s":   func(c time.Time) Leaf { return InstantLeaf(p, p, c, time.Second) },
		"typed at the ms":  func(c time.Time) Leaf { return InstantLeaf(p, p, c.Add(7*time.Millisecond), time.Millisecond) },
		"a date":           func(c time.Time) Leaf { return str(c.Format("2006-01-02")) },
		"an HTTP date":     func(c time.Time) Leaf { return str(c.Format(time.RFC1123)) },
		"prefixed RFC3339": func(c time.Time) Leaf { return str("rep-" + c.Format(time.RFC3339)) },
	}
	for fn, f := range forms {
		shape := func(c time.Time) Shape { return Shape{Leaves: map[string]Leaf{p: f(c)}} }
		for _, off := range []time.Duration{-40 * time.Millisecond, 10 * time.Millisecond, 200 * time.Millisecond} {
			made := recAt.Add(off)
			for rn, full := range givenRegimes(made) {
				g := TestGiven("test o::TestApps", "given:nonce", strconv.FormatInt(made.UnixNano(), 10), strconv.FormatInt(full.UnixNano(), 10))
				learnsAsWithNoGiven(t, fn+", nonce made at "+off.String()+", "+rn, p, shape, recAt, g)
			}
		}
	}
}

// A short given is no part of the application's clock read whose text holds
// it as a fraction of one instant ("14" of "11:14:17", its first digits or
// its last): createdAt learns as it does with no given, as a string, typed,
// and as Unix seconds or millis in a string.
func TestAShortGivenIsNoPartOfTheAppsClockRead(t *testing.T) {
	recAt := time.Date(2026, 10, 22, 11, 14, 17, 0, time.UTC)
	const p = "body.createdAt"
	str := func(v string) Shape {
		return Shape{Leaves: map[string]Leaf{p: {Path: p, Noise: p, Type: String, Value: v}}}
	}
	short := [][2]string{{"14", "15"}, {"17", "18"}, {"11", "12"}, {"2026", "2027"}, {"22", "23"}, {"10", "11"}, {"00", "01"}, {"10-22", "10-23"}}
	for _, c := range []struct {
		form  string
		shape func(time.Time) Shape
		gives [][2]string
	}{
		{"string", func(c time.Time) Shape { return str(c.Format(time.RFC3339)) }, append(short,
			// The instant's start, and its end: a fraction still.
			[2]string{"2026-10-22T11:14", "2026-10-22T11:15"}, [2]string{"14:17Z", "15:18Z"})},
		{"typed", func(c time.Time) Shape { return Shape{Leaves: map[string]Leaf{p: InstantLeaf(p, p, c, time.Second)}} }, short},
		// 1792667657 recorded, 1792667718 in record's full round: its first
		// 9 digits and its last 5, each moved.
		{"unix s string", func(c time.Time) Shape { return str(strconv.FormatInt(c.Unix(), 10)) },
			[][2]string{{"179266765", "179266771"}, {"67657", "67718"}, {"7657", "7718"}}},
		// 1792667657000 recorded, 1792667718500 then.
		{"unix ms string", func(c time.Time) Shape { return str(strconv.FormatInt(c.UnixMilli(), 10)) },
			[][2]string{{"179266765700", "179266771850"}, {"657000", "718500"}}},
	} {
		for _, g := range c.gives {
			learnsAsWithNoGiven(t, c.form+" createdAt, given "+g[0]+" -> "+g[1], p, c.shape, recAt, TestGiven("test o::TestApps", "given:qty", g[0], g[1]))
		}
	}
}

// A nonce of the typed instant's own grain (Unix seconds beside a typed
// second, Unix millis beside a typed milli) names its exact moment in the
// same second or milli: no moment rule reads a given, so the instant learns
// as it does with no given in each test-clock regime. So does createdAt as
// an RFC 3339 text, which holds no text of the nonce.
func TestACoarseNonceOfTheTypedInstantsMomentLeavesItAsItIs(t *testing.T) {
	recAt := time.Date(2026, 10, 22, 11, 14, 17, 30e6, time.UTC)
	const p = "body.createdAt"
	for _, c := range []struct {
		nonce, form string
		of          func(time.Time) string
		leaf        func(time.Time) Leaf
	}{
		{"unix s", "typed at the s", func(c time.Time) string { return strconv.FormatInt(c.Unix(), 10) }, func(c time.Time) Leaf { return InstantLeaf(p, p, c, time.Second) }},
		{"unix ms", "typed at the ms", func(c time.Time) string { return strconv.FormatInt(c.UnixMilli(), 10) }, func(c time.Time) Leaf { return InstantLeaf(p, p, c, time.Millisecond) }},
		{"unix s", "typed at the ms", func(c time.Time) string { return strconv.FormatInt(c.Unix(), 10) }, func(c time.Time) Leaf { return InstantLeaf(p, p, c, time.Millisecond) }},
		{"unix s", "RFC 3339", func(c time.Time) string { return strconv.FormatInt(c.Unix(), 10) }, func(c time.Time) Leaf {
			return Leaf{Path: p, Noise: p, Type: String, Value: c.Format(time.RFC3339)}
		}},
		{"unix ms", "RFC 3339 ms", func(c time.Time) string { return strconv.FormatInt(c.UnixMilli(), 10) }, func(c time.Time) Leaf {
			return Leaf{Path: p, Noise: p, Type: String, Value: c.Format("2006-01-02T15:04:05.000Z07:00")}
		}},
	} {
		shape := func(t time.Time) Shape { return Shape{Leaves: map[string]Leaf{p: c.leaf(t)}} }
		for rn, full := range givenRegimes(recAt) {
			learnsAsWithNoGiven(t, c.nonce+" nonce beside createdAt "+c.form+", "+rn, p, shape, recAt, TestGiven("test o::TestApps", "given:nonce", c.of(recAt), c.of(full)))
		}
	}
}

// A given that holds a moment in another layout than a value it is not a
// text of is no place of that value (no moment rule reads a given): the
// application's own clock in a reference, and the given itself reformatted
// (its moment written in another layout, the test's clock on real time)
// learn as they do with no given. A reformatted given is not followed (a
// stated limit): give the value the application sends.
func TestAGivenInAnotherLayoutIsNoPlaceOfAMomentItNames(t *testing.T) {
	recAt := time.Date(2026, 10, 22, 11, 14, 17, 0, time.UTC)
	const p = "body.ref"
	// Each named by its layout: "s", "ms", "rfc" or "date".
	gives := map[string]func(time.Time) string{
		"s":   func(c time.Time) string { return "job-" + strconv.FormatInt(c.Unix(), 10) },
		"ms":  func(c time.Time) string { return "job-" + strconv.FormatInt(c.UnixMilli(), 10) },
		"rfc": func(c time.Time) string { return "job-" + c.Format(time.RFC3339) },
	}
	refs := map[string]func(time.Time) string{
		"rfc":   func(c time.Time) string { return "rep-" + c.Format(time.RFC3339) },
		"s":     func(c time.Time) string { return "rep-" + strconv.FormatInt(c.Unix(), 10) },
		"date":  func(c time.Time) string { return "rep " + c.Format("2006/01/02 15:04:05") },
		"rfc ":  func(c time.Time) string { return c.Format(time.RFC3339) },
		"date ": func(c time.Time) string { return c.Format("2006/01/02 15:04:05") },
	}
	at := recAt.Add(300 * time.Millisecond)
	for gn, g := range gives {
		for fn, f := range refs {
			shape := func(c time.Time) Shape {
				return Shape{Leaves: map[string]Leaf{p: {Path: p, Noise: p, Type: String, Value: f(c)}}}
			}
			for rn, full := range givenRegimes(recAt) {
				learnsAsWithNoGiven(t, "the app's "+fn+" beside a given "+gn+", "+rn, p, shape, recAt, TestGiven("test o::TestApps", "given:job", g(recAt), g(full)))
				// The given reformatted: the value moves with the given, not
				// by the lead.
				gave := []Earlier{TestGiven("test o::TestApps", "given:job", g(recAt), g(full))}
				alone := ClassifyIn(NewDriftCache(), []string{p}, shape(full), shape(recAt), at, nil, nil, nil, nil)[0]
				got := ClassifyIn(NewDriftCache(), []string{p}, shape(full), shape(recAt), at, gave, nil, nil, nil)[0]
				if strings.TrimSpace(fn) == gn {
					// A cut of the given's text, not its moment in
					// another layout (TestWhatAValueMadeFromAGivenLearnsAs).
					continue
				}
				if got.Kind != alone.Kind || got.Why != alone.Why || got.From != alone.From {
					t.Errorf("the given's %s moment reformatted as %q, %s: %s from %+v (%s); want it as with no given: %s", gn, fn, rn, got.Kind, got.From, got.Why, alone.Kind)
				}
			}
		}
	}
}

// A value made from a UnixNano nonce: the nonce whole is its variable; its
// exact text inside the value, between words or glued to letters, is a part
// (refused, naming the given); a form that does not hold its text (a hash,
// a cut) learns as it does with no given, which can be noise (a stated
// limit: give the value the application sends).
func TestWhatAValueMadeFromAGivenLearnsAs(t *testing.T) {
	recAt := time.Date(2026, 10, 22, 11, 14, 17, 0, time.UTC)
	const p = "body.name"
	const rec, full = "1761131656960000000", "1761225360000000001"
	hash := func(s string) string { x := sha256.Sum256([]byte(s)); return hex.EncodeToString(x[:]) }
	for _, c := range []struct {
		form string
		of   func(string) string
		want string // "whole", "part" or "alone"
	}{
		{"the nonce", func(s string) string { return s }, "whole"},
		{"app-<nonce>", func(s string) string { return "app-" + s }, "part"},
		{"[E2E] generate-stop-<nonce>", func(s string) string { return "[E2E] generate-stop-" + s }, "part"},
		{"APP_<nonce>", func(s string) string { return strings.ToUpper("app_" + s) }, "part"},
		{"app<nonce>", func(s string) string { return "app" + s }, "part"},
		{"<nonce>x9", func(s string) string { return s + "x9" }, "part"},
		{"base64 of JSON holding it", func(s string) string { return base64.StdEncoding.EncodeToString([]byte(`{"n":"` + s + `"}`)) }, "part"},
		{"sha256", hash, "alone"},
		{"its last 6 digits", func(s string) string { return s[len(s)-6:] }, "alone"},
		{"app-<its last 6 digits>", func(s string) string { return "app-" + s[len(s)-6:] }, "alone"},
	} {
		shape := func(v string) Shape {
			return Shape{Leaves: map[string]Leaf{p: {Path: p, Noise: p, Type: String, Value: v}}}
		}
		at := recAt.Add(300 * time.Millisecond)
		gave := []Earlier{TestGiven("test o::TestApps", "given:nonce", rec, full)}
		alone := ClassifyIn(NewDriftCache(), []string{p}, shape(c.of(full)), shape(c.of(rec)), at, nil, nil, nil, nil)[0]
		got := ClassifyIn(NewDriftCache(), []string{p}, shape(c.of(full)), shape(c.of(rec)), at, gave, nil, nil, nil)[0]
		switch c.want {
		case "alone":
			if got.Kind != alone.Kind || got.Why != alone.Why || got.From != alone.From {
				t.Errorf("%s: %s from %+v; want it as with no given: %s", c.form, got.Kind, got.From, alone.Kind)
			}
		default:
			if got.Kind != KindVariable || !got.From.Gave || got.From.Path != "given:nonce" || got.From.Whole != (c.want == "whole") {
				t.Errorf("%s: %s from %+v (%s); want given:nonce's %s", c.form, got.Kind, got.From, got.Why, c.want)
			}
		}
	}
}

// Where a given's text stands in a value, recorded and live: past a cut of
// one of the value's instants (its first hit, "14" of "11:14:17", is no
// part, a later one glued is), inside the value's escaped text, and on both
// sides at once (the live value still holds the recorded given: a part,
// refused naming the given, never learned unchecked). A value that holds
// the given on one side only learns as it does with no given.
func TestWhereAGivensTextStandsInAValue(t *testing.T) {
	recAt := time.Date(2026, 10, 22, 11, 14, 17, 0, time.UTC)
	const p = "body.name"
	const rec, full = "1761131656960000000", "1761225360000000001"
	stamp := func(c time.Time) string { return c.Format(time.RFC3339) }
	live := recAt.Add(FullRoundLead)
	for _, c := range []struct {
		form              string
		gRec, gLive       string
		vRec, vLive, want string // want: "part" or "alone"
	}{
		{"<createdAt>/slot14, given 14", "14", "15", stamp(recAt) + "/slot14", stamp(live) + "/slot15", "part"},
		{"<createdAt> n914, given 14", "14", "15", stamp(recAt) + " n914", stamp(live) + " n915", "part"},
		{"app-<nonce> whose live lost it", rec, full, "app-" + rec, "app-x9", "alone"},
		{"app-x9 whose live gained it", rec, full, "app-x9", "app-" + full, "alone"},
		{"percent-escaped, glued", "Acme Corp " + rec, "Acme Corp " + full, "q=Acme%20Corp%20" + rec + "x9", "q=Acme%20Corp%20" + full + "x9", "part"},
		{"live holds the recorded given too", rec, full, "ids " + rec, "ids " + full + "," + rec, "part"},
		{"recorded holds the live given too", rec, full, "ids " + rec + "," + full, "ids " + full, "part"},
	} {
		shape := func(v string) Shape {
			return Shape{Leaves: map[string]Leaf{p: {Path: p, Noise: p, Type: String, Value: v}}}
		}
		at := recAt.Add(300 * time.Millisecond)
		gave := []Earlier{TestGiven("test o::TestApps", "given:name", c.gRec, c.gLive)}
		alone := ClassifyIn(NewDriftCache(), []string{p}, shape(c.vLive), shape(c.vRec), at, nil, nil, nil, nil)[0]
		got := ClassifyIn(NewDriftCache(), []string{p}, shape(c.vLive), shape(c.vRec), at, gave, nil, nil, nil)[0]
		switch c.want {
		case "alone":
			if got.Kind != alone.Kind || got.Why != alone.Why || got.From != alone.From {
				t.Errorf("%s: %s from %+v; want it as with no given: %s", c.form, got.Kind, got.From, alone.Kind)
			}
		default:
			if got.Kind != KindVariable || !got.From.Gave || got.From.Path != "given:name" || got.From.Whole {
				t.Errorf("%s: %s from %+v (%s); want a part of given:name", c.form, got.Kind, got.From, got.Why)
			}
		}
	}
}

// Stated limit: a given whose text the application's clock read in the same
// call equals (a bare Unix-seconds stamp, or one in a reference) cannot be
// told from that read by its text, and is taken as the given's. Give a
// distinctive value (UnixNano, or a prefixed name).
func TestAGivenEqualToAClockReadIsTakenAsTheGivens(t *testing.T) {
	recAt := time.Date(2026, 10, 22, 11, 14, 17, 30e6, time.UTC)
	at := recAt.Add(300 * time.Millisecond)
	unix := func(c time.Time) string { return strconv.FormatInt(c.Unix(), 10) }
	for fn, f := range map[string]func(time.Time) string{
		"<unix s>":     unix,
		"rep-<unix s>": func(c time.Time) string { return "rep-" + unix(c) },
	} {
		const p = "body.createdAt"
		shape := func(c time.Time) Shape {
			return Shape{Leaves: map[string]Leaf{p: {Path: p, Noise: p, Type: String, Value: f(c)}}}
		}
		gave := []Earlier{TestGiven("test o::TestApps", "given:nonce", unix(recAt), unix(recAt.Add(FullRoundLead+5*time.Millisecond)))}
		got := ClassifyIn(NewDriftCache(), []string{p}, shape(recAt.Add(FullRoundLead)), shape(recAt), at, gave, nil, nil, nil)[0]
		if got.Kind != KindVariable || !got.From.Gave || got.From.Path != "given:nonce" {
			t.Errorf("createdAt %s equal to the given's text: %s from %+v; the stated limit takes it as given:nonce's", fn, got.Kind, got.From)
		}
	}
}
