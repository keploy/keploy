package learn

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// readerZones are the zones the reader's zoned rows write an aware
// datetime in: one ahead of UTC by a half hour (India's), one behind it by
// one (Newfoundland's), UTC written "Z" and "+00:00", and one ahead by whole
// hours, each with how an ISO 8601 writer spells it.
var readerZones = []struct {
	name string
	loc  *time.Location
	iso  string
}{
	{"+05:30", ist, "2006-01-02T15:04:05Z07:00"},
	{"-03:30", time.FixedZone("NST", -(3*3600 + 1800)), "2006-01-02T15:04:05Z07:00"},
	{"Z", time.UTC, "2006-01-02T15:04:05Z07:00"},
	{"+00:00", time.UTC, "2006-01-02T15:04:05-07:00"},
	{"+01:00", time.FixedZone("CET", 3600), "2006-01-02T15:04:05Z07:00"},
}

// readerPGText is at as Postgres writes a timestamptz in text: a space
// before its time, and its zone's offset as "+05:30", or "+01" at a whole
// hour.
func readerPGText(at time.Time) string {
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

// An aware datetime is the instant it is, read whole in every zone and
// form: at the second, the millisecond, four fraction digits with no colon
// in its zone ("3050+0530"), in a sentence and in Postgres's text. Its zone's
// sign glued to the seconds ("13+05" of "…16:20:13+05:30") makes no base64
// token: every changed word is a word of the one instant, at its moment in
// each run, and no whole token the reader lists holds a '+' of it. What
// stands beside the instant is still read: a base64 run apart from it, or
// glued to its end, that changed is a token of its own.
func TestReaderZonedDatetimeIsOneInstant(t *testing.T) {
	rec := time.Date(2026, 10, 31, 10, 50, 13, 305_000_000, time.UTC)
	live := rec.Add(73*time.Second + 600*time.Millisecond)
	for _, z := range readerZones {
		for _, form := range []struct {
			what string
			text func(time.Time) string
		}{
			{"at the second", func(at time.Time) string { return at.In(z.loc).Format(z.iso) }},
			{"at the millisecond", func(at time.Time) string { return at.In(z.loc).Format(strings.Replace(z.iso, "05Z", "05.000Z", 1)) }},
			{"at four fraction digits, no colon in its zone", func(at time.Time) string { return at.In(z.loc).Format("2006-01-02T15:04:05.0000-0700") }},
			{"in a sentence", func(at time.Time) string { return "due " + at.In(z.loc).Format(z.iso) + " sharp" }},
			{"in Postgres's text", func(at time.Time) string { return readerPGText(at.In(z.loc)) }},
		} {
			r, l := form.text(rec), form.text(live)
			rd := readText(r, l)
			if len(rd.Words) == 0 || len(rd.Instants) != 1 {
				t.Errorf("%s %s (%q -> %q): %q, %d instants; want one instant", z.name, form.what, r, l, changedPairs(rd), len(rd.Instants))
				continue
			}
			iw := rd.Instants[0]
			if iw.Rec == nil || iw.Live == nil || iw.Rec.key.at != rec.Truncate(iw.Rec.res).UnixNano() || iw.Live.key.at != live.Truncate(iw.Live.res).UnixNano() {
				t.Errorf("%s %s: instant %+v; want each run's moment", z.name, form.what, iw)
			}
			for _, w := range rd.Words {
				if w.Instant != iw {
					t.Errorf("%s %s: word %q>%q outside the instant", z.name, form.what, w.Rec, w.Live)
				}
				for _, tok := range w.Tokens {
					if tok.Kind == tokenB64 && (strings.ContainsAny(tok.Rec, "+=") || strings.ContainsAny(tok.Live, "+=")) {
						t.Errorf("%s %s: base64 token %q>%q across the instant", z.name, form.what, tok.Rec, tok.Live)
					}
				}
			}
		}
		at := func(at time.Time) string { return at.In(z.loc).Format(z.iso) }
		for _, c := range []struct{ what, rec, live, run string }{
			{"a run apart from it", "sig=ab+cd at " + at(rec), "sig=ef+gh at " + at(live), "ab+cd"},
			{"a run apart from it, the instant the same", "sig=ab+cd at " + at(rec), "sig=ef+gh at " + at(rec), "ab+cd"},
			{"a run glued to its end", at(rec) + "+ab+c", at(live) + "+ef+g", "+ab+c"},
		} {
			rd := readText(c.rec, c.live)
			found := false
			for _, w := range rd.Words {
				for _, tok := range w.Tokens {
					found = found || tok.Kind == tokenB64 && tok.Rec == c.run
				}
			}
			if !found {
				t.Errorf("%s, %s (%q -> %q): no base64 token %q among %q", z.name, c.what, c.rec, c.live, c.run, changedPairs(rd))
			}
		}
	}
}
