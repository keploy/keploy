package learn

import (
	"fmt"
	"strings"
	"testing"
	"time"
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
