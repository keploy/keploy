package learn

import (
	"sort"
	"strings"
	"time"
)

// AN INSTANT BUILT ON A SERVED ONE BY AN OFFSET.
//
// The echo (instants.go) takes a value for a copy of what the application was
// given where one rule names the moment F saw by what F served and the moment
// it was recorded with by what the recordings hold, each cut to the value's
// own precision: built on what it was given by no offset. An application
// also builds a value on what it was given by an offset (a message's
// visibility five minutes after the time it was sent, a due date a day after
// a served date). Where record's full round gave the application, at one
// place of what it was given, another instant than the recording did
// (another recording of the call F delivered there, a part of the test
// case's request keploy test sent with a template value), by about as much as
// the value moved, the value may be built on that place by an offset. It then
// moves as a read of the clock does wherever the place moved by about the
// lead, and at the minute, the hour or the day the lead takes a move of a
// unit or two for a read whatever moved it. The offset is read from the
// moments, never from a text's look:
//
//   - a moved place is an instant of one layout in one field of an answer
//     (models.MockSpec's answer fields; every field of a kind whose spec
//     holds its request and its answer in one), named by the word written
//     before it (instantLabel: its key, "created_at" of a JSON object), as
//     two recordings of one call (callOf) hold it, at two moments, at any
//     position in the field (a list F gave the application with another
//     number of elements puts the instant a value is built on at another
//     index): one the recording
//     gave the application before the call (recorded before it: hr), and
//     another that record's full round gave it in the test (served earlier
//     in it, the payload of an exchange served leaf by leaf too,
//     Earlier.Payload: hf), where one of the two is not what the other run
//     gave it too: hr one F did not give it, or hf one the recording did not
//     give it before the call, or both what both runs gave it, which F gave
//     in another order than the recording did (an application that takes
//     the first or the last of what it received takes the other one then)
//     or in an order keploy cannot tell (Raw.ServedAs: it fails closed).
//     Two recordings both runs gave the application in the order the
//     recording did are no move (F gave it nothing in their place), nor
//     are answers to two calls; or an instant of a part of the test case's request that keploy
//     test sent with another value than recorded (Earlier.Given). An answer's own clock stamp
//     and a protocol-phase answer's own clock reads are none (rules 2 and 3:
//     no value came from one), nor is what the application itself sent, or
//     an answer F served with the application's own value (a variable):
//     those moved with the application's own value;
//   - a changed instant of the value (its moment as recorded, rec, and as F
//     saw it, live, each at its own resolution) is built on a moved place
//     (hr where the recording gave it, hf where F did) by an offset where one
//     offset o puts hr+o in rec's unit and hf+o in live's, as a value cut to
//     its unit, truncated or rounded, after an offset is;
//   - a dated value is read against the places' dated instants; a time of
//     day against their times of day, in UTC or as each writes it in its own
//     zone, around the day.
//
// Such a value is refused, before any lead or window excuses it, as a pick
// the echo finds is, and the reason names the two places and the offset
// (offsetWhy): keploy follows no variable with an offset yet. A read of the
// clock that a moved place matches by chance is refused too: a false red,
// which fails closed and names the place, never noise.

// offsetPlace is an instant at a place the offset rule reads: its moment, the
// mock (or the test case) it stands in, and where in it ("<mock> at <field>");
// call, the call its recording answers (callOf; "" of the test case's
// request); recorded, whether the recording gave the application it before
// the call (its mock recorded before the call: a place's moment as the
// recording gave it, hr), and given, whether record's full round gave it in
// the test (served earlier in it: a place's moment as F gave it, hf); at,
// when its mock's request was recorded, and nth, where F served it in the
// test (Raw.ServedAs; 0 where it did not or that is not known): the two
// orders the two runs gave the application their recordings in.
type offsetPlace struct {
	m               moment
	mock            string
	where           string
	call            string
	recorded, given bool
	at              time.Time
	nth             int
}

// inRecordedOrder reports whether F gave the application the recordings of
// the places a and b, both of which both runs gave it, in the order the
// recording did: then F gave it nothing in the other's place, and a value
// that moved from one to the other moved by something else. One recording's
// two places are in its order. Two recordings F gave it in another order, or
// in one keploy cannot tell (either's not known, or both recorded at one
// instant), are not: the application may have taken the other one.
func inRecordedOrder(a, b offsetPlace) bool {
	if a.mock == b.mock && a.at.Equal(b.at) {
		return true
	}
	if a.nth == 0 || b.nth == 0 || a.nth == b.nth || a.at.Equal(b.at) {
		return false
	}
	return a.at.Before(b.at) == (a.nth < b.nth)
}

// offsetKey is a place of an answer, as two recordings of a kind each hold
// it: the field (rawField.path), which of its forms (0: as it stands, 1: its
// other declared forms, rawExtra), the instant's layout, and the word written
// before it (instantLabel). Every instant of that layout and label in the
// field stands at it, at any position.
type offsetKey struct {
	path         string
	form, layout int
	label        string
}

// instantLabel is the word written before the i-th of spans, the instants of
// t: the last word before it that is no part of another instant ("at" of
// "created_at": "2026-…", "jobs" of each instant of "jobs": ["2026-…",
// "2026-…"]), "" where none is. It names where in a field an instant stands
// whatever its position: no value is read.
func instantLabel(t string, spans []instant, i int) string {
	j := spans[i].start - 1
	for j >= 0 {
		in := false
		for _, sp := range spans {
			if sp.start <= j && j < sp.end {
				j, in = sp.start-1, true
				break
			}
		}
		if in {
			continue
		}
		if !isWord(t[j]) {
			j--
			continue
		}
		end := j + 1
		for j >= 0 && isWord(t[j]) {
			j--
		}
		return t[j+1 : end]
	}
	return ""
}

// offsetPlaces are the places a call's offset rule reads (offsetPlacesOf):
// the answers' instants by place (byPlace), each place's sorted by moment,
// and the test case's request's moved instants (given), each as recorded and
// as keploy test sent it.
type offsetPlaces struct {
	byPlace map[offsetKey][]offsetPlace
	given   [][2]offsetPlace
}

// offsetPlacesOf reads the places the offset rule reads for a call recorded
// at at (offsetPlaces): each payload served earlier in the test or recorded
// before at (raw), and each Earlier.Payload of an exchange served leaf by leaf
// (served), with stamps and protocol-phase answer clock reads blanked
// (searched), each place said recorded where its recording came before at
// and given where F served it in the test; and the parts of the test case's
// own request keploy test sent with another value than recorded.
func (dc *DriftCache) offsetPlacesOf(at time.Time, served []Earlier, raw []Raw) *offsetPlaces {
	op := &offsetPlaces{byPlace: map[offsetKey][]offsetPlace{}}
	// One recording read once, whether F gave it the application in the test
	// as any of its readings says: a payload a leaf-by-leaf exchange gives
	// and the same recording among raw are one.
	type payload struct {
		x     Raw
		given bool
		nth   int
	}
	var payloads []*payload
	byID := map[string]*payload{}
	add := func(x Raw, given bool) {
		id := x.mock + "\x00" + x.at.String()
		nth := 0
		if given {
			nth = x.nth
		}
		if p, ok := byID[id]; ok {
			p.given = p.given || given
			if p.nth == 0 {
				p.nth = nth
			}
			return
		}
		p := &payload{x: x, given: given, nth: nth}
		byID[id] = p
		payloads = append(payloads, p)
	}
	for _, x := range raw {
		if x.read && (x.served || x.at.Before(at)) {
			add(x, x.served)
		}
	}
	for _, e := range served {
		if e.Given {
			if e.Unread != "" {
				continue
			}
			for _, s := range e.Seen {
				if s.Live == s.Leaf.Value || s.Leaf.Type == Instant {
					continue
				}
				where := e.Mock + " at " + s.Leaf.Path
				for _, f := range [][2]string{{s.Leaf.Value, s.Live}, {dc.rawExtra(s.Leaf.Value), dc.rawExtra(s.Live)}} {
					for _, p := range driftedInstants(f[0], f[1]) {
						op.given = append(op.given, [2]offsetPlace{{m: p.rec, mock: e.Mock, where: where, recorded: true}, {m: p.live, mock: e.Mock, where: where, given: true}})
					}
				}
			}
			continue
		}
		if e.Payload.read {
			add(e.Payload, true)
		}
	}
	for _, p := range payloads {
		x := p.x
		recorded := x.at.Before(at)
		text := dc.searched(x, false)[0]
		answers := hasAnswerField(x.fields)
		for _, f := range x.fields {
			if f.key || f.end > len(text) || answers && !answerFields[fieldRoot(f.path)] {
				continue
			}
			own := text[f.start:f.end]
			where := x.mock + " at " + f.describe()
			for form, t := range []string{own, dc.rawExtra(own)} {
				spans := dc.spansOf(t)
				for si, sp := range spans {
					if !sp.keyed {
						continue
					}
					k := offsetKey{path: f.path, form: form, layout: sp.layout, label: instantLabel(t, spans, si)}
					op.byPlace[k] = append(op.byPlace[k], offsetPlace{m: sp.moment(), mock: x.mock, where: where, call: x.call, recorded: recorded, given: p.given, at: x.at, nth: p.nth})
				}
			}
		}
	}
	for k, list := range op.byPlace {
		if len(list) < 2 {
			delete(op.byPlace, k)
			continue
		}
		sort.SliceStable(list, func(i, j int) bool { return list[i].m.key.at < list[j].m.key.at })
	}
	return op
}

// fieldRoot is the spec's own field a raw field's path starts in
// ("Res" of "Res.body", "MongoResponses" of "MongoResponses[0].Message").
func fieldRoot(path string) string {
	if i := strings.IndexAny(path, ".["); i >= 0 {
		return path[:i]
	}
	return path
}

// hasAnswerField reports whether one of fields stands in one of the spec's
// answer fields (answerFields): a kind whose spec holds its request and its
// answer in one field has none, and all its fields are read.
func hasAnswerField(fields []rawField) bool {
	for _, f := range fields {
		if answerFields[fieldRoot(f.path)] {
			return true
		}
	}
	return false
}

// offsetMatch is a changed instant of a value (rec, live) built on a moved
// place (hr as the recording gave it, hf as record's full round did) by an
// offset of about off.
type offsetMatch struct {
	rec, live moment
	hr, hf    offsetPlace
	off       time.Duration
}

// offsetOf reports whether some offset o puts hr+o in rec's unit and hf+o in
// live's (each moment the start of its unit, of its resolution), all read on
// one line, in nanoseconds (of a time of day, around the day: mod, else 0):
// about how much, and ok. About how much is the whole number of the coarser
// of the two resolutions among the offsets that do, where one is (an
// application adds a whole offset: five minutes), else the middle of them,
// to that resolution.
func offsetOf(rec, live, hr, hf int64, rres, lres time.Duration, mod int64) (time.Duration, bool) {
	if mod > 0 {
		// The places' moments, around the day, nearest the value's.
		near := func(h, v int64) int64 {
			d := ((v-h)%mod + mod) % mod
			if d > mod/2 {
				d -= mod
			}
			return v - d
		}
		hr, hf = near(hr, rec), near(hf, live)
	}
	lo := max(rec-hr, live-hf)
	hi := min(rec+int64(rres)-hr, live+int64(lres)-hf)
	if lo >= hi {
		return 0, false
	}
	res := int64(max(rres, lres))
	q := lo / res
	if q*res < lo {
		q++
	}
	if q*res < hi {
		return time.Duration(q * res), true
	}
	return time.Duration((lo + hi) / 2).Round(time.Duration(res)), true
}

// offsetFrom is the first of pairs (a value's changed instants) built on a
// moved place by an offset (offsetMatch): the test case's request's moved
// parts first, then the answers' places, in the order the places are read,
// each place the recording gave the application (recorded) against each
// another recording of its call F gave it there (given), by moment, where
// not both are what both runs gave it in the order the recording did
// (inRecordedOrder). ok is false when none is.
func (op *offsetPlaces) offsetFrom(pairs []driftedInstant) (offsetMatch, bool) {
	if op == nil {
		return offsetMatch{}, false
	}
	keys := make([]offsetKey, 0, len(op.byPlace))
	for k := range op.byPlace {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		a, b := keys[i], keys[j]
		if a.path != b.path {
			return a.path < b.path
		}
		if a.form != b.form {
			return a.form < b.form
		}
		if a.layout != b.layout {
			return a.layout < b.layout
		}
		return a.label < b.label
	})
	for _, p := range pairs {
		for _, g := range op.given {
			if m, ok := offsetBetween(p, g[0], g[1]); ok {
				return m, true
			}
		}
		for _, k := range keys {
			list := op.byPlace[k]
			for i := range list {
				// list[i] as the recording gave it, list[j] as F did.
				if !list[i].recorded {
					continue
				}
				// A dated value: only a place F gave the application within
				// the value's move of hr, give or take a unit of the value
				// (offsetOf), can be one; list is sorted by moment.
				from, to := 0, len(list)
				if !p.rec.key.day {
					d := p.live.key.at - p.rec.key.at
					lo, hi := list[i].m.key.at+d-int64(p.rec.res), list[i].m.key.at+d+int64(p.live.res)
					from = sort.Search(len(list), func(j int) bool { return list[j].m.key.at > lo })
					to = sort.Search(len(list), func(j int) bool { return list[j].m.key.at >= hi })
				}
				for j := from; j < to; j++ {
					// hf given by F, an answer to hr's call, and not both
					// what both runs gave the application in the order the
					// recording did: two recordings F gave in the test and
					// the recording gave before the call, in one order, are
					// no place F gave in another's (and neither is one
					// recording's two instants).
					hr, hf := list[i], list[j]
					if !hf.given || hf.call != hr.call || hr.given && hf.recorded && inRecordedOrder(hr, hf) {
						continue
					}
					if m, ok := offsetBetween(p, list[i], list[j]); ok {
						return m, true
					}
				}
			}
		}
	}
	return offsetMatch{}, false
}

// offsetBetween reports whether the changed instant p is built on the place
// hr (as the recording gave it) and hf (as record's full round did) by an
// offset: two moments, read as p's are (offsetOf), a dated value against
// dated instants, a time of day against times of day (an instant's in UTC,
// or as it writes it in its own zone).
func offsetBetween(p driftedInstant, hr, hf offsetPlace) (offsetMatch, bool) {
	if hr.m.key == hf.m.key {
		return offsetMatch{}, false
	}
	m := offsetMatch{rec: p.rec, live: p.live, hr: hr, hf: hf}
	if !p.rec.key.day {
		if hr.m.key.day || hf.m.key.day {
			return offsetMatch{}, false
		}
		off, ok := offsetOf(p.rec.key.at, p.live.key.at, hr.m.key.at, hf.m.key.at, p.rec.res, p.live.res, 0)
		m.off = off
		return m, ok
	}
	// A time of day: the places' times of day, each read one way on both
	// sides.
	readings := func(h moment) []int64 {
		if h.key.day {
			return []int64{h.key.at}
		}
		return []int64{timeOfDay(h.key.at), h.wall}
	}
	rr, fr := readings(hr.m), readings(hf.m)
	for i := range rr {
		if i >= len(fr) {
			break
		}
		if off, ok := offsetOf(p.rec.key.at, p.live.key.at, rr[i], fr[i], p.rec.res, p.live.res, nsPerDay); ok {
			m.off = off
			return m, true
		}
	}
	return offsetMatch{}, false
}

// offsetWhy is why a drifted value that m names is refused (read after
// "<field> changed, and "): where its recorded moment and the moment F saw
// in it stand against a moved place, by about how much, why that shows no
// read of the clock, and how to correct it. It names no value.
func offsetWhy(m offsetMatch) string {
	rel := "at"
	off := m.off
	switch {
	case off > 0:
		rel = off.String() + " after"
	case off < 0:
		rel = (-off).String() + " before"
	}
	unit := unitName(max(m.rec.res, m.live.res))
	return "its moment, to the " + unit + ", stands " + rel + " the moment " + m.hr.where + " holds as the recording gave it, and record's full round saw it " + rel + " the moment " + m.hf.where + " holds, which record's full round gave the application in its place: a value built on what the application was given by an offset moves so, as a read of the clock does, so keploy cannot tell it is a read of the clock. If the field is a time the application reads from its own clock, declare it noise in keploy.yml, and keploy stops comparing it; otherwise it is built on " + m.hf.mock + "'s instant, which keploy cannot follow with an offset yet"
}

// unitName names a resolution as a reason reads it: "minute", "second",
// "millisecond", or the duration.
func unitName(res time.Duration) string {
	switch res {
	case 24 * time.Hour:
		return "day"
	case time.Hour:
		return "hour"
	case time.Minute:
		return "minute"
	case time.Second:
		return "second"
	case time.Millisecond:
		return "millisecond"
	case time.Microsecond:
		return "microsecond"
	case time.Nanosecond:
		return "nanosecond"
	}
	return res.String()
}

// offsetOfValue is offsetFrom of a drifted value's changed instants
// (valueDrift.instants), the places read only where it has one.
func offsetOfValue(v *valueDrift, places func() *offsetPlaces) (offsetMatch, bool) {
	if len(v.instants) == 0 {
		return offsetMatch{}, false
	}
	return places().offsetFrom(v.instants)
}
