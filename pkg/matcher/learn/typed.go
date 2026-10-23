package learn

import (
	"strings"
	"time"
)

// AN INSTANT ITS PROTOCOL TYPES.
//
// A field its protocol's decoder types as one instant (a BSON UTC datetime,
// type 0x09, or timestamp, 0x11; a typed timestamp column) is one value on
// the wire: the words of its date and time exist only in keploy's text of
// it. The decoder hands it to the learn step as an Instant leaf
// (InstantLeaf), and ClassifyIn compares it as one instant
// (classifyInstant), never word by word: a minute of it is no word an
// unrelated recording can hold (a Mongo reply's requestId of 40, a version
// in an SDK's User-Agent, SQS's API version 2012-11-05). Only a decoder's
// type makes a leaf an instant. A string that reads as a date stays a
// String, read word by word: no text's shape decides anything. Where the
// word-by-word reading meets an instant leaf (an earlier exchange's, the
// call's own answer's), it reads its moment, never its words (appearedIn,
// unaccounted, Echoes, echoedPart).
//
// Read whole, a drifted instant gets a value's checks, each of the instant:
//
//   - a variable where a leaf of an exchange F served, or a part of the
//     test case's own request keploy test sent with a template value,
//     drifted with it: one that was, as recorded, the instant it was
//     recorded with (in any layout and form), or that held the moment it was
//     recorded with and the moment F saw, both ways (rule 1), whatever its
//     provenance;
//   - the echo, before any provenance excuses it (instants.go): where an
//     earlier place holds the moment F saw (an instant of it in any layout,
//     cut to the instant's own precision, or its time of day), by the rule
//     an earlier recording holds the moment it was recorded with, or alone,
//     it may be a pick from what the application was given, by its clock; it
//     is a variable of the place that holds the moment it was recorded with,
//     or, held nowhere, refused. An instant its protocol types is never read
//     as a value built on a coarser instant (a date with a time of its own):
//     matched at its own precision only;
//   - a variable where a payload served or recorded earlier holds the moment
//     it was recorded with, exactly, in any layout, a protocol-phase answer's
//     own clock reads and an HTTP answer's stamp blanked (rules 2 and 3):
//     an instant equal to an earlier one is followed, whatever its
//     provenance;
//   - noise only where its provenance shows a read of the application's
//     clock (InstantClockRead): it moved by FullRoundLead, or lies within its
//     call's window in both runs. An echo of it in the call's own recorded
//     answer is then answered there (KindEcho);
//   - refused otherwise (KindUnaccounted), saying why. Read whole, an instant
//     derived from what the application was given (an hour after a moment it
//     was served) shares no word with its source, so only its provenance
//     tells a read of the clock from it: a false red that fails closed,
//     never noise.

// InstantLeaf is the leaf of a field its protocol's decoder types as one
// instant: at in UTC, truncated to res (the decoder's resolution: a BSON
// datetime's millisecond, a BSON timestamp's second, a date column's day),
// written in the form of the coarsest of instantForms that res is no finer
// than: RFC 3339 with the fraction digits res needs (none, 3, 6 or 9), its
// time to the minute, or its date (ISO 8601). Its type is Instant whatever
// its year: of a year before 0 or after 9999 the form writes a text of
// another length (a sign, a fifth digit), which InstantOf does not read, and
// the learn step refuses it.
func InstantLeaf(path, noise string, at time.Time, res time.Duration) Leaf {
	f := instantFormOf(res)
	t := at.UTC()
	if res > 0 {
		t = t.Truncate(res)
	}
	return Leaf{Path: path, Noise: noise, Type: Instant, Value: t.Format(f.layout)}
}

// InstantOf reads l, a leaf InstantLeaf wrote: its instant, and the
// resolution its form writes (instantForms). ok is false for a leaf of any
// other type, a String that reads as a date among them (only a decoder types
// a leaf an instant), and for a value no form writes.
func InstantOf(l Leaf) (time.Time, time.Duration, bool) {
	if l.Type != Instant {
		return time.Time{}, 0, false
	}
	// Each form writes its own length (a year of four digits), and Go's
	// parser would take a fraction its layout does not write: the length
	// picks the form.
	for _, f := range instantForms {
		if len(f.layout) != len(l.Value) {
			continue
		}
		t, err := time.ParseInLocation(f.layout, l.Value, time.UTC)
		if err != nil {
			return time.Time{}, 0, false
		}
		return t, f.res, true
	}
	return time.Time{}, 0, false
}

// InstantClockRead reports whether rec and live, an Instant leaf as
// recorded and as record's full round saw it, are a read of each run's
// clock by their provenance (instantClock). false for a leaf InstantOf does
// not read.
func InstantClockRead(rec, live Leaf, w *Window) bool {
	rt, rres, ok := InstantOf(rec)
	if !ok {
		return false
	}
	lt, lres, ok := InstantOf(live)
	if !ok {
		return false
	}
	return instantClock(rt, lt, rres, lres, w)
}

// instantClock reports whether rt and lt, an instant as recorded (at
// resolution rres) and as F saw it (lres), are a read of each run's clock,
// as clockIn reads an instant of a text: lt is rt moved by FullRoundLead
// (leadMoved), or each lies within its run's span of its call's window w
// (nil: none), each at its own resolution. An instant before 2000 or after
// 2100 is no read of a recording's clock.
func instantClock(rt, lt time.Time, rres, lres time.Duration, w *Window) bool {
	if !inClockRange(rt) || !inClockRange(lt) {
		return false
	}
	return w != nil && w.Rec.holds(rt, rres) && w.F.holds(lt, lres) || leadMoved(rt, lt, rres, lres, false)
}

// instantForms are the forms InstantLeaf writes an instant in, coarsest
// first, each with the resolution it writes: its date, its time to the
// minute, and RFC 3339 to the second, the millisecond, the microsecond and
// the nanosecond, in UTC.
var instantForms = []struct {
	layout string
	res    time.Duration
}{
	{"2006-01-02", 24 * time.Hour},
	{"2006-01-02T15:04Z", time.Minute},
	{"2006-01-02T15:04:05Z", time.Second},
	{"2006-01-02T15:04:05.000Z", time.Millisecond},
	{"2006-01-02T15:04:05.000000Z", time.Microsecond},
	{"2006-01-02T15:04:05.000000000Z", time.Nanosecond},
}

// instantFormOf is the coarsest form that writes an instant of resolution
// res (the nanosecond's for none).
func instantFormOf(res time.Duration) struct {
	layout string
	res    time.Duration
} {
	for _, f := range instantForms {
		if res >= f.res {
			return f
		}
	}
	return instantForms[len(instantForms)-1]
}

// instantMoment is t, of resolution res, as the places' moments are compared
// (heldMoments): the instant, in UTC (an instant its protocol types names
// its moment, as an epoch does), the time of day it writes there, and its
// resolution, the precision a place's instant is cut to where it is matched
// against it (covers). It is typed: a place holds it only as an instant cut
// to its own precision, never as a coarser instant it is finer than (the
// founder's rule for an instant its protocol types: equal to an earlier
// instant at its own precision).
func instantMoment(t time.Time, res time.Duration) moment {
	t = t.UTC()
	return moment{key: instantKey{at: t.UnixNano()}, wall: clockOfDay(t), res: res, zoned: true, typed: true}
}

// The reasons a typed instant is refused for (Learned.Why), read after
// "<field> changed, and ".
const (
	instantNotClock = "nothing shows it is a read of the application's clock: its protocol types it as one instant, which keploy compares whole, and it did not move by the lead that record's full round runs ahead of the recording, nor does a window of its call show a read. Read whole, an instant derived from a value the application was given shares no word with that value, so keploy learns an instant as noise only where it is a read of the clock. If the field is a time the application reads from its own clock, declare it noise in keploy.yml, and keploy stops comparing it"
)

// instantUnread is why a drifted leaf its protocol types as one instant
// (r as recorded, l as F saw it) that InstantOf cannot read on a side is
// refused: what keploy found there, and how to correct it.
func instantUnread(r, l Leaf) string {
	var found string
	switch {
	case r.Type != Instant:
		found = "its protocol types it as one instant as record's full round saw it, but not as it was recorded"
	case l.Type != Instant:
		found = "its protocol types it as one instant as it was recorded, but not as record's full round saw it"
	default:
		found = "its protocol types it as one instant, and keploy cannot read that instant as it was recorded or as record's full round saw it (a year before 0 or after 9999, or a text no decoder wrote as one)"
	}
	return found + ", so keploy cannot compare it whole or tell a read of the clock from a value that flowed. If the field is a time the application reads from its own clock, declare it noise in keploy.yml, and keploy stops comparing it"
}

// classifyInstant is ClassifyIn's reading of a drifted leaf its protocol
// types as one instant (r as recorded, l as F saw it: Instant leaves), in its
// call's window w, as the header of this file says: the instant whole, never
// its words. unread says why the test case's own request could not be read
// (requestUnread; "" when it was, or no test case's request applies): then
// the echo could not read it, and the instant is never noise or an echo.
func (dc *DriftCache) classifyInstant(r, l Leaf, at time.Time, served []Earlier, raw []Raw, resp []Leaf, w *Window, unread string) Learned {
	ln := Learned{Kind: KindNoise, Recorded: r, Live: l}
	rt, rres, okR := InstantOf(r)
	lt, lres, okL := InstantOf(l)
	if !okR || !okL {
		ln.Kind, ln.Why = KindUnaccounted, instantUnread(r, l)
		return ln
	}
	rec, live := instantMoment(rt, rres), instantMoment(lt, lres)
	if from, ok := dc.instantServedFrom(r, rec, live, served); ok {
		ln.Kind, ln.From = KindVariable, from
		return ln
	}
	// The echo, before any provenance excuses the instant.
	if echoed := echoedPairs([]driftedInstant{{rec: rec, live: live}}, dc.earlierMoments(at, served, raw), dc.recordedMoments(at, served, raw)); len(echoed) > 0 {
		if from, ok := dc.instantEchoFrom(r, echoed, at, served, raw); ok {
			ln.Kind, ln.From, ln.Why = KindVariable, from, instantEvidence(from)
			return ln
		}
		where := dc.heldAt(echoed, at, served, raw)
		if where == "" {
			where = "earlier in the test"
		}
		ln.Kind, ln.Why = KindUnaccounted, pickUnaccounted(where)
		return ln
	}
	if from, ok := dc.instantRawFrom(r, rec, at, raw); ok {
		ln.Kind, ln.From, ln.Why = KindVariable, from, instantEvidence(from)
		return ln
	}
	// The test case's own request, as recorded, holding the moment it was
	// recorded with, before any provenance excuses it: a pick from the
	// request by the clock whose pick in F the request does not list.
	if where, asRecorded := dc.requestHolds([]moment{rec}, []moment{live}, served); where != "" {
		ln.Kind, ln.Why = KindUnaccounted, requestHoldsWhy(where, asRecorded)
		return ln
	}
	if !instantClock(rt, lt, rres, lres, w) {
		ln.Kind, ln.Why = KindUnaccounted, instantNotClock
		return ln
	}
	// Noise, or an echo, only where the echo could read the test case's own
	// request, where the moment F saw may stand.
	if unread != "" {
		ln.Kind, ln.Why = KindUnaccounted, unreadRequestWhy(unread)
		return ln
	}
	if echo := dc.instantEchoes(rec, resp); len(echo) > 0 {
		ln.Kind, ln.Echo = KindEcho, echo
	}
	return ln
}

// instantServedFrom is the leaf of an exchange F served earlier that the
// drifted instant (r; rec and live its moments) came from, ranked as
// appearedIn ranks a value's places (placeRank): a leaf F saw drift that
// was, as recorded, that instant (its text, or, in any form, one instant and
// nothing else, of the moment it was recorded with: an equal instant is
// followed whatever its provenance, as in a payload), or that held the
// moment it was recorded with as recorded and the moment F saw as F saw it,
// neither on its other side (it drifted with it, rule 1). Whole, of the
// place, is its recorded text's being r's: where the value can be bound. The
// test case's own request is such a place too, where keploy test sent a part
// of it with template values (Earlier.Given): nothing binds there.
func (dc *DriftCache) instantServedFrom(r Leaf, rec, live moment, served []Earlier) (Place, bool) {
	var best Place
	rank := 7
	for _, e := range served {
		for _, s := range e.Seen {
			if s.Live == s.Leaf.Value {
				continue
			}
			whole := s.Leaf.Value == r.Value
			if !whole && !dc.isInstant(s.Leaf.Value, rec) && !dc.instantCoDrifted(s.Leaf.Value, s.Live, rec, live) {
				continue
			}
			if pr := placeRank(e, s, whole); pr < rank {
				best, rank = seenPlace(e, s, whole), pr
			}
		}
	}
	return best, rank < 7
}

// instantSource reports whether v, a drifted value read word by word, came
// from s, a leaf F saw drift whose protocol types it as one instant (Instant):
// v held the moment s was recorded with as recorded and the moment F saw
// there as F saw it, in any form, neither on its other side.
func (dc *DriftCache) instantSource(s Seen, v *valueDrift) bool {
	rt, _, ok := InstantOf(s.Leaf)
	if !ok {
		return false
	}
	lt, _, ok := InstantOf(Leaf{Type: Instant, Value: s.Live})
	if !ok {
		return false
	}
	return dc.instantCoDrifted(v.rec, v.live, instantMoment(rt, 0), instantMoment(lt, 0))
}

// isInstant reports whether text, in one of its declared forms (textForms),
// is one instant and nothing else, of moment m to the nanosecond.
func (dc *DriftCache) isInstant(text string, m moment) bool {
	for _, f := range textForms(text) {
		f = strings.TrimSpace(f)
		sp := dc.spansOf(f)
		if len(sp) == 1 && sp[0].keyed && !sp[0].key.day && sp[0].start == 0 && sp[0].end == len(f) && sp[0].key == m.key {
			return true
		}
	}
	return false
}

// instantCoDrifted reports whether a leaf F saw drift (rec as recorded, live
// as F saw it) held moment rm as recorded and lm as F saw it, in any form,
// and neither on its other side.
func (dc *DriftCache) instantCoDrifted(rec, live string, rm, lm moment) bool {
	in := func(text string, m moment) bool {
		return dc.holdsExact(text, m) || dc.holdsExact(dc.rawExtra(text), m)
	}
	return in(rec, rm) && in(live, lm) && !in(live, rm) && !in(rec, lm)
}

// instantEchoFrom is the earlier place holding the moment the drifted
// instant r was recorded with, as an instant or its time of day, by the rule
// the echo found it a copy by (echoed, holdsRecorded, as appearedIn's echo
// reads the places): a leaf of an exchange F served earlier, ranked by
// placeRank, then a payload served or recorded before at, its field where
// one holds it.
func (dc *DriftCache) instantEchoFrom(r Leaf, echoed []echoMatch, at time.Time, served []Earlier, raw []Raw) (Place, bool) {
	var best Place
	rank := 7
	for _, e := range served {
		for _, s := range e.Seen {
			if !dc.holdsRecorded(s.Leaf.Value, echoed) && !dc.holdsRecorded(dc.rawExtra(s.Leaf.Value), echoed) {
				continue
			}
			whole := s.Leaf.Value == r.Value
			if pr := placeRank(e, s, whole); pr < rank {
				best, rank = seenPlace(e, s, whole), pr
				best.Echo = true
			}
		}
	}
	if rank < 7 {
		return best, true
	}
	for _, x := range raw {
		if !x.served && !x.at.Before(at) {
			continue
		}
		for _, f := range x.fields {
			if !f.key && dc.holdsRecorded(x.text[f.start:f.end], echoed) {
				return Place{Mock: x.mock, Field: f.describe(), Echo: true}, true
			}
		}
		if dc.holdsRecorded(x.text, echoed) || dc.holdsRecorded(dc.rawExtra(x.text), echoed) {
			return Place{Mock: x.mock, Echo: true}, true
		}
	}
	return Place{}, false
}

// instantRawFrom is the first payload served earlier in the test or recorded
// before at that holds rec, the moment the drifted instant r was recorded
// with, as an instant of its own in any layout, exactly (not its time of
// day, nor a finer instant cut to it: those are the echo's, which F's moment
// gates), in any form, with a protocol-phase answer's own clock reads and an
// HTTP answer's stamp blanked (searched: rules 2 and 3), and the field that
// holds it.
func (dc *DriftCache) instantRawFrom(r Leaf, rec moment, at time.Time, raw []Raw) (Place, bool) {
	for _, x := range raw {
		if !x.served && !x.at.Before(at) {
			continue
		}
		texts := dc.searched(x, false)
		if !dc.holdsExact(texts[0], rec) && !dc.holdsExact(texts[1], rec) {
			continue
		}
		field := ""
		for _, f := range x.fields {
			if f.key || f.end > len(texts[0]) {
				continue
			}
			own := texts[0][f.start:f.end]
			if dc.holdsExact(own, rec) || dc.holdsExact(dc.rawExtra(own), rec) {
				field = f.describe()
				break
			}
		}
		return Place{Mock: x.mock, Field: field, Held: r.Value}, true
	}
	return Place{}, false
}

// instantEchoes are the leaves of the call's recorded answer (resp) that
// hold rec, the moment the drifted instant was recorded with, as an instant
// of their own, exactly, in any form: places keploy answers with the moment
// F saw.
func (dc *DriftCache) instantEchoes(rec moment, resp []Leaf) []Leaf {
	var out []Leaf
	for _, a := range resp {
		if a.Value != "" && (dc.holdsExact(a.Value, rec) || dc.holdsExact(dc.rawExtra(a.Value), rec)) {
			out = append(out, a)
		}
	}
	return out
}

// holdsExact reports whether text holds m as an instant of its own, in any
// layout (instantSpans), to the nanosecond: not its time of day.
func (dc *DriftCache) holdsExact(text string, m moment) bool {
	if text == "" {
		return false
	}
	for _, sp := range dc.spansOf(text) {
		if sp.keyed && !sp.key.day && sp.key == m.key {
			return true
		}
	}
	return false
}

// instantEvidence is why the drifted instant is a variable of from, a place
// the echo found or a payload holding the moment it was recorded with, as a
// reason names it: what was found and where, why no read of the clock
// accounts for it, and how to correct it. It names no value.
func instantEvidence(from Place) string {
	at := placeAt(from)
	if from.Echo {
		return echoFound(at) + "; " + echoProvenance + ", so keploy follows it as a value from there. " + correction(from.Mock)
	}
	return "the moment it was recorded with stands in " + at + ", as an instant of its own; an instant equal to one an earlier place holds is followed as a value from there, whether or not it is a read of the clock. " + correction(from.Mock)
}
