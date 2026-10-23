package learn

import (
	"reflect"
	"regexp"
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
//   - the app's own clock read: an instant that moved by F's lead, or that
//     lies, in both runs, within its call's window (Window: from the call's
//     previous call to the call, by the stamps keploy took of both, and the
//     clock F set; PreviousCall). Its words are then no words of a value
//     (clockIn), and only the same read whole elsewhere is followed;
//   - a dependency's own clock read: rules 2 and 3 below, which say that a
//     place is no source of a value, never that a value is noise.
//
// Before any of these excuses a word of a value, the echo reads its instants
// (echoedIn): where an earlier place holds the moment F saw in the value (a
// payload served or recorded earlier, or a leaf of an exchange F served, its
// recorded value or the one F saw, on either side, in every form), the value
// may be a pick from what the application was given, by its clock (a
// timetable's next departure), which moves as a read of the clock does. It
// is then a variable of the place holding the moment it was recorded with,
// or, held nowhere, refused (unaccounted). A place holds a moment as an
// instant of it in any layout, or as its time of day as written in its own
// zone or in UTC (heldMoments).
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
// The word-level learning build (train/feat/word-level-learn) rewrites
// ClassifyIn with instant slots: it keeps these rules.

// instantRes are clockLayouts' patterns in either case: a case-folded form of
// a text holds its instants folded.
var instantRes = func() []*regexp.Regexp {
	out := make([]*regexp.Regexp, len(clockLayouts))
	for i, lay := range clockLayouts {
		out[i] = regexp.MustCompile("(?i)" + lay.re.String())
	}
	return out
}()

// instantKey is the moment an instant names, compared as one unit: at, in
// UnixNano (of a time of day, the nanoseconds into its day, day). Two
// instants are one only with equal keys: an epoch second and that second's
// millisecond 473 are two moments; "…16.47Z" and "…16.470Z" one.
type instantKey struct {
	at  int64
	day bool
}

// instant is one instant a text holds: [start, end) of it, the index of its
// layout in clockLayouts, what it is (key), the time of day its text writes
// (wall) and the resolution its text gives it (res). keyed is false for a
// time-ordered UUID's time groups, which name no moment a payload is
// searched for.
type instant struct {
	start, end, layout int
	key                instantKey
	wall               int64
	res                time.Duration
	keyed              bool
}

// moment is an instant as a payload is searched for it (heldMoments): what
// it is (key), and the time of day its text writes (wall, in nanoseconds
// into its day): of a zoned instant, its wall clock in its own zone
// ("2026-10-22T11:15+05:30" writes 11:15, at 05:45 UTC); of an instant in
// UTC or of no zone, its UTC time of day; of a time of day, itself.
type moment struct {
	key  instantKey
	wall int64
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
		return moment{key: instantKey{at: tod, day: true}, wall: tod}, res, true
	}
	wall := clockOfDay(t)
	at := t.UnixNano()
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
	return moment{key: instantKey{at: at}, wall: wall}, res, true
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
				sp.key, sp.wall, sp.res, sp.keyed = mo.key, mo.wall, res, true
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
// layout in the live value.
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
			byLayout[sp.layout] = append(byLayout[sp.layout], moment{sp.key, sp.wall})
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
			out = append(out, driftedInstant{rec: moment{r.key, r.wall}, live: l[n]})
		}
	}
	return out
}

// readInstants fills d's reading of its instants once its forms are read:
// the instants of its forms that changed (instants), and the moments its
// forms hold, recorded and live (recKeys, liveKeys).
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
			}
		}
		for _, sp := range instantSpans(f[1]) {
			if sp.keyed {
				d.liveKeys[sp.key] = true
			}
		}
	}
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

// echoedInstants are the changed instants of v whose live moment an earlier
// place holds too (held: every payload appearedIn searches, in every form,
// as it stands, and every value F saw or the recording holds at a leaf of an
// exchange F served earlier, on either side): what an application that
// stores an instant it was sent sends, or that picks one from what it was
// served (a departure from a timetable, by its clock). It is read before any
// lead or window excuses a word of v: a pick from served data moves with the
// clock as a read of it does. Each is returned as recorded; nil when none. A
// place holds an instant where it names that moment (heldMoments.holds): an
// instant of the same moment in any layout, or a time of day that is the
// instant's own, to the nanosecond, as written in its own zone or in UTC (a
// timetable's "11:15" holds "2026-10-22 11:15" and
// "2026-10-22T11:15+05:30"), and the other way round.
func echoedInstants(v *valueDrift, held *heldMoments) []driftedInstant {
	var out []driftedInstant
	for _, p := range v.instants {
		if held.holds(p.live) {
			out = append(out, p)
		}
	}
	return out
}

// recorded are the moments of instants as recorded; live as F saw them.
func recordedOf(instants []driftedInstant) []moment {
	out := make([]moment, len(instants))
	for i, p := range instants {
		out[i] = p.rec
	}
	return out
}

func liveOf(instants []driftedInstant) []moment {
	out := make([]moment, len(instants))
	for i, p := range instants {
		out[i] = p.live
	}
	return out
}

// heldIn are the moments texts hold, each read once per test (spansOf).
func (dc *DriftCache) heldIn(held *heldMoments, texts ...string) {
	for _, text := range texts {
		if text != "" {
			held.add(dc.spansOf(text))
		}
	}
}

// holdsInstant reports whether text holds, whole, one of instants, as
// echoedInstants reads a place (heldMoments).
func (dc *DriftCache) holdsInstant(text string, instants []moment) bool {
	if len(instants) == 0 || text == "" {
		return false
	}
	var held heldMoments
	held.add(dc.spansOf(text))
	for _, k := range instants {
		if held.holds(k) {
			return true
		}
	}
	return false
}

// heldMoments are the moments a place's instants name: each keyed instant
// as it is (keys), its times of day (days), and, of its other instants, the
// time of day each writes in its own zone and its UTC one (onDays), in
// nanoseconds.
type heldMoments struct {
	keys         map[instantKey]bool
	days, onDays map[int64]bool
}

// add files spans' keyed instants.
func (h *heldMoments) add(spans []instant) {
	for _, sp := range spans {
		if !sp.keyed {
			continue
		}
		if h.keys == nil {
			h.keys, h.days, h.onDays = map[instantKey]bool{}, map[int64]bool{}, map[int64]bool{}
		}
		h.keys[sp.key] = true
		if sp.key.day {
			h.days[sp.key.at] = true
		} else {
			h.onDays[timeOfDay(sp.key.at)] = true
			h.onDays[sp.wall] = true
		}
	}
}

// holds reports whether the place names m's moment: an instant of it, or,
// for an instant, a time of day that is its own (as it writes it in its
// zone, or in UTC), and, for a time of day, an instant whose own it is.
func (h *heldMoments) holds(m moment) bool {
	if h.keys == nil {
		return false
	}
	if h.keys[m.key] {
		return true
	}
	if m.key.day {
		return h.onDays[m.key.at]
	}
	return h.days[timeOfDay(m.key.at)] || h.days[m.wall]
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
// has none: no clock read of its answer is told (rule 2), and its instants
// are followed.
var answerFields = func() map[string]bool {
	out := map[string]bool{}
	st := reflect.TypeOf(models.MockSpec{})
	for _, name := range answerFieldNames {
		f, ok := st.FieldByName(name)
		if !ok {
			// TestEveryAnswerFieldIsAnAnswer names it.
			continue
		}
		tag := strings.Split(f.Tag.Get("json"), ",")[0]
		if tag == "" {
			tag = name
		}
		out[tag] = true
	}
	return out
}()

// answerFieldNames are the Go names of answerFields.
var answerFieldNames = []string{"GenericResponses", "HTTPResp", "MongoResponses", "PostgresResponsesV2", "GRPCResp", "MySQLResponses", "DNSResp", "HTTP2Resp"}

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
			if x.Served {
				given = append(given, text)
			}
		}
	}
	for _, e := range served {
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
