package learn

import (
	"encoding/base64"
	"fmt"
	"net/url"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
)

// A zone keploy's rows write an aware datetime in: its name in a row's
// title, its offset from UTC, and how an ISO 8601 writer spells it.
type rowZone struct {
	name string
	loc  *time.Location
	iso  string
}

// zonedRows are the zones every row below writes its datetimes in: one
// ahead of UTC by a half hour (India's), one behind it by one (Newfoundland's),
// UTC written "Z" and "+00:00", and one ahead by whole hours.
var zonedRows = []rowZone{
	{"+05:30", ist, "2006-01-02T15:04:05Z07:00"},
	{"-03:30", time.FixedZone("NST", -(3*3600 + 1800)), "2006-01-02T15:04:05Z07:00"},
	{"Z", time.UTC, "2006-01-02T15:04:05Z07:00"},
	{"+00:00", time.UTC, "2006-01-02T15:04:05-07:00"},
	{"+01:00", time.FixedZone("CET", 3600), "2006-01-02T15:04:05Z07:00"},
}

// pgText is at as Postgres writes a timestamptz in text: a space before
// its time, and its zone's offset as "+05:30", or "+01" at a whole hour.
func pgText(at time.Time) string {
	_, off := at.Zone()
	sign := '+'
	if off < 0 {
		sign, off = '-', -off
	}
	zone := fmt.Sprintf("%c%02d", sign, off/3600)
	if m := off % 3600 / 60; m != 0 {
		zone += fmt.Sprintf(":%02d", m)
	}
	return at.Format("2006-01-02 15:04:05") + zone
}

// An aware datetime's text is the instant it is, read by the clock's rules,
// whether or not it is a read of the clock: its zone's sign glued to the
// seconds before it ("13+05" of "2026-10-31T16:20:13+05:30") makes no run
// of the base64 alphabet that does not decode, and the value is not refused
// as one, in any zone, at the second, the millisecond, at four fraction
// digits with no colon in its zone ("3050+0530") or in Postgres's text. What
// stands beside the instant is still read:
// a run that does not decode, apart from it or glued to its end, that
// changed is refused.
func TestAnInstantsZoneMakesNoBase64Run(t *testing.T) {
	rec := time.Date(2026, 10, 31, 10, 50, 13, 305_000_000, time.UTC)
	live := rec.Add(73*time.Second + 600*time.Millisecond)
	for _, z := range zonedRows {
		for _, form := range []struct {
			what string
			text func(time.Time) string
		}{
			{"at the second", func(at time.Time) string { return at.In(z.loc).Format(z.iso) }},
			{"at the millisecond", func(at time.Time) string { return at.In(z.loc).Format(strings.Replace(z.iso, "05Z", "05.000Z", 1)) }},
			{"at four fraction digits, no colon in its zone", func(at time.Time) string { return at.In(z.loc).Format("2006-01-02T15:04:05.0000-0700") }},
			{"in a sentence", func(at time.Time) string { return "due " + at.In(z.loc).Format(z.iso) + " sharp" }},
			{"in Postgres's text", func(at time.Time) string { return pgText(at.In(z.loc)) }},
		} {
			r, l := form.text(rec), form.text(live)
			if r == l {
				t.Fatalf("%s %s: the row's values do not drift (%q)", z.name, form.what, r)
			}
			if why := unparsed(r, l, clockSet{}); why != "" {
				t.Errorf("%s %s (%q -> %q): refused %q; want its instant read by the clock's rules", z.name, form.what, r, l, why)
			}
			for run := range b64Runs(r) {
				if strings.ContainsAny(run, "+=") {
					t.Errorf("%s %s: b64Runs(%q) holds %q, a run of its instant", z.name, form.what, r, run)
				}
			}
		}
		at := func(at time.Time) string { return at.In(z.loc).Format(z.iso) }
		for _, c := range []struct{ what, rec, live string }{
			{"a run apart from it", "sig=ab+cd at " + at(rec), "sig=ef+gh at " + at(live)},
			{"a run apart from it, the instant the same", "sig=ab+cd at " + at(rec), "sig=ef+gh at " + at(rec)},
			{"a run glued to its end", at(rec) + "+ab+c", at(live) + "+ef+g"},
		} {
			if why := unparsed(c.rec, c.live, clockSet{}); why != "it changed in a run of the base64 alphabet that does not decode" {
				t.Errorf("%s, %s (%q -> %q): %q; want refused as a run of the base64 alphabet that does not decode", z.name, c.what, c.rec, c.live, why)
			}
		}
	}
}

// A zoned datetime a call sends is judged by the clock's rules in each
// protocol's learn: keploy's HTTP learn and the Postgres learn
// (ClassifyGiven), a value as text and a Postgres timestamptz's text, and
// the Mongo learn (ClassifyFollowing), a string in a command; in every
// zone, "+05:30" and "-03:30" as "Z", "+00:00" and "+01:00". A poll of
// now - 60s in one of keploy mock's client tests is learned as the clock
// - 1m, which a replay checks: a look-back of a minute in the replay's
// call passes and one of two refuses. The call's own read is learned as a
// read of the clock. keploy test, whose calls know no test begin, refuses
// the poll by the clock's rules (the instant is no read its window holds),
// never as a run of the base64 alphabet. Each outcome is the one the value
// written in UTC gets.
func TestAZonedDatetimeIsJudgedByTheClocksRulesInEveryProtocol(t *testing.T) {
	call := time.Date(2026, 10, 31, 10, 52, 15, 438_000_000, time.UTC)
	began := call.Add(-time.Second)
	callF := call.Add(73*time.Second + 600*time.Millisecond)
	beganF := began.Add(72 * time.Second)
	w := (&Window{Rec: Span{From: call.Add(-100 * time.Millisecond), To: call, Began: began}, F: Span{From: callF.Add(-100 * time.Millisecond), To: callF, Began: beganF}}).ComparesOffsets()
	type protocol struct {
		name, path string
		text       func(z rowZone, at time.Time) string
		classify   func(dc *DriftCache, p string, live, rec Shape) Learned
	}
	given := func(dc *DriftCache, p string, live, rec Shape) Learned {
		return ClassifyGiven(dc, []string{p}, live, rec, call, nil, nil, nil, w, nil)[0]
	}
	iso := func(z rowZone, at time.Time) string { return at.In(z.loc).Format(z.iso) }
	protocols := []protocol{
		{"HTTP", "body.since", iso, given},
		{"Postgres", "Req.query.bind[0]", func(z rowZone, at time.Time) string { return pgText(at.In(z.loc)) }, given},
		{"Mongo", "body.cmd.filter.dirtyAt.$lte", iso, func(dc *DriftCache, p string, live, rec Shape) Learned {
			return ClassifyFollowing(dc, []string{p}, live, rec, call, nil, nil, nil, w, nil)[0]
		}},
	}
	const notRead = "it is one instant whole that its call's window on the application's clock does not hold in both runs"
	for _, pr := range protocols {
		learn := func(dc *DriftCache, z rowZone, ago time.Duration) Learned {
			shape := func(at time.Time) Shape {
				return Shape{Leaves: map[string]Leaf{pr.path: {Path: pr.path, Noise: pr.path, Type: String, Value: pr.text(z, at.Add(-ago))}}}
			}
			return pr.classify(dc, pr.path, shape(callF), shape(call))
		}
		utc := zonedRows[2]
		for _, z := range zonedRows {
			t.Run(pr.name+" "+z.name, func(t *testing.T) {
				// The poll, in a client test.
				poll, inUTC := learn(clientTestCacheIn(z.loc), z, time.Minute), learn(clientTestCache(), utc, time.Minute)
				if poll.Kind != KindClockOffset || poll.Offset == nil || poll.Offset.Min > -time.Minute || poll.Offset.Max < -time.Minute {
					t.Fatalf("the poll of now - 60s (%q) learned %s (%v): %s; want the clock - 1m", poll.Recorded.Value, poll.Kind, poll.Offset, poll.Why)
				}
				if inUTC.Kind != poll.Kind || inUTC.Offset.Min != poll.Offset.Min || inUTC.Offset.Max != poll.Offset.Max {
					t.Fatalf("the poll learned %v in %s, %v in UTC; want alike", poll.Offset, z.name, inUTC.Offset)
				}
				replay := call.Add(3 * time.Hour)
				s := Span{From: replay.Add(-100 * time.Millisecond), To: replay, Zone: z.loc, Own: true, Began: replay.Add(-time.Second)}
				leaf := func(at time.Time) Leaf {
					return Leaf{Path: pr.path, Noise: pr.path, Type: String, Value: pr.text(z, at)}
				}
				if ok, why := CheckClockRead(pr.path, *poll.Offset, leaf(replay.Add(-time.Minute)), s, true); !ok {
					t.Errorf("a replay's poll of now - 60s: refused (%s); want passed", why)
				}
				if ok, _ := CheckClockRead(pr.path, *poll.Offset, leaf(replay.Add(-2*time.Minute)), s, true); ok {
					t.Errorf("a replay's poll of now - 120s: passed; want refused")
				}
				// The call's own read, in a client test.
				if own := learn(clientTestCacheIn(z.loc), z, 0); own.Kind != KindClockRead || own.Read == nil {
					t.Errorf("the call's own read (%q) learned %s: %s; want a read of the clock", own.Recorded.Value, own.Kind, own.Why)
				}
				// The poll, in keploy test.
				kt := learn(clockedDrifts(), z, time.Minute)
				if kt.Kind != KindUnaccounted || !strings.HasPrefix(kt.Why, notRead) {
					t.Errorf("keploy test's poll (%q) learned %s: %s; want refused as no read its window holds", kt.Recorded.Value, kt.Kind, kt.Why)
				}
				if ref := learn(clockedDrifts(), utc, time.Minute); ref.Kind != kt.Kind || ref.Why != kt.Why {
					t.Errorf("keploy test's poll learned %s (%s) in %s, %s (%s) in UTC; want alike", kt.Kind, kt.Why, z.name, ref.Kind, ref.Why)
				}
			})
		}
	}
}

// An instant inside a longer text is judged by the clock's rules as the same
// instant whole is, in each protocol's learn and in every zone ("+05:30",
// "-03:30", "Z", "+00:00", Postgres's "+00"): a query's literal, a word of a
// note. In keploy test a poll of now - 1m or now - 3h, one instant or two
// in one text, is refused, saying which instant its call's window does not
// hold, never learned as noise that would let any value pass at every
// replay (and never as a run of the base64 alphabet). In keploy mock's
// client tests a poll of now - 60s inside a text is learned as the clock -
// 1m, as the instant whole is, at the word the instant stands at: a replay
// checks that word is the clock - 1m in its call and compares the rest of
// the text.
func TestAnInstantInsideATextIsJudgedAsTheInstantWhole(t *testing.T) {
	call := time.Date(2026, 10, 31, 10, 52, 15, 438_000_000, time.UTC)
	began := call.Add(-time.Second)
	callF := call.Add(73*time.Second + 600*time.Millisecond)
	beganF := began.Add(72 * time.Second)
	w := (&Window{Rec: Span{From: call.Add(-100 * time.Millisecond), To: call, Began: began}, F: Span{From: callF.Add(-100 * time.Millisecond), To: callF, Began: beganF}}).ComparesOffsets()
	given := func(dc *DriftCache, p string, live, rec Shape) Learned {
		return ClassifyGiven(dc, []string{p}, live, rec, call, nil, nil, nil, w, nil)[0]
	}
	iso := func(z rowZone, at time.Time) string { return at.In(z.loc).Format(z.iso) }
	pg := func(z rowZone, at time.Time) string { return pgText(at.In(z.loc)) }
	type protocol struct {
		name, path string
		instant    func(z rowZone, at time.Time) string
		text       func(instant string) string
		classify   func(dc *DriftCache, p string, live, rec Shape) Learned
	}
	protocols := []protocol{
		{"HTTP", "body.note", iso, func(s string) string { return "jobs due before " + s + ", page 2" }, given},
		{"Postgres", "Req.query.text", pg, func(s string) string { return "SELECT id FROM jobs WHERE run_at > '" + s + "' LIMIT 10" }, given},
		{"Mongo", "body.cmd.filter.note", iso, func(s string) string { return "dirty<=" + s + ";shard=7" }, func(dc *DriftCache, p string, live, rec Shape) Learned {
			return ClassifyFollowing(dc, []string{p}, live, rec, call, nil, nil, nil, w, nil)[0]
		}},
	}
	const notHeld = "that its call's window on the application's clock does not hold in both runs"
	for _, pr := range protocols {
		learn := func(dc *DriftCache, value func(at time.Time) string) Learned {
			shape := func(at time.Time) Shape {
				return Shape{Leaves: map[string]Leaf{pr.path: {Path: pr.path, Noise: pr.path, Type: String, Value: value(at)}}}
			}
			return pr.classify(dc, pr.path, shape(callF), shape(call))
		}
		for _, z := range zonedRows {
			whole := func(ago time.Duration) func(at time.Time) string {
				return func(at time.Time) string { return pr.instant(z, at.Add(-ago)) }
			}
			inText := func(ago time.Duration) func(at time.Time) string {
				return func(at time.Time) string { return pr.text(pr.instant(z, at.Add(-ago))) }
			}
			t.Run(pr.name+" "+z.name, func(t *testing.T) {
				// keploy test: the text gets the instant whole's outcome. A
				// poll, one instant or two in one text, is refused, naming
				// where the instant stands; the call's own read, one the
				// lead moved, and a time of day (read modulo a day) are not.
				lead := func(at time.Time) time.Time {
					if at.Equal(call) {
						return call.Add(-30 * time.Second)
					}
					return call.Add(-30*time.Second + FullRoundLead)
				}
				tod := func(at time.Time) string { return at.Add(-time.Minute).In(z.loc).Format("15:04:05") }
				for _, c := range []struct {
					what    string
					instant func(at time.Time) string
					refused bool
					text    func(at time.Time) string
				}{
					{"now - 1m", whole(time.Minute), true, nil},
					{"now - 3h", whole(3 * time.Hour), true, nil},
					{"two instants, now - 3h and now", whole(3 * time.Hour), true, func(at time.Time) string {
						return pr.text(pr.instant(z, at.Add(-3*time.Hour)) + "' AND '" + pr.instant(z, at))
					}},
					{"the call's own read", whole(0), false, nil},
					{"a read the lead moved", func(at time.Time) string { return pr.instant(z, lead(at)) }, false, nil},
					{"a time of day", tod, false, nil},
					{"a wall clock of no zone, the call's own read in the application's zone", func(at time.Time) string { return at.In(z.loc).Format("2006-01-02 15:04:05") }, false, nil},
				} {
					text := c.text
					if text == nil {
						text = func(at time.Time) string { return pr.text(c.instant(at)) }
					}
					inZone := func() *DriftCache {
						dc := clockedDrifts()
						dc.zone = z.loc
						return dc
					}
					wl, kt := learn(inZone(), c.instant), learn(inZone(), text)
					if !c.refused {
						if wl.Kind != KindClockRead {
							t.Fatalf("keploy test, %s whole (%q): learned %s: %q; the row wants a read of the clock", c.what, wl.Recorded.Value, wl.Kind, wl.Why)
						}
						if kt.Kind == KindUnaccounted {
							t.Errorf("keploy test, %s in a text (%q): refused %q; want not refused, as the instant whole is not", c.what, kt.Recorded.Value, kt.Why)
						}
						continue
					}
					if wl.Kind != KindUnaccounted || wl.Why != outsideWindowWhy {
						t.Fatalf("keploy test, %s whole (%q): learned %s: %q; the row wants it refused as no read its window holds", c.what, wl.Recorded.Value, wl.Kind, wl.Why)
					}
					if kt.Kind != KindUnaccounted || !strings.Contains(kt.Why, notHeld) || !strings.Contains(kt.Why, "noise there would let any value pass at every replay") {
						t.Errorf("keploy test, %s in a text (%q): learned %s: %q; want refused as an instant its call's window does not hold", c.what, kt.Recorded.Value, kt.Kind, kt.Why)
					}
					if !strings.HasPrefix(kt.Why, "it holds an instant (its characters ") {
						t.Errorf("keploy test, %s in a text: %q names no place of the instant", c.what, kt.Why)
					}
				}
				// keploy mock's client tests: a poll of now - 60s and of now -
				// 3h inside a text gets the outcome the instant whole gets.
				for _, ago := range []time.Duration{time.Minute, 3 * time.Hour} {
					wl, tl := learn(clientTestCacheIn(z.loc), whole(ago)), learn(clientTestCacheIn(z.loc), inText(ago))
					if tl.Kind != wl.Kind {
						t.Fatalf("a client test's poll of now - %v: learned %s whole (%s), %s inside a text (%q: %s); want alike", ago, wl.Kind, wl.Why, tl.Kind, tl.Recorded.Value, tl.Why)
					}
					if wl.Kind == KindClockOffset && (tl.Offset == nil || tl.Offset.Min != wl.Offset.Min || tl.Offset.Max != wl.Offset.Max || tl.Offset.Word.IsZero()) {
						t.Fatalf("a client test's poll of now - %v: whole %v, inside a text %v; want the same offset, at the instant's word", ago, wl.Offset, tl.Offset)
					}
				}
				poll := learn(clientTestCacheIn(z.loc), inText(time.Minute))
				if poll.Kind != KindClockOffset || poll.Offset == nil || poll.Offset.Min > -time.Minute || poll.Offset.Max < -time.Minute {
					t.Fatalf("a client test's poll of now - 60s in a text (%q) learned %s (%v): %s; want the clock - 1m", poll.Recorded.Value, poll.Kind, poll.Offset, poll.Why)
				}
				replay := call.Add(3 * time.Hour)
				s := Span{From: replay.Add(-100 * time.Millisecond), To: replay, Zone: z.loc, Own: true, Began: replay.Add(-time.Second)}
				leaf := func(v string) Leaf { return Leaf{Path: pr.path, Noise: pr.path, Type: String, Value: v} }
				if ok, why := CheckClockRead(pr.path, *poll.Offset, leaf(pr.text(pr.instant(z, replay.Add(-time.Minute)))), s, true); !ok {
					t.Errorf("a replay's poll of now - 60s in the text: refused (%s); want passed", why)
				}
				if ok, _ := CheckClockRead(pr.path, *poll.Offset, leaf(pr.text(pr.instant(z, replay.Add(-2*time.Minute)))), s, true); ok {
					t.Errorf("a replay's poll of now - 120s in the text: passed; want refused")
				}
				if ok, _ := CheckClockRead(pr.path, *poll.Offset, leaf(strings.Replace(pr.text(pr.instant(z, replay.Add(-time.Minute))), "jobs", "users", 1)+"!"), s, true); ok {
					t.Errorf("a replay's text that changed around the poll: passed; want refused")
				}
			})
		}
	}
}

// A run of the base64 alphabet that changed, glued to the start of an
// instant ("new+york/" before "2026-10-31"), is read apart from the instant
// as one glued to its end is, and refused where it does not decode, as it
// is with no instant beside it; before the instant was read apart, the
// date's dashes made the run hold a '-' and it was let go.
func TestAChangedRunGluedToAnInstantsStartIsRefused(t *testing.T) {
	const want = "it changed in a run of the base64 alphabet that does not decode"
	for _, c := range []struct{ what, rec, live string }{
		{"a date", "/search/new+york/2026-10-31", "/search/los+gato/2026-10-31"},
		{"an aware datetime", "/search/new+york/2026-10-31T16:20:13+05:30", "/search/los+gato/2026-10-31T16:20:13+05:30"},
		{"no instant", "/search/new+york/", "/search/los+gato/"},
	} {
		if why := unparsed(c.rec, c.live, clockSet{}); why != want {
			t.Errorf("%s (%q -> %q): %q; want %q", c.what, c.rec, c.live, why, want)
		}
	}
}

// One rule for an instant wherever it stands (founder, 2026-10-31): an
// instant inside a text, or inside an encoded form of a text (percent-
// encoded, a base64 or base64url cursor, a JSON string's escapes, "+"
// for a zone's '+'), at its start or its end, gets the outcome the same
// instant whole gets, in keploy test (a call with a window and one with
// none) and in keploy mock's client tests, in every zone ("+05:30",
// "-03:30", "Z", "+00:00", "+01:00", Postgres's "+00"), for the call's own
// read, a poll of now - 2s, now - 60s or now - 3h, one of now + 1h, and,
// in keploy test, a read the lead moved: refused whole, it is refused in
// the text, saying why the instant whole is; learned whole as a read of the
// clock, plain or plus an offset, it is learned as that read at its word,
// in the text decoded once, the learning holding the whole text, and a
// replay passes the text with a read made so and refuses one whose text
// around the word changed. Never noise, which would let any value pass at
// every replay.
func TestAnInstantIsJudgedAsTheInstantWholeWhereverItStands(t *testing.T) {
	cursor := func(enc *base64.Encoding) func(string) string {
		return func(s string) string { return "cursor=" + enc.EncodeToString([]byte(`{"since":"`+s+`","page":2}`)) }
	}
	judgedAsWhole(t, []textForm{
		{"in a text", func(s string) string { return "jobs due before " + s + ", page 2" }},
		{"at a text's start", func(s string) string { return s + " sharp" }},
		{"at a text's end", func(s string) string { return "due " + s }},
		// An instant that did not change beside the one that did is read
		// as the text it stands in.
		{"beside an instant that did not change", func(s string) string { return "between 2026-01-01T00:00:00Z and " + s }},
		{"percent-encoded", func(s string) string {
			return "since=" + strings.ReplaceAll(url.QueryEscape(s), "+", "%20") + "&page=2"
		}},
		{"in a base64 cursor", cursor(base64.StdEncoding)},
		{"in a base64url cursor", cursor(base64.RawURLEncoding)},
		{"in a JSON string's escapes", func(s string) string { return `{\"since\":\"` + s + `\",\"page\":2}` }},
		{"with its '+' escaped", func(s string) string { return "since " + strings.ReplaceAll(s, "+", `\u002b`) + " page 2" }},
	})
}

// An instant that is a value whole in an encoded form (percent-encoded, a
// bare base64 or base64url cursor of it) is the instant whole: decoded
// once, it is judged by the plain whole's rule, in every zone, at every
// age, in keploy test with a window and without one and in keploy mock's
// client tests, under the rule a copy follows its source and without it:
// refused for the plain whole's reason, or learned as the plain whole's
// read at its word, the whole of its decoded form
// (models.ClockWordDecoded), which a replay checks as the plain whole.
func TestAnEncodedInstantWholeIsJudgedAsThePlainWhole(t *testing.T) {
	judgedAsWhole(t, []textForm{
		{"percent-encoded whole", func(s string) string { return strings.ReplaceAll(url.QueryEscape(s), "+", "%20") }},
		{"a bare base64 cursor of it", func(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }},
		{"a bare base64url cursor of it", func(s string) string { return base64.RawURLEncoding.EncodeToString([]byte(s)) }},
	})
}

// textForm is a form a row writes an instant in: its name, and the text
// holding the instant.
type textForm struct {
	name string
	text func(instant string) string
}

// judgedAsWhole checks that an instant written in each of forms is learned
// as the same instant whole is, in every zone and at every age, in keploy
// test (a call with a window and one with none) and in keploy mock's
// client tests, under the rule a copy follows its source and without it,
// and that a replay checks its word as the whole.
func judgedAsWhole(t *testing.T, forms []textForm) {
	t.Helper()
	call := time.Date(2026, 10, 31, 10, 52, 15, 438_000_000, time.UTC)
	began := call.Add(-time.Second)
	callF := call.Add(73*time.Second + 600*time.Millisecond)
	beganF := began.Add(72 * time.Second)
	w := (&Window{Rec: Span{From: call.Add(-100 * time.Millisecond), To: call, Began: began}, F: Span{From: callF.Add(-100 * time.Millisecond), To: callF, Began: beganF}}).ComparesOffsets()
	const p = "body.note"
	type spelling struct {
		name    string
		loc     *time.Location
		instant func(at time.Time) string
	}
	var spellings []spelling
	for _, z := range zonedRows {
		spellings = append(spellings, spelling{z.name, z.loc, func(at time.Time) string { return at.In(z.loc).Format(z.iso) }})
	}
	spellings = append(spellings, spelling{"Postgres +00", time.UTC, func(at time.Time) string { return pgText(at.UTC()) }})
	type classify = func(dc *DriftCache, drift []string, live, rec Shape, at time.Time, earlier []Earlier, raw []Raw, resp []Leaf, w *Window, given GivenEarlier) []Learned
	kt := func(z *time.Location) *DriftCache { dc := clockedDrifts(); dc.zone = z; return dc }
	modes := []struct {
		name   string
		cache  func(*time.Location) *DriftCache
		window *Window
		lead   bool
		cl     classify
	}{
		{"keploy test, a call with a window", kt, w, true, ClassifyGiven},
		{"keploy test, a call with no window", kt, nil, true, ClassifyGiven},
		{"keploy test under the rule a copy follows its source, a call with a window", kt, w, true, ClassifyFollowing},
		{"keploy test under the rule a copy follows its source, a call with no window", kt, nil, true, ClassifyFollowing},
		{"a client test", clientTestCacheIn, w, false, ClassifyGiven},
		{"a client test under the rule a copy follows its source", clientTestCacheIn, w, false, ClassifyFollowing},
	}
	for _, sp := range spellings {
		t.Run(sp.name, func(t *testing.T) {
			for _, m := range modes {
				reads := map[string]func(at time.Time) time.Time{
					"the call's own read": func(at time.Time) time.Time { return at },
					"now - 2s":            func(at time.Time) time.Time { return at.Add(-2 * time.Second) },
					"now - 60s":           func(at time.Time) time.Time { return at.Add(-time.Minute) },
					"now - 3h":            func(at time.Time) time.Time { return at.Add(-3 * time.Hour) },
					"now + 1h":            func(at time.Time) time.Time { return at.Add(time.Hour) },
				}
				if m.lead {
					reads["a read the lead moved"] = func(at time.Time) time.Time {
						if at.Equal(call) {
							return call.Add(-30 * time.Second)
						}
						return call.Add(-30*time.Second + FullRoundLead)
					}
				}
				for what, read := range reads {
					learn := func(value func(at time.Time) string) Learned {
						shape := func(at time.Time) Shape {
							return Shape{Leaves: map[string]Leaf{p: {Path: p, Noise: p, Type: String, Value: value(at)}}}
						}
						return m.cl(m.cache(sp.loc), []string{p}, shape(callF), shape(call), call, nil, nil, nil, m.window, nil)[0]
					}
					whole := learn(func(at time.Time) string { return sp.instant(read(at)) })
					for _, f := range forms {
						in := learn(func(at time.Time) string { return f.text(sp.instant(read(at))) })
						where := fmt.Sprintf("%s, %s %s (%q)", m.name, what, f.name, in.Recorded.Value)
						if in.Kind != whole.Kind {
							t.Errorf("%s: learned %s (%s); the instant whole learned %s (%s); want alike", where, in.Kind, in.Why, whole.Kind, whole.Why)
							continue
						}
						if in.Recorded.Value != f.text(sp.instant(read(call))) || in.Live.Value != f.text(sp.instant(read(callF))) {
							t.Errorf("%s: the learning holds %q and %q, not the whole text", where, in.Recorded.Value, in.Live.Value)
						}
						switch whole.Kind {
						case KindUnaccounted:
							// keploy test refuses the text by the instant
							// whole's refusal; a client test refuses it by
							// the same reason, naming where the instant
							// stands where the whole names it whole.
							same := strings.HasPrefix(in.Why, "it holds an instant (") && strings.HasSuffix(in.Why, "which keploy refuses as it refuses that instant whole: "+whole.Why)
							if !m.lead {
								same = unplaced(in.Why) == unplaced(whole.Why) && strings.Contains(in.Why, "(an instant, ")
							}
							if !same {
								t.Errorf("%s: refused %q; want the instant whole's refusal (%q), naming where it stands", where, in.Why, whole.Why)
							}
							continue
						case KindClockRead, KindClockOffset:
						default:
							t.Fatalf("%s: the instant whole learned %s (%s); the row wants a read of the clock or a refusal", where, whole.Kind, whole.Why)
						}
						wo, io := whole.Read, in.Read
						if whole.Kind == KindClockOffset {
							wo, io = whole.Offset, in.Offset
						}
						if io == nil || io.Word.IsZero() {
							t.Errorf("%s: learned %s with no word: %+v", where, in.Kind, io)
							continue
						}
						got := *io
						got.Word = models.ClockWord{}
						if got != *wo {
							t.Errorf("%s: learned %+v at its word; the instant whole %+v; want the same read", where, got, *wo)
						}
						// A replay a day later makes the same read in its call.
						replay := call.Add(24 * time.Hour)
						s := Span{From: replay.Add(-100 * time.Millisecond), To: replay, Zone: sp.loc, Own: true, Began: replay.Add(-time.Second)}
						at := func(v string) Leaf { return Leaf{Path: p, Noise: p, Type: String, Value: v} }
						okWhole, _ := CheckClockRead(p, *wo, at(sp.instant(read(replay))), s, true)
						if ok, why := CheckClockRead(p, *io, at(f.text(sp.instant(read(replay)))), s, true); ok != okWhole {
							t.Errorf("%s: a replay's text passed %v (%s); the instant whole passed %v", where, ok, why, okWhole)
						}
						if ok, _ := CheckClockRead(p, *io, at(f.text(sp.instant(read(replay)))+"!"), s, true); ok {
							t.Errorf("%s: a replay's text that changed after the word passed; want refused", where)
						}
					}
				}
			}
		})
	}
}

// unplaced is why, a refusal, with the place it names, its first
// parenthesis, taken out.
func unplaced(why string) string {
	i := strings.Index(why, " (")
	j := strings.Index(why, "): ")
	if i < 0 || j < i {
		return why
	}
	return why[:i] + why[j+1:]
}

// readAtWord reports whether ln is a plain read of the clock at the word of
// its recorded text between before and after, the text whole in Recorded
// and Live: what the one rule for an instant wherever it stands learns a
// text whose one changed instant is, whole, a plain read of the clock
// (wordRead).
func readAtWord(ln Learned, before, after string) bool {
	o, ok := ln.Clock()
	rec := ln.Recorded.Value
	if !plainRead(ln) || !ok || !strings.HasPrefix(rec, before) || !strings.HasSuffix(rec, after) || len(rec) <= len(before)+len(after) {
		return false
	}
	return o.Word == models.ClockWordOf(rec, len(before), len(rec)-len(after))
}

// asWhole says how in, a text holding a changed instant, is not learned as
// whole, the same instant whole, is: "" where it is. Refused, the text is
// refused for the instant whole's reason (keploy test names where the
// instant stands before it; a client test names it in the reason's own
// place); learned whole as a read of the clock, plain or plus an offset,
// the text is learned as that read at the instant's word.
func asWhole(in, whole Learned) string {
	if in.Kind != whole.Kind {
		return fmt.Sprintf("learned %s (%s); the instant whole %s (%s)", in.Kind, in.Why, whole.Kind, whole.Why)
	}
	switch in.Kind {
	case KindUnaccounted:
		if in.Why == whole.Why || unplaced(in.Why) == unplaced(whole.Why) && strings.Contains(in.Why, "(an instant, ") ||
			strings.HasPrefix(in.Why, "it holds an instant (") && strings.HasSuffix(in.Why, "which keploy refuses as it refuses that instant whole: "+whole.Why) {
			return ""
		}
		return fmt.Sprintf("refused %q; the instant whole %q", in.Why, whole.Why)
	case KindClockRead, KindClockOffset:
		wo, io := whole.Read, in.Read
		if in.Kind == KindClockOffset {
			wo, io = whole.Offset, in.Offset
		}
		if wo == nil || io == nil || io.Word.IsZero() {
			return fmt.Sprintf("learned %s %+v, no read at a word; the instant whole %+v", in.Kind, io, wo)
		}
		got := *io
		got.Word = models.ClockWord{}
		if got != *wo {
			return fmt.Sprintf("learned %+v at its word; the instant whole %+v", got, *wo)
		}
		return ""
	}
	return fmt.Sprintf("learned %s (%s); the row wants a read of the clock or a refusal", in.Kind, in.Why)
}

// api-server's poll inside a filter string ("dirty<=<now - 2s>;shard=7",
// "<now - 60s>"), with and without an answer the test was served in F's
// second beside it (mock-1070's claimedAt, a moment record's full round
// saw that a pick from it may give), gets the outcome the poll whole gets,
// in keploy mock's client tests and in keploy test, under the rule a copy
// follows its source and without it, in every zone ("+05:30", "-03:30",
// "Z", "+00:00", "+01:00", Postgres's "+00"): refused for the whole's
// reason, or learned as the whole's read at its word.
func TestAPollInsideAFilterGetsTheWholePollsOutcome(t *testing.T) {
	const (
		dirty = "body.cmd.query.$and.0.$or.0.dirtyAt.$lte"
		stamp = "body.cursor.firstBatch.0.claimedAt.$date.$numberLong"
	)
	payload := func(name string, at time.Time) Raw {
		return RawOf(&models.Mock{Name: name, Spec: models.MockSpec{ReqTimestampMock: at, ResTimestampMock: at.Add(time.Millisecond)}}, true).ServedAs(1)
	}
	began := time.Date(2026, 10, 31, 10, 52, 14, 430_000_000, time.UTC)
	call := time.Date(2026, 10, 31, 10, 52, 15, 438_000_000, time.UTC)
	beganF, callF := began.Add(72*time.Second), call.Add(73*time.Second+600*time.Millisecond)
	w := (&Window{Rec: Span{From: call.Add(-100 * time.Millisecond), To: call, Began: began}, F: Span{From: callF.Add(-100 * time.Millisecond), To: callF, Began: beganF}}).ComparesOffsets()
	type classify = func(dc *DriftCache, drift []string, live, rec Shape, at time.Time, earlier []Earlier, raw []Raw, resp []Leaf, w *Window, given GivenEarlier) []Learned
	rules := map[string]classify{"without the rule a copy follows its source": ClassifyGiven, "under the rule a copy follows its source": ClassifyFollowing}
	learn := func(cl classify, client bool, zone *time.Location, value func(time.Time) string, ago time.Duration, pick bool) Learned {
		tr := NewTracker()
		tr.SetClientTests(client)
		tr.Begin()
		n, _, _, _ := tr.Test()
		if pick {
			at := call.Add(-500 * time.Millisecond)
			tr.Served(n, Earlier{Mock: "mock-1070", At: at, Own: true, Payload: payload("mock-1070", at), Seen: []Seen{
				{Side: Response, Leaf: InstantLeaf(stamp, stamp, at, time.Millisecond), Live: InstantLeaf(stamp, stamp, callF.Add(-ago+200*time.Millisecond), time.Millisecond).Value, Place: stamp},
			}})
		}
		_, earlier, _, _ := tr.Test()
		shape := func(at time.Time) Shape {
			return Shape{Leaves: map[string]Leaf{dirty: {Path: dirty, Noise: dirty, Type: String, Value: value(at.Add(-ago))}}}
		}
		dc := clientTestCacheIn(zone)
		if !client {
			dc = clockedDrifts()
			dc.zone = zone
		}
		return cl(dc, []string{dirty}, shape(callF), shape(call), call, earlier, nil, nil, w, nil)[0]
	}
	type spelling struct {
		name    string
		loc     *time.Location
		instant func(at time.Time) string
	}
	var spellings []spelling
	for _, z := range zonedRows {
		spellings = append(spellings, spelling{z.name, z.loc, func(at time.Time) string { return at.In(z.loc).Format(z.iso) }})
	}
	spellings = append(spellings, spelling{"Postgres +00", time.UTC, func(at time.Time) string { return pgText(at.UTC()) }})
	for _, sp := range spellings {
		t.Run(sp.name, func(t *testing.T) {
			inText := func(at time.Time) string { return "dirty<=" + sp.instant(at) + ";shard=7" }
			for _, ago := range []time.Duration{2 * time.Second, time.Minute} {
				for _, client := range []bool{true, false} {
					for _, pick := range []bool{false, true} {
						for rule, cl := range rules {
							whole := learn(cl, client, sp.loc, sp.instant, ago, pick)
							in := learn(cl, client, sp.loc, inText, ago, pick)
							if why := asWhole(in, whole); why != "" {
								t.Errorf("now - %v, client test %v, its answer in F's second beside it %v, %s: %s", ago, client, pick, rule, why)
							}
						}
					}
				}
			}
		})
	}
}

// A text holding two changed instants is refused where the instant whole
// of either is, whichever stands first: the call's own read, which its
// window holds, before or after a time the harness gave three hours back
// that moved ten minutes in record's full round (neither the clock, the clock
// plus one offset, nor the lead),
// in keploy test (a call with a window) and in keploy mock's client tests,
// in every zone. keploy test names where the refused instant stands and
// says why the instant whole is refused.
func TestATextIsRefusedWhereAnyInstantOfItIsRefusedWhole(t *testing.T) {
	call := time.Date(2026, 10, 31, 10, 52, 15, 438_000_000, time.UTC)
	began := call.Add(-time.Second)
	callF := call.Add(73*time.Second + 600*time.Millisecond)
	w := (&Window{Rec: Span{From: call.Add(-100 * time.Millisecond), To: call, Began: began}, F: Span{From: callF.Add(-100 * time.Millisecond), To: callF, Began: began.Add(72 * time.Second)}}).ComparesOffsets()
	const p = "body.sql"
	for _, z := range zonedRows {
		t.Run(z.name, func(t *testing.T) {
			own := func(at time.Time) string { return at.In(z.loc).Format(z.iso) }
			poll := func(at time.Time) string {
				v := call.Add(-3 * time.Hour)
				if !at.Equal(call) {
					v = v.Add(10 * time.Minute)
				}
				return v.In(z.loc).Format(z.iso)
			}
			for _, m := range []struct {
				name   string
				client bool
				w      *Window
			}{{"keploy test, a call with a window", false, w}, {"a client test", true, w}} {
				learn := func(value func(at time.Time) string) Learned {
					dc := clientTestCacheIn(z.loc)
					if !m.client {
						dc = clockedDrifts()
						dc.zone = z.loc
					}
					shape := func(at time.Time) Shape {
						return Shape{Leaves: map[string]Leaf{p: {Path: p, Noise: p, Type: String, Value: value(at)}}}
					}
					return ClassifyGiven(dc, []string{p}, shape(callF), shape(call), call, nil, nil, nil, m.w, nil)[0]
				}
				refused := learn(poll)
				if refused.Kind != KindUnaccounted {
					t.Fatalf("%s: the time whole learned %s (%s); the row wants it refused", m.name, refused.Kind, refused.Why)
				}
				for order, text := range map[string]func(at time.Time) string{
					"the held instant first": func(at time.Time) string { return "ran_at >= '" + own(at) + "' AND due < '" + poll(at) + "'" },
					"the held instant last":  func(at time.Time) string { return "due < '" + poll(at) + "' AND ran_at >= '" + own(at) + "'" },
				} {
					ln := learn(text)
					if ln.Kind != KindUnaccounted || !m.client && (!strings.HasPrefix(ln.Why, "it holds an instant (") || !strings.HasSuffix(ln.Why, "which keploy refuses as it refuses that instant whole: "+refused.Why)) {
						t.Errorf("%s, %s: %s (%s); want it refused as the time whole is (%s)", m.name, order, ln.Kind, ln.Why, refused.Why)
					}
				}
			}
		})
	}
}

// A text more than one instant of which changed is refused, never left
// noise: two reads three hours back (a query's two bounds), two of the
// call's own reads (an INSERT's created_at and updated_at) and a bulk text
// with one own read per row, in every zone, in keploy test (a call with a
// window and one with none) and in keploy mock's client tests, under the
// rule a copy follows its source and without it. Where no instant of it is
// refused whole, the reason is that more than one changed; one refused
// whole refuses it for that instant's reason. A client test refuses one
// whose instants are no read the app made in the call's window (a poll
// three hours back) by its own reason for such an instant.
func TestATextMoreThanOneInstantOfWhichChangedIsRefused(t *testing.T) {
	call := time.Date(2026, 10, 31, 10, 52, 15, 438_000_000, time.UTC)
	began := call.Add(-time.Second)
	callF := call.Add(73*time.Second + 600*time.Millisecond)
	w := (&Window{Rec: Span{From: call.Add(-100 * time.Millisecond), To: call, Began: began}, F: Span{From: callF.Add(-100 * time.Millisecond), To: callF, Began: began.Add(72 * time.Second)}}).ComparesOffsets()
	const p = "body.sql"
	type classify = func(dc *DriftCache, drift []string, live, rec Shape, at time.Time, earlier []Earlier, raw []Raw, resp []Leaf, w *Window, given GivenEarlier) []Learned
	kt := func(z *time.Location) *DriftCache { dc := clockedDrifts(); dc.zone = z; return dc }
	modes := []struct {
		name   string
		cache  func(*time.Location) *DriftCache
		window *Window
		cl     classify
	}{
		{"keploy test, a call with a window", kt, w, ClassifyGiven},
		{"keploy test, a call with no window", kt, nil, ClassifyGiven},
		{"keploy test under the rule a copy follows its source", kt, w, ClassifyFollowing},
		{"a client test", clientTestCacheIn, w, ClassifyGiven},
		{"a client test under the rule a copy follows its source", clientTestCacheIn, w, ClassifyFollowing},
	}
	for _, z := range zonedRows {
		t.Run(z.name, func(t *testing.T) {
			spell := func(at time.Time) string { return at.In(z.loc).Format(z.iso) }
			texts := []struct {
				name  string
				whole func(at time.Time) string
				text  func(at time.Time) string
			}{
				{"two reads three hours back", func(at time.Time) string { return spell(at.Add(-3 * time.Hour)) }, func(at time.Time) string {
					v := spell(at.Add(-3 * time.Hour))
					return "a >= '" + v + "' AND b < '" + v + "'"
				}},
				{"two own reads", spell, func(at time.Time) string {
					return "INSERT INTO jobs (created_at, updated_at) VALUES ('" + spell(at) + "', '" + spell(at) + "')"
				}},
				{"a bulk text with one own read per row", spell, func(at time.Time) string {
					var b strings.Builder
					b.WriteString("INSERT INTO events (id, at) VALUES ")
					for i := range 50 {
						if i > 0 {
							b.WriteString(", ")
						}
						fmt.Fprintf(&b, "(%d, '%s')", i, spell(at))
					}
					return b.String()
				}},
			}
			for _, m := range modes {
				learn := func(value func(at time.Time) string) Learned {
					shape := func(at time.Time) Shape {
						return Shape{Leaves: map[string]Leaf{p: {Path: p, Noise: p, Type: String, Value: value(at)}}}
					}
					return m.cl(m.cache(z.loc), []string{p}, shape(callF), shape(call), call, nil, nil, nil, m.window, nil)[0]
				}
				for _, tx := range texts {
					whole, in := learn(tx.whole), learn(tx.text)
					switch {
					case whole.Kind == KindUnaccounted:
						if why := asWhole(in, whole); why != "" {
							t.Errorf("%s, %s: %s; want it refused as the instant whole is", m.name, tx.name, why)
						}
					case in.Kind == KindUnaccounted && m.cache(z.loc).clientTests && strings.HasPrefix(in.Why, "no clock read by the app matched it (an instant, "):
						// A client test refuses it by its own reason, where
						// its instants are no read the app made in the
						// call's window, which keeps it bindable as the
						// test's own value where it is first sent.
					case in.Kind != KindUnaccounted || in.Why != moreInstantsWhy:
						t.Errorf("%s, %s: %s (%s); the instant whole %s; want it refused as more than one changed instant in one text", m.name, tx.name, in.Kind, in.Why, whole.Kind)
					}
				}
			}
		})
	}
}

// What the one rule for an instant wherever it stands reads as an instant
// inside a text is what it reads as an instant whole, with no evidence the
// clock moved it (a stated limit, which the learning's re-look of what an
// instant is owns): a changed number of an epoch's shape (an order id of
// ten digits, a snowflake id of nineteen), a date, or a time of day. So a
// fresh id inside a text is refused in a call with a window, as the id
// whole is, and in a call with no window it is learned as a read of the
// clock at its word, which a replay sending the id again refuses: a false
// red, never a false green. This row pins today's outcome.
func TestAChangedNumberDateOrTimeOfDayInATextIsReadAsAnInstant(t *testing.T) {
	call := time.Date(2026, 10, 31, 10, 52, 15, 438_000_000, time.UTC)
	began := call.Add(-time.Second)
	callF := call.Add(73*time.Second + 600*time.Millisecond)
	w := (&Window{Rec: Span{From: call.Add(-100 * time.Millisecond), To: call, Began: began}, F: Span{From: callF.Add(-100 * time.Millisecond), To: callF, Began: began.Add(72 * time.Second)}}).ComparesOffsets()
	const p = "body.v"
	learn := func(rec, live string, w *Window) Learned {
		shape := func(v string) Shape {
			return Shape{Leaves: map[string]Leaf{p: {Path: p, Noise: p, Type: String, Value: v}}}
		}
		dc := clockedDrifts()
		dc.zone = time.UTC
		return ClassifyGiven(dc, []string{p}, shape(live), shape(rec), call, nil, nil, nil, w, nil)[0]
	}
	for _, c := range []struct {
		name, rec, live, before, after string
	}{
		{"a snowflake id in a path", "/api/orders/1852345678901234567", "/api/orders/1852399999901234567", "/api/orders/", ""},
		{"an order id of ten digits", "Order #1234567890 placed", "Order #1987654321 placed", "Order #", " placed"},
		{"a date", "order 2026-11-05 ok", "order 2026-11-09 ok", "order ", " ok"},
	} {
		if ln := learn(c.rec, c.live, w); ln.Kind != KindUnaccounted || !strings.HasPrefix(ln.Why, "it holds an instant (") {
			t.Errorf("%s, a call with a window: %s (%s); want it refused as the instant whole is", c.name, ln.Kind, ln.Why)
		}
	}
	ln := learn("Order #1234567890 placed", "Order #1987654321 placed", nil)
	if !readAtWord(ln, "Order #", " placed") {
		t.Fatalf("an order id of ten digits, a call with no window: %s %+v (%s); want a read of the clock at its word", ln.Kind, ln.Read, ln.Why)
	}
	replay := call.Add(24 * time.Hour)
	s := Span{From: replay.Add(-100 * time.Millisecond), To: replay, Zone: time.UTC, Own: true, Began: replay.Add(-time.Second)}
	if ok, _ := CheckClockRead(p, *ln.Read, Leaf{Path: p, Noise: p, Type: String, Value: "Order #1234567890 placed"}, s, true); ok {
		t.Errorf("a replay sending the recorded id again passed; want it refused")
	}
	for _, win := range []*Window{w, nil} {
		if ln := learn("slot 10:30 booked", "slot 11:45 booked", win); !readAtWord(ln, "slot ", " booked") {
			t.Errorf("a time of day, a window %v: %s %+v (%s); want a read of the clock at its word", win != nil, ln.Kind, ln.Read, ln.Why)
		}
	}
}

// A word read in its text as it stands is checked there at a replay: the
// same text percent-encoded, which the word's sides as recorded are not,
// is refused, as is a word in a form this keploy does not read. A word
// read in its text's decoded form passes the same text however it is
// encoded.
func TestAWordIsCheckedInTheFormItWasLearnedIn(t *testing.T) {
	call := time.Date(2026, 10, 31, 10, 52, 15, 438_000_000, time.UTC)
	callF := call.Add(73*time.Second + 600*time.Millisecond)
	const p = "body.note"
	spell := func(at time.Time) string { return at.In(ist).Format(time.RFC3339) }
	plain := func(at time.Time) string { return "jobs due before " + spell(at) + ", page 2" }
	encoded := func(at time.Time) string { return strings.ReplaceAll(url.QueryEscape(plain(at)), "+", "%20") }
	learn := func(value func(time.Time) string) models.ClockOffset {
		shape := func(at time.Time) Shape {
			return Shape{Leaves: map[string]Leaf{p: {Path: p, Noise: p, Type: String, Value: value(at)}}}
		}
		dc := clockedDrifts()
		dc.zone = ist
		ln := ClassifyGiven(dc, []string{p}, shape(callF), shape(call), call, nil, nil, nil, nil, nil)[0]
		o, ok := ln.Clock()
		if !plainRead(ln) || !ok || o.Word.IsZero() {
			t.Fatalf("%q: %s %+v (%s); want a read of the clock at its word", ln.Recorded.Value, ln.Kind, ln.Read, ln.Why)
		}
		return o
	}
	replay := call.Add(24 * time.Hour)
	s := Span{From: replay.Add(-100 * time.Millisecond), To: replay, Zone: ist, Own: true, Began: replay.Add(-time.Second)}
	check := func(o models.ClockOffset, v string) bool {
		ok, _ := CheckClockRead(p, o, Leaf{Path: p, Noise: p, Type: String, Value: v}, s, true)
		return ok
	}
	raw := learn(plain)
	if raw.Word.Form != "" || !check(raw, plain(replay)) || check(raw, encoded(replay)) {
		t.Errorf("a word read as the text stands (%+v): want it to pass the plain text and refuse it percent-encoded", raw.Word)
	}
	dec := learn(encoded)
	if dec.Word.Form != models.ClockWordDecoded || !check(dec, encoded(replay)) || !check(dec, plain(replay)) {
		t.Errorf("a word read in the decoded text (%+v): want it to pass the text in either form", dec.Word)
	}
	other := raw
	other.Word.Form = "rot13"
	if check(other, plain(replay)) {
		t.Errorf("a word in a form this keploy does not read passed; want it refused")
	}
}
