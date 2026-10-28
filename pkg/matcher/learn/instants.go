package learn

import (
	"reflect"
	"sort"
	"strconv"
	"strings"
	"time"

	"go.keploy.io/server/v3/pkg/models"
)

// WHERE A DRIFTED INSTANT CAME FROM.
//
// The classifier reads a drifted value word by word, at every earlier place,
// in a payload and in the call's own answer, whatever the value looks like:
// the words of an instant's date and time are words like any other. A value
// is excused as a clock read by its provenance alone, never by its shape
// (units, HH:MM, digits, lengths):
//
//   - the app's own clock read: an instant that moved by F's lead as a read
//     made no later than its call moves (readNoLaterThan), or that lies, in
//     both runs, within its call's window (Window: from the call's previous
//     call to the call, by the stamps keploy took of both, and the clock F
//     set; PreviousCall). Its words are then no words of a value (clockAt),
//     and only the same read whole elsewhere is followed;
//   - a dependency's own clock read: rules 2 and 3 below, which say that a
//     place is no source of a value, never that a value is noise.
//
// Before any of these excuses a word of a value, the echo reads its instants
// (echoedIn): where an earlier place holds the moment F saw in the value (a
// payload served or recorded earlier, a leaf of an exchange F served, its
// recorded value or the one F saw, on either side, in every form, or the
// test case's own request, as recorded and as sent: Earlier.Given), the
// value may be a pick from what the application was given, by its clock (a
// timetable's next departure, the next of the slots the request lists), or a
// copy of it, which moves as a read of the clock does. It is then a variable
// of the place holding the moment it was recorded with, or, held nowhere,
// refused (unaccounted). What F gave the application at a place no earlier
// place of the call's own test is (another test's recording, or one recorded
// after the call, wherever F served it: echoedElsewhere) is read so too, the
// moment F served there, and the moment its recording holds only where it
// was recorded before the call; a value it finds there is refused, naming
// it, never a variable. A place holds a moment as an instant of it in any
// layout, at the value's own precision (an instant the place holds finer, cut
// to the value's resolution or to a coarser one the value is whole at,
// truncated or rounded: a stored second of a served millisecond), as the
// start of a value read from a text that is finer than it (the value cut to
// the place's instant's unit, an hour or a day, longer than F's lead: a
// served date with the application's own time of day), or as its time of
// day as written in its own zone or in UTC (cuts). An application writes a value by one rule, so the echo takes a
// value for a copy where one rule names the moment F saw in what F served
// and the moment it was recorded with in what the recordings hold (oneCut:
// a neighbour recording F delivered within one unit of the recorded one is
// one), or where what F served names F's moment alone (holdsApart). The
// moments are compared, never their texts.
//
// Nor before the offset rule reads them (offset.go): where F gave the
// application, at a place of what it was given, another instant than the
// recording did, moved as the value's instant moved, the value may be built
// on it by an offset (a message's sent time and five minutes), and is
// refused, naming the place and the offset.
//
// A value whose provenance shows neither stays checked: where an earlier
// place holds one of its words, it is refused (a variable that cannot be
// followed, or unaccounted), even when the word is a minute that a Date
// header holds by coincidence. That false red fails closed, naming the value
// and the recording, never noise.
//
// Two rules read instants (clockLayouts: RFC 1123, ISO 8601, compact, an
// epoch, a time of day, a time-ordered UUID's time) to compare moments across
// layouts; neither learns a value as noise because of what it looks like:
//
//  1. At an earlier leaf F saw drift, a value that is no clock read drifted
//     with that leaf also where the leaf held a moment of the value's that
//     changed both ways, in any declared layout (holdsMomentOf:
//     "1792667656470" and "2026-10-22T11:14:16.47Z" are one moment), or its
//     text whole, recorded and live. This only finds more: the leaf's words
//     co-drift with the value's as any value's do.
//
//  2. A protocol-phase recording's answer's own clock read is no place a
//     value came from (Raw.AnswerClock). Its provenance is the recording's:
//     a protocol-phase exchange (a driver's handshake, heartbeat or auth,
//     IsProtocolPhase) is the driver's, its answer consumed by the driver,
//     and an instant that answer holds within the span keploy stamped from
//     the recording's request to its answer, at the instant's own
//     resolution, that its request does not hold, is the dependency's read
//     of its clock as it answered (a Mongo isMaster's localTime). F serves
//     that answer from its recording: a value that differs between the
//     recording and F was not copied from it. Every search of a payload
//     (appearedIn, unaccounted) reads it with those instants blanked; the
//     echo (echoedInstants: the payloads hold the instant F saw too) reads
//     it as it stands. Two clocks that read one moment are no copy of each
//     other (a queue consumer's processed_at was read in the millisecond in
//     which the handshake on the connection the driver opened for its insert
//     was answered, and that reply's localTime was that millisecond, in 7
//     runs of 10). A data-plane answer's instants are
//     searched as any text is: an answer can read back a value the
//     application stored in that second. A dependency whose clock is off by
//     more than the call's span holds no clock read of its answer: its
//     instants are followed, a false red, never a false green.
//
//  3. An HTTP answer's Date header is its server's clock read as it
//     answered (AnswerStamp): HTTP defines it as the moment the origin
//     generated the message (RFC 9110 §6.6.1), and it is read so only
//     within the span keploy stamped the exchange in, at its resolution of a
//     second. Its provenance is the protocol's: F serves the answer from its
//     recording, so a value that differs between the recording and F was not
//     copied from it. Every search reads a payload with such a header
//     blanked (Raw's stamps), and a leaf that is one, served as recorded
//     (Seen.Stamp, stamped), is searched for nothing; the echo reads both as
//     they stand, so a value F's run copied whole from a Date header F served
//     is followed. Where the value drifted from a part of one stamp to the
//     same part of another stamp F served (stampsEchoed: a batch labelled by
//     its answer's minute, F serving another recording of the call), the
//     premise fails, and every stamp is searched as any text. A server
//     whose clock is off by more than the span writes no stamp: its Date
//     header is searched as any text is.
//
// A field its protocol's decoder types as one instant (an Instant leaf: a
// BSON datetime) is no text: it is compared whole, never word by word, and is
// noise only by its provenance (typed.go). What this header says of a
// value's words is of a value the decoder gives as text.
//
// The word-level learning build (train/feat/word-level-learn) rewrites
// ClassifyIn with instant slots: it keeps these rules.

// instantRes are clockLayouts' patterns in either case: a case-folded form of
// a text holds its instants folded. Each reads a text only in the windows of
// it that can hold a match (layoutRe).
var instantRes = func() []*layoutRe {
	out := make([]*layoutRe, len(clockLayouts))
	for i, lay := range clockLayouts {
		out[i] = newLayoutRe(lay)
	}
	return out
}()

// instantKey is the moment an instant names, compared as one unit: at, in
// UnixNano (of a time of day, the nanoseconds into its day, day). Two
// instants are one only with equal keys: an epoch second and that second's
// millisecond 473 are two moments; "…16.47Z" and "…16.470Z" one. The echo
// also holds a coarser instant where a finer one cut to its precision is it
// (covers): that second is a copy of the millisecond, not the other way.
type instantKey struct {
	at  int64
	day bool
}

// instant is one instant a text holds: [start, end) of it, the index of its
// layout in clockLayouts, what it is (key), the time of day its text writes
// (wall), the resolution its text gives it (res) and the zone it names
// (zone, zoned: moment). keyed is false for a time-ordered UUID's time
// groups, which name no moment a payload is searched for.
type instant struct {
	start, end, layout int
	key                instantKey
	wall               int64
	res                time.Duration
	zone               int64
	zoned              bool
	keyed              bool
}

// moment is sp as a payload is searched for it.
func (sp instant) moment() moment {
	return moment{key: sp.key, wall: sp.wall, res: sp.res, zone: sp.zone, zoned: sp.zoned}
}

// moment is an instant as a payload is searched for it (heldMoments): what
// it is (key), the time of day its text writes (wall, in nanoseconds into
// its day): of a zoned instant, its wall clock in its own zone
// ("2026-10-22T11:15+05:30" writes 11:15, at 05:45 UTC); of an instant in
// UTC or of no zone, its UTC time of day; of a time of day, itself), the
// resolution its text gives it (res: a millisecond of "1792667656473", a
// second of "…T11:14:16Z"), and the zone its text names: zoned where it
// names one or is an absolute instant (an epoch, a "Z", an HTTP-date), with
// its offset east of UTC in nanoseconds (zone: 05:30 of "+05:30", -10:00 of
// "-10:00"). An instant of no zone ("2026-10-22 16:44:16", a DATETIME) is
// read in UTC, and is not zoned: it may be a wall clock in any zone
// (inZone). A time of day is never zoned.
type moment struct {
	key   instantKey
	wall  int64
	res   time.Duration
	zone  int64
	zoned bool
	// typed: the moment of an instant its protocol types (instantMoment),
	// which a place holds only as an instant cut to its own precision
	// (covers). A moment read from a text is held the other way too: where
	// it, cut to the unit of an instant the place holds (an hour, a day), is
	// that instant (a value built on a served date and the application's
	// own time of day).
	typed bool
}

// clockOfDay is t's time of day as its text writes it, in its own location,
// in nanoseconds into its day.
func clockOfDay(t time.Time) int64 {
	return int64(t.Hour())*int64(time.Hour) + int64(t.Minute())*int64(time.Minute) + int64(t.Second())*int64(time.Second) + int64(t.Nanosecond())
}

// instantKeyOf reads s, a match of layout k, in either case: the moment, the
// time of day its text writes (moment.wall), its resolution, and ok, false
// when its layout does not read it (out of range, no date).
func instantKeyOf(k int, s string) (moment, time.Duration, bool) {
	lay := clockLayouts[k]
	up := strings.ToUpper(s)
	t, res, ok := lay.read(up)
	if !ok {
		return moment{}, 0, false
	}
	if lay.wraps {
		tod := clockOfDay(t)
		return moment{key: instantKey{at: tod, day: true}, wall: tod, res: res}, res, true
	}
	wall := clockOfDay(t)
	at := t.UnixNano()
	_, off := t.Zone()
	zone, zoned := int64(off)*int64(time.Second), namesZone(k, up)
	// An epoch with a fraction, to the nanosecond from its digits (its
	// layout reads it through a float).
	if i := strings.IndexByte(up, '.'); i == 10 && isEpochText(up) {
		frac := up[i+1:]
		if len(frac) > 9 {
			frac = frac[:9]
		}
		sec, err := strconv.ParseInt(up[:i], 10, 64)
		if err != nil {
			return moment{}, 0, false
		}
		ns, err := strconv.ParseInt(frac+strings.Repeat("0", 9-len(frac)), 10, 64)
		if err != nil {
			return moment{}, 0, false
		}
		at = sec*int64(time.Second) + ns
		wall = timeOfDay(at)
	}
	return moment{key: instantKey{at: at}, wall: wall, res: res, zone: zone, zoned: zoned}, res, true
}

// namesZone reports whether s, a match of layout k (in upper case), is an
// instant its text places on the time line by itself: an HTTP-date, an
// epoch, or a text that names its zone ("Z", "+05:30", "-0500", "UTC",
// "GMT"). A date or a date and time of no zone is a wall clock of a zone the
// text does not name.
func namesZone(k int, s string) bool {
	switch k {
	case 1:
		if strings.HasSuffix(s, "UTC") || strings.HasSuffix(s, "GMT") {
			return true
		}
		// After its date ("2006-01-02"), whose dashes name no zone.
		return len(s) > 10 && strings.ContainsAny(s[10:], "Z+-")
	case 2:
		return strings.HasSuffix(s, "Z")
	}
	return true
}

// isEpochText reports whether s is digits around one '.'.
func isEpochText(s string) bool {
	dot := false
	for i := 0; i < len(s); i++ {
		switch c := s[i]; {
		case c == '.' && !dot:
			dot = true
		case c < '0' || c > '9':
			return false
		}
	}
	return dot
}

// instantSpans are the instants s holds: each a match of a layout of
// clockLayouts, in either case, at ASCII word boundaries, that its layout
// reads as an instant, no two overlapping (an earlier layout's first, as
// clockIn takes them), in the order they stand. A time-ordered UUID's span is
// its time groups, unkeyed.
func instantSpans(s string) []instant {
	var out []instant
	var taken []bool
	for k, re := range instantRes {
		lay := clockLayouts[k]
		for _, m := range re.FindAllStringIndex(s, -1) {
			if m[0] > 0 && isWord(s[m[0]-1]) && isWord(s[m[0]]) || m[1] < len(s) && isWord(s[m[1]-1]) && isWord(s[m[1]]) {
				continue
			}
			if taken != nil {
				free := true
				for i := m[0]; i < m[1]; i++ {
					if taken[i] {
						free = false
						break
					}
				}
				if !free {
					continue
				}
			}
			sp := instant{start: m[0], end: m[1], layout: k}
			if lay.timeWord != nil {
				if _, _, ok := lay.read(s[m[0]:m[1]]); !ok {
					continue
				}
				sp.end = m[0] + uuidTimeEnd(s[m[0]:m[1]])
			} else {
				mo, res, ok := instantKeyOf(k, s[m[0]:m[1]])
				if !ok {
					continue
				}
				sp.key, sp.wall, sp.res, sp.zone, sp.zoned, sp.keyed = mo.key, mo.wall, res, mo.zone, mo.zoned, true
			}
			if taken == nil {
				taken = make([]bool, len(s))
			}
			// The whole match is taken, a UUID's other groups too: no
			// later layout reads an instant inside it.
			for i := m[0]; i < m[1]; i++ {
				taken[i] = true
			}
			out = append(out, sp)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].start < out[j].start })
	return out
}

// uuidTimeEnd is where the time groups of s, a time-ordered UUID, end
// (uuidTimeWord): after its second group (version 7) or its third (1).
func uuidTimeEnd(s string) int {
	end, group := 0, 0
	for i := 0; i <= len(s); i++ {
		if i < len(s) && s[i] != '-' {
			continue
		}
		if !uuidTimeWord(s, group) {
			break
		}
		end, group = i, group+1
	}
	return end
}

// blankSpans is s with each instant it holds that blank says to blank
// replaced by spaces.
func blankSpans(s string, blank func(instant) bool) string {
	spans := instantSpans(s)
	if len(spans) == 0 {
		return s
	}
	var b []byte
	for _, sp := range spans {
		if !blank(sp) {
			continue
		}
		if b == nil {
			b = []byte(s)
		}
		for i := sp.start; i < sp.end; i++ {
			b[i] = ' '
		}
	}
	if b == nil {
		return s
	}
	return string(b)
}

// driftedInstant is a keyed instant of a drifted value that changed: the
// moment it was recorded, and the moment F saw in its place, the k-th of its
// layout in the live value. Both sides are of one layout (driftedInstants),
// or both an instant its protocol types (classifyInstant), so both are
// dated instants or both times of day: a layout is one or the other
// (clockLayout.wraps).
type driftedInstant struct{ rec, live moment }

// driftedInstants are the keyed instants of a drifted value's recorded and
// live forms (instantSpans) that changed, the k-th of a layout in one against
// the k-th of it in the other, as clockIn pairs them; one the live form holds
// none of is no pair.
func driftedInstants(recorded, live string) []driftedInstant {
	rs := instantSpans(recorded)
	if len(rs) == 0 {
		return nil
	}
	byLayout := map[int][]moment{}
	for _, sp := range instantSpans(live) {
		if sp.keyed {
			byLayout[sp.layout] = append(byLayout[sp.layout], sp.moment())
		}
	}
	seen := map[int]int{}
	var out []driftedInstant
	for _, r := range rs {
		if !r.keyed {
			continue
		}
		n := seen[r.layout]
		seen[r.layout]++
		if l := byLayout[r.layout]; n < len(l) && l[n].key != r.key {
			out = append(out, driftedInstant{rec: r.moment(), live: l[n]})
		}
	}
	return out
}

// readInstants fills d's reading of its instants once its forms are read:
// the instants of its forms that changed (instants), and the moments its
// forms hold, recorded and live (recKeys, liveKeys; recMoments,
// liveMoments).
func (d *valueDrift) readInstants() {
	d.recKeys, d.liveKeys = map[instantKey]bool{}, map[instantKey]bool{}
	seen := map[driftedInstant]bool{}
	for _, f := range d.forms {
		for _, p := range driftedInstants(f[0], f[1]) {
			if !seen[p] {
				seen[p] = true
				d.instants = append(d.instants, p)
			}
		}
		for _, sp := range instantSpans(f[0]) {
			if sp.keyed {
				d.recKeys[sp.key] = true
				d.recMoments = appendMoment(d.recMoments, sp.moment())
			}
		}
		for _, sp := range instantSpans(f[1]) {
			if sp.keyed {
				d.liveKeys[sp.key] = true
				d.liveMoments = appendMoment(d.liveMoments, sp.moment())
			}
		}
	}
}

// appendMoment is ms with m, unless ms holds it.
func appendMoment(ms []moment, m moment) []moment {
	for _, x := range ms {
		if x == m {
			return ms
		}
	}
	return append(ms, m)
}

// lostMoments are the moments v was recorded with that F's value of it does
// not hold, in the order they stand.
func lostMoments(v *valueDrift) []moment {
	var out []moment
	for _, m := range v.recMoments {
		if !v.liveKeys[m.key] {
			out = append(out, m)
		}
	}
	return out
}

// holdsMomentOf reports whether e, what F saw drift at an earlier place, held
// a moment a changed instant of v names both ways (rule 1): v's
// recorded moment in e's recorded value and the moment F saw in v in e's
// live value, neither on e's other side.
func (e *valueDrift) holdsMomentOf(v *valueDrift) bool {
	for _, p := range v.instants {
		if e.recKeys[p.rec.key] && e.liveKeys[p.live.key] && !e.liveKeys[p.rec.key] && !e.recKeys[p.live.key] {
			return true
		}
	}
	return false
}

// echoMatch is a changed instant of a value that the echo took for a copy of
// what the application was given (echoedPairs): the moment it was recorded
// with (rec) and the moment F saw in it (live), and the rules (cutRule) one
// of which names both, live by what F served and rec by what the recordings
// hold; nil where an instant F served names F's moment alone, and any rule
// may then name the recorded one (and where one F served at a place recorded
// at or after the call names F's moment by a rule that does not name the
// recorded one: lateEchoes).
type echoMatch struct {
	rec, live moment
	rules     map[cutRule]bool
}

// echoedInstants are the changed instants of v that the echo takes for a
// copy of what the application was given (echoedPairs): held, every payload
// appearedIn searches, in every form, as it stands, and every value F saw or
// the recording holds at a leaf of an exchange F served earlier, on either
// side; recorded, what the recordings of those places hold. nil when none.
func echoedInstants(v *valueDrift, held, recorded *heldMoments) []echoMatch {
	return echoedPairs(v.instants, held, recorded)
}

// echoedPairs are the pairs (a changed instant as recorded and as F saw it)
// whose live moment an earlier place holds too: what an application that
// stores an instant it was sent sends, or that picks one from what it was
// served (a departure from a timetable, by its clock). The echo reads them
// before any lead or window excuses a word of the value: a pick from served
// data moves with the clock as a read of it does. A place holds an instant
// where it names that moment (cuts): an instant of the same moment in any
// layout, or a time of day that is the instant's own, as written in its own
// zone or in UTC (a timetable's "11:15" holds "2026-10-22 11:15" and
// "2026-10-22T11:15+05:30"), and the other way round, each at the drifted
// instant's own precision (a second of a served millisecond is held), or, of
// a value read from a text, the value at the unit of the place's instant,
// an hour or a day (a served date the value is built on). A pair is a copy where one
// rule names the moment F saw by what F served and the moment it was
// recorded with by what the recordings hold (oneCut), or where an instant F
// served names the moment F saw and not the one it was recorded with
// (holdsApart: F's alone is held). A wall clock of no zone is also read in
// each zone a zone may be in, where one rule names both moments as that
// zone's (oneCut): one side alone fitting some zone is a coincidence a read
// of the clock can meet.
func echoedPairs(pairs []driftedInstant, held, recorded *heldMoments) []echoMatch {
	var out []echoMatch
	for _, p := range pairs {
		if rules := oneCut(held, recorded, p.rec, p.live); rules != nil {
			out = append(out, echoMatch{rec: p.rec, live: p.live, rules: rules})
			continue
		}
		if held.holdsApart(p.live, p.rec) {
			out = append(out, echoMatch{rec: p.rec, live: p.live})
			continue
		}
		// A wall clock of no zone, in a zone neither it nor the place names
		// (a DATETIME an application writes in its own).
		if p.rec.zoned || p.live.zoned {
			continue
		}
		for _, z := range held.zonesOf(p.live) {
			r, l := p.rec.inZone(z), p.live.inZone(z)
			if rules := oneCut(held, recorded, r, l); rules != nil {
				out = append(out, echoMatch{rec: r, live: l, rules: rules})
				break
			}
		}
	}
	return out
}

// holdsRecorded reports whether text holds, as the echo's source, the
// recorded moment of one of ms: by one of its rules (any, where it has none),
// by which text does not name the moment F saw too (an instant both runs'
// values are built on is no source of what changed).
func (dc *DriftCache) holdsRecorded(text string, ms []echoMatch) bool {
	for _, m := range ms {
		if dc.namesApart(text, m.rec, m.live, m.rules) {
			return true
		}
	}
	return false
}

// holdsLive reports whether text holds the moment F saw of one of ms, by one
// of its rules (any, where it has none), by which it does not name the
// recorded moment too.
func (dc *DriftCache) holdsLive(text string, ms []echoMatch) bool {
	for _, m := range ms {
		if dc.namesApart(text, m.live, m.rec, m.rules) {
			return true
		}
	}
	return false
}

// namesApart reports whether an instant text holds names m by a rule of
// rules (any, nil) by which it does not name other (cuts).
func (dc *DriftCache) namesApart(text string, m, other moment, rules map[cutRule]bool) bool {
	if text == "" {
		return false
	}
	for _, sp := range dc.spansOf(text) {
		if sp.keyed && namesMomentApart(sp.moment(), m, other, rules) {
			return true
		}
	}
	return false
}

// namesMomentApart reports whether h, an instant a place holds, names m by a
// rule of rules (any, nil) by which it does not name other (cuts).
func namesMomentApart(h, m, other moment, rules map[cutRule]bool) bool {
	return cuts(h, m, func(c cutRule) bool {
		if rules != nil && !rules[c] {
			return false
		}
		return !cuts(h, other, func(d cutRule) bool { return d == c })
	})
}

// heldIn are the moments texts hold, each read once per test (spansOf).
func (dc *DriftCache) heldIn(held *heldMoments, texts ...string) {
	for _, text := range texts {
		if text != "" {
			held.add(dc.spansOf(text))
		}
	}
}

// heldMoments are the moments a place's instants name: each keyed instant
// as it is, once in each spelling (its zone, its wall clock, its
// resolution: list), which a drifted moment is matched against at its own
// precision, as an instant or as a time of day (covers).
type heldMoments struct {
	seen map[moment]bool
	list []moment
}

// add files spans' keyed instants.
func (h *heldMoments) add(spans []instant) {
	for _, sp := range spans {
		if !sp.keyed {
			continue
		}
		h.addMoment(sp.moment())
	}
}

// addMoment files m.
func (h *heldMoments) addMoment(m moment) {
	if h.seen == nil {
		h.seen = map[moment]bool{}
	}
	if !h.seen[m] {
		h.seen[m] = true
		h.list = append(h.list, m)
	}
}

// holds reports whether the place names m's moment: an instant of it, or,
// for an instant, a time of day that is its own (as it writes it in its
// zone, or in UTC), and, for a time of day, an instant whose own it is; each
// at m's own precision, the place's finer instants cut to it, or m, read
// from a text, at the resolution of a coarser instant of the place (covers).
func (h *heldMoments) holds(m moment) bool {
	for _, held := range h.list {
		if covers(held, m) {
			return true
		}
	}
	return false
}

// holdsApart reports whether the place names m's moment (holds) by an
// instant that does not also name other's: an instant that names both the
// moment a value was recorded with and the one F saw (one by truncation, the
// other by rounding) is, alone, a copy of neither, since an application cuts
// one instant by one rule (oneCut reads such an instant beside another).
func (h *heldMoments) holdsApart(m, other moment) bool {
	for _, held := range h.list {
		if covers(held, m) && !covers(held, other) {
			return true
		}
	}
	return false
}

// namesApart reports whether an instant of the place names m's moment by a
// rule that does not name other's (namesMomentApart): one that names both by
// one rule (a day both fall in) names neither apart, whatever another rule
// names (lateEchoes).
func (h *heldMoments) namesApart(m, other moment) bool {
	for _, held := range h.list {
		if namesMomentApart(held, m, other, nil) {
			return true
		}
	}
	return false
}

// precisionUnits are the precisions an instant may be cut to, finest first:
// each decimal fraction of a second, the second, the minute, the hour and
// the day.
var precisionUnits = []time.Duration{
	time.Nanosecond, 10 * time.Nanosecond, 100 * time.Nanosecond,
	time.Microsecond, 10 * time.Microsecond, 100 * time.Microsecond,
	time.Millisecond, 10 * time.Millisecond, 100 * time.Millisecond,
	time.Second, time.Minute, time.Hour, 24 * time.Hour,
}

// nsPerDay is a day in nanoseconds.
const nsPerDay = int64(24 * time.Hour)

// offset is how far ahead of UTC m's text writes it (the offset of the zone
// it names, as its text names it), in nanoseconds: 0 of a time of day, and
// of an instant in UTC or of no zone.
func (m moment) offset() int64 {
	if m.key.day {
		return 0
	}
	return m.zone
}

// zoneOffsets are the offsets a zone may have from UTC: each quarter of an
// hour from 12 hours behind it to 14 ahead.
var zoneOffsets = func() []int64 {
	var out []int64
	for o := -12 * time.Hour; o <= 14*time.Hour; o += 15 * time.Minute {
		if o != 0 {
			out = append(out, int64(o))
		}
	}
	return out
}()

// zonesOf are the zones a zone may be in (zoneOffsets) that read m, a moment
// of no zone, as a wall clock some instant of the place may name at m's
// precision: those within the coarsest unit m may be cut to of where m's
// wall clock, read in UTC, stands from each instant the place holds (of a
// time of day, around the day). A unit m is whole at, or, read in a zone a
// quarter hour off, the hour (of a wall clock whole at the minute) or the
// day (whole at the quarter hour); and, of a moment read from a text, the
// coarsest unit a held instant may stand for, where that is coarser (a value
// built on it: its resolution, a coarser one it is whole at, a day where it
// is whole at a quarter hour). Only these can make the place hold m
// (covers), and each is read once, in order.
func (h *heldMoments) zonesOf(m moment) []int64 {
	unit := int64(m.res)
	for _, d := range precisionUnits {
		if u := int64(d); u > unit && whole(m.key.at, 0, u) && (!m.key.day || u < nsPerDay) {
			unit = u
		}
	}
	switch {
	case whole(m.key.at, 0, int64(15*time.Minute)):
		unit = nsPerDay
	case whole(m.key.at, 0, int64(time.Minute)) && unit < int64(time.Hour):
		unit = int64(time.Hour)
	}
	near := map[int64]bool{}
	for _, held := range h.list {
		var ds []int64
		switch {
		case !held.key.day && !m.key.day:
			ds = []int64{m.key.at - held.key.at}
		default:
			// A time of day on either side: around the day.
			d := (timeOfDay(m.key.at) - timeOfDay(held.key.at)) % nsPerDay
			ds = []int64{d, d - nsPerDay, d + nsPerDay}
			if !held.key.day {
				w := (timeOfDay(m.key.at) - held.wall) % nsPerDay
				ds = append(ds, w, w-nsPerDay, w+nsPerDay)
			}
		}
		// A moment read from a text may be finer than the held instant it
		// is built on (covers): within the coarsest unit the held instant
		// may stand for of it too, its resolution or a coarser one it is
		// whole at, a day where it is whole at a quarter hour (some zone's
		// midnight).
		u := unit
		if !m.typed {
			hu := int64(held.res)
			for _, d := range precisionUnits {
				if x := int64(d); x > hu && whole(held.key.at, 0, x) && (!held.key.day || x < nsPerDay) {
					hu = x
				}
			}
			if whole(held.key.at, 0, int64(15*time.Minute)) {
				hu = nsPerDay
			}
			if hu > u {
				u = hu
			}
		}
		for _, d := range ds {
			for _, z := range zoneOffsets {
				if z >= d-u && z <= d+u {
					near[z] = true
				}
			}
		}
	}
	var out []int64
	for _, z := range zoneOffsets {
		if near[z] {
			out = append(out, z)
		}
	}
	return out
}

// inZone is m, a moment of no zone (a wall clock its text names no zone
// of), read as the wall clock of a zone z ahead of UTC: the instant it then
// is, zoned z; of a time of day, its time of day in UTC.
func (m moment) inZone(z int64) moment {
	if m.key.day {
		at := ((m.key.at-z)%nsPerDay + nsPerDay) % nsPerDay
		return moment{key: instantKey{at: at, day: true}, wall: m.wall, res: m.res, typed: m.typed}
	}
	return moment{key: instantKey{at: m.key.at - z}, wall: m.wall, res: m.res, zone: z, zoned: true, typed: m.typed}
}

// cut is at, in nanoseconds, cut to a whole u as a clock z ahead of UTC
// writes it: truncated, and rounded half up.
func cut(at, z, u int64) (trunc, round int64) {
	r := ((at+z)%u + u) % u
	trunc, round = at-r, at-r
	if 2*r >= u {
		round += u
	}
	return trunc, round
}

// whole reports whether at is a whole u as a clock z ahead of UTC writes it.
func whole(at, z, u int64) bool { return ((at+z)%u+u)%u == 0 }

// covers reports whether held, an instant a place holds, names m's moment
// (cuts): by one rule or another. Nothing is read from either's text but its
// moment.
func covers(held, m moment) bool {
	return held.key == m.key || cuts(held, m, func(cutRule) bool { return true })
}

// cutRule is one rule by which an instant a place holds names a value's
// moment (cuts): which of the two is cut (the place's instant to the value's
// precision, a coarser copy of it; or, finer, the value to the resolution of
// the place's instant, a value built on it), how (rounded, or truncated), to
// which unit, as a clock zone ahead of UTC writes it, and which readings of
// the two moments meet (read). An application writes a value by one rule:
// the echo takes a value for a copy of what it was given only where one rule
// names the moment it was recorded with in what the recordings hold and the
// moment F saw in what F served (oneCut).
type cutRule struct {
	finer bool
	round bool
	unit  int64
	zone  int64
	read  cutRead
}

// cutRead is which readings of the two moments a rule compares.
type cutRead uint8

const (
	// readInstants: both as instants, cut as a clock zone ahead of UTC
	// writes them.
	readInstants cutRead = iota
	// readWall: the one of no zone as the other's own wall clock, in the
	// other's zone (a date or a DATETIME of a zoned instant's local time).
	readWall
	// readDayUTC and readDayOwn: a time of day against an instant's time of
	// day, in UTC, or as the instant writes it in its zone.
	readDayUTC
	readDayOwn
	// readDays: two times of day.
	readDays
)

// cuts visits each rule (cutRule) by which held, an instant a place holds,
// names m, a value's moment, and reports whether visit accepted one:
//
//   - coarser: held truncated, or rounded, to a unit of precisionUnits m is
//     whole at is m (a stored second of a served millisecond, "…T11:14:16Z"
//     of "1792667656473"; the same second written as milliseconds,
//     "1792667656000"; a minute or a day of it; the instant itself, at the
//     finest unit). A finer m is no copy of a coarser held this way;
//   - finer, of a moment read from a text only (moment.typed): m truncated to
//     a unit held stands for (its resolution or coarser, whole at it) that
//     is longer than F's lead, which m is finer than, is held (a served date
//     with the application's own time of day: "2026-10-23T09:00:00Z" of
//     "2026-10-23").
//
// Two instants are cut as a clock in UTC, in held's zone or in m's zone
// writes them (a day is a zone's own); an m of no zone (read in UTC) is also
// held's own wall clock, cut (a date of a zoned instant's own day, a
// DATETIME of its local time), and a held of no zone m's. A wall clock of a
// zone neither names is the echo's to read (echoedPairs). Of a time of day
// only its time of day is cut, to less than a day, and an instant's time of
// day is the one it writes in its zone or in UTC.
func cuts(held, m moment, visit func(cutRule) bool) bool {
	finer := !m.typed
	// coarser visits each unit m may be cut to in zone z: every unit m is
	// whole at (below a day of a time of day), finer than its text's
	// resolution too. A formatter that drops a fraction's trailing zeros
	// writes one value at the resolution its digits happen to need
	// ("…T11:14:16Z", then "…T11:14:16.784Z"), so a text's resolution is no
	// bound on the unit its value was cut to; and a held instant cut to a
	// finer unit m is whole at names m only where it names m at m's own.
	coarser := func(at, z int64, day bool, each func(u int64) bool) bool {
		for _, d := range precisionUnits {
			u := int64(d)
			if day && u >= nsPerDay || !whole(at, z, u) {
				continue
			}
			if each(u) {
				return true
			}
		}
		return false
	}
	// finerUnits visits each unit held's instant hat may stand for in zone
	// z that the value's mat is finer than: held's resolution or coarser,
	// hat whole at it and mat not (below a day of a time of day), and longer
	// than F's lead. A read of the clock moves by the lead between record
	// and F, so at a unit within it (a second, a minute) every read lands in
	// another unit in F, and a value finer than an instant of such a unit
	// is built on it no more than a read of the clock falls in it; at a
	// longer one (an hour, a day) a read stays in one unit in both runs but
	// where it crosses the unit's edge.
	finerUnits := func(hat, mat, z int64, day bool, each func(u int64) bool) bool {
		if !finer {
			return false
		}
		for _, d := range precisionUnits {
			u := int64(d)
			if d < held.res || d <= FullRoundLead || day && u >= nsPerDay || !whole(hat, z, u) || whole(mat, z, u) {
				continue
			}
			if each(u) {
				return true
			}
		}
		return false
	}
	// try visits c truncated where trunc is want, and rounded where round
	// is.
	try := func(c cutRule, trunc, round, want int64) bool {
		if trunc == want && visit(c) {
			return true
		}
		c.round = true
		return round == want && visit(c)
	}
	ofDay := func(v, u int64) (int64, int64) {
		t, r := cut(v, 0, u)
		return (t%nsPerDay + nsPerDay) % nsPerDay, (r%nsPerDay + nsPerDay) % nsPerDay
	}
	switch {
	case !held.key.day && !m.key.day:
		hz, mz := held.offset(), m.offset()
		// UTC, held's zone and m's, each once.
		zones := []int64{0}
		for _, z := range []int64{hz, mz} {
			if z != 0 && (len(zones) == 1 || z != zones[1]) {
				zones = append(zones, z)
			}
		}
		for _, z := range zones {
			if coarser(m.key.at, z, false, func(u int64) bool {
				t, r := cut(held.key.at, z, u)
				return try(cutRule{unit: u, zone: z}, t, r, m.key.at)
			}) {
				return true
			}
			if finerUnits(held.key.at, m.key.at, z, false, func(u int64) bool {
				t, _ := cut(m.key.at, z, u)
				return t == held.key.at && visit(cutRule{finer: true, unit: u, zone: z})
			}) {
				return true
			}
		}
		// m of no zone (read in UTC) may be held's own wall clock, cut: the
		// date or time of day a zoned instant writes.
		if !m.zoned && hz != 0 && coarser(m.key.at, 0, false, func(u int64) bool {
			t, r := cut(held.key.at, hz, u)
			return try(cutRule{unit: u, zone: hz, read: readWall}, t+hz, r+hz, m.key.at)
		}) {
			return true
		}
		// And held of no zone m's: a date of no zone a zoned value's own day
		// is built on.
		if !held.zoned && mz != 0 && finerUnits(held.key.at, m.key.at+mz, 0, false, func(u int64) bool {
			t, _ := cut(m.key.at, mz, u)
			return t+mz == held.key.at && visit(cutRule{finer: true, unit: u, zone: mz, read: readWall})
		}) {
			return true
		}
	case held.key.day && m.key.day:
		if coarser(m.key.at, 0, true, func(u int64) bool {
			t, r := ofDay(held.key.at, u)
			return try(cutRule{unit: u, read: readDays}, t, r, m.key.at)
		}) {
			return true
		}
		return finerUnits(held.key.at, m.key.at, 0, true, func(u int64) bool {
			t, _ := ofDay(m.key.at, u)
			return t == held.key.at && visit(cutRule{finer: true, unit: u, read: readDays})
		})
	case held.key.day:
		// A time of day held, an instant drifted: the instant's own time
		// of day, as it writes it or in UTC.
		for i, mt := range []int64{timeOfDay(m.key.at), m.wall} {
			rd := []cutRead{readDayUTC, readDayOwn}[i]
			if coarser(mt, 0, true, func(u int64) bool {
				t, r := ofDay(held.key.at, u)
				return try(cutRule{unit: u, read: rd}, t, r, mt)
			}) {
				return true
			}
			if finerUnits(held.key.at, mt, 0, true, func(u int64) bool {
				t, _ := ofDay(mt, u)
				return t == held.key.at && visit(cutRule{finer: true, unit: u, read: rd})
			}) {
				return true
			}
		}
	default:
		// An instant held, a time of day drifted.
		for i, ht := range []int64{timeOfDay(held.key.at), held.wall} {
			rd := []cutRead{readDayUTC, readDayOwn}[i]
			if coarser(m.key.at, 0, true, func(u int64) bool {
				t, r := ofDay(ht, u)
				return try(cutRule{unit: u, read: rd}, t, r, m.key.at)
			}) {
				return true
			}
			if finerUnits(ht, m.key.at, 0, true, func(u int64) bool {
				t, _ := ofDay(m.key.at, u)
				return t == ht && visit(cutRule{finer: true, unit: u, read: rd})
			}) {
				return true
			}
		}
	}
	return false
}

// oneCut are the rules (cutRule) each of which names live, the moment F saw
// in a value, by an instant of fside (what F served) and rec, the moment the
// value was recorded with, by an instant of rside (what the recordings
// hold); nil when none does: an application writes a value by one rule, so a copy of what it was
// given was cut so in both runs. An instant may name both moments by two
// rules (truncated and rounded), and is then a copy of neither alone; but
// where another instant names the recorded moment by the rule it names F's
// by, the value is a copy all the same (a neighbour recording F delivered,
// within one unit of the recorded one). Of a finer value (one built on a
// coarser instant) the two instants differ: one instant both runs' values
// are built on is no change the value copied.
func oneCut(fside, rside *heldMoments, rec, live moment) map[cutRule]bool {
	byRule := map[cutRule][]instantKey{}
	for _, hr := range rside.list {
		cuts(hr, rec, func(c cutRule) bool {
			byRule[c] = append(byRule[c], hr.key)
			return false
		})
	}
	if len(byRule) == 0 {
		return nil
	}
	var rules map[cutRule]bool
	for _, hf := range fside.list {
		cuts(hf, live, func(c cutRule) bool {
			for _, k := range byRule[c] {
				if !c.finer || k != hf.key {
					if rules == nil {
						rules = map[cutRule]bool{}
					}
					rules[c] = true
					break
				}
			}
			return false
		})
	}
	return rules
}

// timeOfDay is at's (UnixNano) time of day on its UTC day, in nanoseconds:
// what a time of day's instantKey holds.
func timeOfDay(at int64) int64 {
	const day = int64(24 * time.Hour)
	return (at%day + day) % day
}

// spansOf is instantSpans(text), read once per test (a nil cache reads it
// now).
func (dc *DriftCache) spansOf(text string) []instant {
	if dc == nil {
		return instantSpans(text)
	}
	dc.mu.Lock()
	s, ok := dc.spans[text]
	dc.mu.Unlock()
	if ok {
		return s
	}
	s = instantSpans(text)
	dc.mu.Lock()
	if dc.spans == nil {
		dc.spans = map[string][]instant{}
	}
	dc.spans[text] = s
	dc.mu.Unlock()
	return s
}

// tokensOfForms are the words of forms, by reading.
func tokensOfForms(forms []string) []map[string]bool {
	var t []map[string]bool
	for _, word := range readings {
		set := map[string]bool{}
		for _, f := range forms {
			for w := range tokensBy(f, word) {
				set[w] = true
			}
		}
		t = append(t, set)
	}
	return t
}

// answerFields are the JSON names of the fields of a mock's spec that hold
// its answer (models.MockSpec: an HTTP or HTTP/2 response, Mongo's, MySQL's,
// Postgres v2's, gRPC's, DNS's, and a generic parser's response frames). A
// kind whose spec holds its request and answer in one field (Postgres v3)
// has none here: a Postgres v3 query's answer stands below that field
// (answerPath), and answerClock, which reads these alone, tells no clock
// read of it (rule 2), so its instants are followed.
var answerFields = jsonNames(answerFieldNames)

// requestFields are the JSON names of the fields of a mock's spec that hold
// its request, of each kind that holds its answer in an answerFields field:
// a recording of one of them that holds no answer (a generic parser's frames
// the application sent, that nothing answered) gave the application nothing.
var requestFields = jsonNames(requestFieldNames)

// jsonNames are the JSON names of the fields of models.MockSpec named.
func jsonNames(named []string) map[string]bool {
	return jsonNamesOf(reflect.TypeOf(models.MockSpec{}), named)
}

// jsonNamesOf are the JSON names of the fields of st named.
func jsonNamesOf(st reflect.Type, named []string) map[string]bool {
	out := map[string]bool{}
	for _, name := range named {
		if _, ok := st.FieldByName(name); !ok {
			// TestEveryAnswerFieldIsAnAnswer names it.
			continue
		}
		out[jsonName(st, name)] = true
	}
	return out
}

// jsonName is the JSON name of st's field name (name itself where its tag
// names none).
func jsonName(st reflect.Type, name string) string {
	f, ok := st.FieldByName(name)
	if !ok {
		return name
	}
	if tag := strings.Split(f.Tag.Get("json"), ",")[0]; tag != "" {
		return tag
	}
	return name
}

// answerFieldNames are the Go names of answerFields.
var answerFieldNames = []string{"GenericResponses", "HTTPResp", "MongoResponses", "PostgresResponsesV2", "GRPCResp", "MySQLResponses", "DNSResp", "HTTP2Resp"}

// requestFieldNames are the Go names of requestFields.
var requestFieldNames = []string{"GenericRequests", "HTTPReq", "MongoRequests", "PostgresRequestsV2", "GRPCReq", "MySQLRequests", "DNSReq", "HTTP2Req"}

// postgresV3Query is where a Postgres v3 query's spec stands in a mock's
// spec as RawOf reads it ("postgresV3.query."): the one field of the spec
// that holds the query's request and its answer (models.PostgresV3QuerySpec).
var postgresV3Query = jsonName(reflect.TypeOf(models.MockSpec{}), "PostgresV3") + "." +
	jsonName(reflect.TypeOf(models.PostgresV3Spec{}), "Query") + "."

// postgresV3QueryRequest are the JSON names of the fields of a Postgres v3
// query's spec that hold no answer: what the application sent (its SQL, its
// binds and their formats, the result formats it asked for) and what keploy
// read from it to classify and pair it. Every other field of it (its
// response, its side effects, and a field added later) holds its answer:
// read as given, which only refuses more.
var postgresV3QueryRequest = jsonNamesOf(reflect.TypeOf(models.PostgresV3QuerySpec{}), postgresV3QueryRequestNames)

// postgresV3QueryRequestNames are the Go names of postgresV3QueryRequest.
var postgresV3QueryRequestNames = []string{"Class", "Lifetime", "SQLAstHash", "SQLNormalized", "Relations", "ParamOIDs", "VolatilePositions", "InvocationID", "PrecedingTxState", "BindValues", "BindFormats", "Literals", "ResultFormats"}

// postgresV3QueryField is the field of a Postgres v3 query's spec a raw
// field at path stands in ("response" of "postgresV3.query.response.rows[0]"),
// false where it stands in none.
func postgresV3QueryField(path string) (string, bool) {
	rest, ok := strings.CutPrefix(path, postgresV3Query)
	if !ok || rest == "" {
		return "", false
	}
	return fieldRoot(rest), true
}

// answerPath reports whether a raw field at path stands in its spec's
// answer: in one of answerFields, or in a field of a Postgres v3 query's
// spec that holds its answer (any but postgresV3QueryRequest).
func answerPath(path string) bool {
	if answerFields[fieldRoot(path)] {
		return true
	}
	f, ok := postgresV3QueryField(path)
	return ok && !postgresV3QueryRequest[f]
}

// requestPath reports whether a raw field at path stands in its spec's
// request: in one of requestFields, or in a field of a Postgres v3 query's
// spec among postgresV3QueryRequest.
func requestPath(path string) bool {
	if requestFields[fieldRoot(path)] {
		return true
	}
	f, ok := postgresV3QueryField(path)
	return ok && postgresV3QueryRequest[f]
}

// answerClock are the moments of a protocol-phase mock's answer's own clock
// reads (rule 2), of payload (its spec as RawOf reads it, dropKeys applied)
// recorded from asked to answered: each keyed instant that a form of its answer's text
// holds, within [asked, answered] at its own resolution (a time of day names
// no day, and falls in no span), and that no form of the rest of its text
// (its request) holds. nil when there is none, or the mock's answer or span
// is not known.
func answerClock(payload interface{}, asked, answered time.Time) []time.Time {
	top, ok := payload.(map[string]interface{})
	if !ok || asked.IsZero() || answered.IsZero() || answered.Before(asked) {
		return nil
	}
	answer, rest := map[string]interface{}{}, map[string]interface{}{}
	for k, v := range top {
		if answerFields[k] {
			answer[k] = v
		} else {
			rest[k] = v
		}
	}
	if len(answer) == 0 {
		return nil
	}
	text := func(v interface{}) string {
		var sb strings.Builder
		textOf(v, &sb)
		return sb.String()
	}
	// The answer's instants within the span first: most answers hold none,
	// and the request is read only for those that do.
	var cands []instantKey
	seen := map[instantKey]bool{}
	for _, f := range textForms(text(answer)) {
		for _, sp := range instantSpans(f) {
			if !sp.keyed || seen[sp.key] {
				continue
			}
			seen[sp.key] = true
			at := time.Unix(0, sp.key.at).UTC()
			if at.Before(asked.Truncate(sp.res)) || at.After(answered.Truncate(sp.res)) {
				continue
			}
			cands = append(cands, sp.key)
		}
	}
	if len(cands) == 0 {
		return nil
	}
	requested := map[instantKey]bool{}
	for _, f := range textForms(text(rest)) {
		for _, sp := range instantSpans(f) {
			if sp.keyed {
				requested[sp.key] = true
			}
		}
	}
	var out []time.Time
	for _, k := range cands {
		if !requested[k] {
			out = append(out, time.Unix(0, k.at).UTC())
		}
	}
	return out
}

// AnswerStamp reports whether value, an HTTP answer's Date header, is its
// server's own clock read as it answered (rule 3): HTTP defines the header as
// the moment the origin generated the message (RFC 9110 §6.6.1), no part of
// what it answered, and value names a second within the span keploy stamped
// the exchange in, from its request (asked) to its answer (answered), each at
// the header's resolution of a second. The provenance is the protocol's and
// the recording's stamps: a server whose clock is off by more than the
// exchange's span writes no stamp keploy reads so, and its Date header is
// searched as any text is (a false red, never a false green). false when the
// span is not known, or value is no HTTP-date.
func AnswerStamp(value string, asked, answered time.Time) bool {
	if asked.IsZero() || answered.IsZero() || answered.Before(asked) {
		return false
	}
	t, _, ok := readRFC1123(strings.TrimSpace(value))
	if !ok {
		return false
	}
	return !t.Before(asked.Truncate(time.Second)) && !t.After(answered.Truncate(time.Second))
}

// stampFields are the fields of a mock's spec (by their JSON names, as
// textFields writes them) that hold an answer's Date header: an HTTP
// answer's, and an HTTP/2 answer's.
var stampFields = []string{"Res.header.", "http2Resp.headers."}

// answerStamps are the fields of a payload's text (fields, as RawOf writes
// it) that are its answer's own clock stamp (rule 3, AnswerStamp): an HTTP
// or HTTP/2 answer's Date header, whatever its name's case, within the span
// from asked to answered. nil when there is none.
func answerStamps(text string, fields []rawField, asked, answered time.Time) []rawField {
	var out []rawField
	for _, f := range fields {
		if f.key {
			continue
		}
		for _, prefix := range stampFields {
			name, ok := strings.CutPrefix(f.path, prefix)
			if ok && strings.EqualFold(name, "date") && AnswerStamp(text[f.start:f.end], asked, answered) {
				out = append(out, f)
			}
		}
	}
	return out
}

// stamped reports whether s is its answer's own clock stamp as F served it
// (Seen.Stamp): F served the leaf as recorded. A stamp F served otherwise (a
// variable's value answered there) is no stamp: the premise of rule 3, that F
// served the stamp the recording holds, does not hold.
func stamped(s Seen) bool { return s.Stamp && s.Live == s.Leaf.Value }

// stampsEchoed reports whether v drifted as a word of one stamp to the same
// word of another: a word v held when recorded (lost or moved) is the k-th
// word of a stamp (of a payload served or recorded earlier, or a stamped leaf
// of an exchange F served), and a word v holds in F (gained or moved) is the
// k-th word of a stamp F served, in one reading. An HTTP-date writes its
// parts in one order (RFC 9110 §5.6.7: day name, day, month, year, hour,
// minute, second), so the k-th words of two stamps are one part of each:
// the application took that part of the stamp it was given at record and
// of another stamp F gave it (a batch labelled by its answer's minute, F
// serving another recording of the call). Rule 3's premise, that F served
// the stamp the value could have come from, then fails: every stamp is
// searched as any text (searched, searchedTokens with keep), and a stamped
// leaf as any leaf. A word F saw that one stamp holds at another part than
// the recorded word another holds is no such drift.
func (dc *DriftCache) stampsEchoed(v *valueDrift, served []Earlier, raw []Raw) bool {
	var given, all []string
	for _, x := range raw {
		for _, f := range x.stamps {
			text := x.text[f.start:f.end]
			all = append(all, text)
			if x.served {
				given = append(given, text)
			}
		}
	}
	for _, e := range served {
		if e.Given {
			continue
		}
		for _, s := range e.Seen {
			if stamped(s) {
				all = append(all, s.Leaf.Value)
				given = append(given, s.Live)
			}
		}
	}
	if len(given) == 0 {
		return false
	}
	for i, word := range readings {
		rec := map[int]bool{}
		for _, text := range all {
			for k, w := range wordsBy(text, word) {
				if v.lost[i][w] || v.movedRec[i][w] {
					rec[k] = true
				}
			}
		}
		if len(rec) == 0 {
			continue
		}
		for _, text := range given {
			for k, w := range wordsBy(text, word) {
				if rec[k] && (v.gained[i][w] || v.movedLive[i][w]) {
					return true
				}
			}
		}
	}
	return false
}

// blankStamps is text with each of stamps blanked (replaced by spaces).
func blankStamps(text string, stamps []rawField) string {
	if len(stamps) == 0 {
		return text
	}
	b := []byte(text)
	for _, f := range stamps {
		for i := f.start; i < f.end && i < len(b); i++ {
			b[i] = ' '
		}
	}
	return string(b)
}

// clockKeys are x's answer's clock reads (Raw.AnswerClock) as moments.
func clockKeys(x Raw) map[instantKey]bool {
	if len(x.answerClock) == 0 {
		return nil
	}
	out := make(map[instantKey]bool, len(x.answerClock))
	for _, t := range x.answerClock {
		out[instantKey{at: t.UnixNano()}] = true
	}
	return out
}

// rawKey is a payload's text with its answer's clock reads and stamps, as a
// cache key.
func rawKey(x Raw) string {
	if len(x.answerClock) == 0 && len(x.stamps) == 0 {
		return x.text
	}
	var sb strings.Builder
	sb.WriteString(x.text)
	for _, t := range x.answerClock {
		sb.WriteByte(0)
		sb.WriteString(strconv.FormatInt(t.UnixNano(), 10))
	}
	for _, f := range x.stamps {
		sb.WriteByte(1)
		sb.WriteString(strconv.Itoa(f.start))
		sb.WriteByte('-')
		sb.WriteString(strconv.Itoa(f.end))
	}
	return sb.String()
}

// searched is x as every search of a payload reads it (rules 2 and 3): its
// text and its other forms (rawExtra), with its answer's stamps blanked
// before its forms are read (but where keep says the stamps are searched as
// any text: stampsEchoed), and its answer's own clock reads blanked in every
// form.
func (dc *DriftCache) searched(x Raw, keep bool) [2]string {
	if keep {
		x.stamps = nil
	}
	keys := clockKeys(x)
	if len(keys) == 0 && len(x.stamps) == 0 {
		return [2]string{x.text, dc.rawExtra(x.text)}
	}
	k := rawKey(x)
	dc.mu.Lock()
	s, ok := dc.blanked[k]
	dc.mu.Unlock()
	if ok {
		return s
	}
	text := blankStamps(x.text, x.stamps)
	blank := func(sp instant) bool { return sp.keyed && keys[sp.key] }
	s = [2]string{blankSpans(text, blank), blankSpans(dc.rawExtra(text), blank)}
	dc.mu.Lock()
	if dc.blanked == nil {
		dc.blanked = map[string][2]string{}
	}
	dc.blanked[k] = s
	dc.mu.Unlock()
	return s
}

// searchedTokens are the words of x's text in every declared form, by
// reading, its answer's stamps (unless keep) and own clock reads blanked
// (rules 2 and 3).
func (dc *DriftCache) searchedTokens(x Raw, keep bool) []map[string]bool {
	if keep {
		x.stamps = nil
	}
	keys := clockKeys(x)
	if len(keys) == 0 && len(x.stamps) == 0 {
		return dc.rawTokens(x.text)
	}
	k := rawKey(x)
	dc.mu.Lock()
	t, ok := dc.blankTok[k]
	dc.mu.Unlock()
	if ok {
		return t
	}
	forms := textForms(blankStamps(x.text, x.stamps))
	for i := range forms {
		forms[i] = blankSpans(forms[i], func(sp instant) bool { return sp.keyed && keys[sp.key] })
	}
	t = tokensOfForms(forms)
	dc.mu.Lock()
	if dc.blankTok == nil {
		dc.blankTok = map[string][]map[string]bool{}
	}
	dc.blankTok[k] = t
	dc.mu.Unlock()
	return t
}
