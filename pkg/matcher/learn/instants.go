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
// The classifier reads a drifted value word by word, at every earlier place
// (02df41407's reading). Three rules read instants (clockLayouts: RFC 1123,
// ISO 8601, compact, an epoch, a time of day, a time-ordered UUID's time),
// and none of them learns a value as noise because of what it looks like
// where F saw what became of it:
//
//  1. At an earlier leaf F saw drift, a unit (rule 3) that is no clock read
//     the lead moved or read in its call's window drifted with that leaf
//     also where the leaf held the unit's moment both ways, in any declared
//     layout (holdsMomentOf: "1792667656470" and "2026-10-22T11:14:16.47Z"
//     are one moment), or its text whole, recorded and live. This only finds
//     more: the leaf's words co-drift with the unit's as any value's do.
//
//  2. A protocol-phase recording's answer's own clock read is no place a
//     value came from (Raw.AnswerClock). A protocol-phase exchange (a
//     driver's handshake, heartbeat or auth, IsProtocolPhase) is the
//     driver's, its answer consumed by the driver: an instant that answer
//     holds, between the recording's request and its answer at its own
//     resolution, that its request does not hold, is the dependency's read
//     of its clock as it answered (a Mongo isMaster's localTime). Every
//     search of a payload (appearedIn, unaccounted) reads it with those
//     instants blanked; the echo (echoedInstants: the payloads hold the
//     instant F saw too) reads it as it stands. Two clocks that read one
//     moment are no copy of each other (S12 E3, leg (b): the consumer's
//     processed_at was read in the millisecond in which the handshake on the
//     connection the driver opened for its insert was answered, and that
//     reply's localTime was that millisecond, in 7 runs of 10). A data-plane
//     answer's instants are searched as any text is: an answer can read
//     back a value the application stored in that second. A dependency whose
//     clock is off by more than the call's span holds no clock read of its
//     answer: its instants are followed, a false red, never a false green.
//
//  3. A value that is one instant whole, of more than one word (an ISO 8601
//     or RFC 1123 stamp, a time of day, an epoch with its fraction), is one
//     unit (valueDrift.unit). In a payload, which F holds no live view of,
//     its words and pieces are never searched on their own, and never left
//     unaccounted for there: the value is searched whole, as it is written
//     (and echoed, echoedInstants). Its words are the date and time of one
//     moment: two stamps of one minute share their minute, and a second's
//     word is every small number a payload holds (a request id, a count),
//     so whether they met would turn on when each was read (S12 E2, leg
//     (b): the typed reading of the insert's processed_at, read 2 ms before
//     a concurrent handler's call and so outside its call's window, met a
//     receive's Date header in its minute "14"). At an earlier leaf F saw,
//     where both runs' values are known, a unit is read word by word as any
//     value is (rule 1 adds to that). Nothing else is a unit: a value of one
//     word (an epoch, a compact stamp, an id whose digits read as a date),
//     and a value an instant only stands inside of ("user-1298498081", a
//     time-ordered UUID), are read word by word everywhere.
//
// echoedPart, which asks whether the call's own answer holds a word of what
// changed, reads a unit's words not at all (rule 3: they are no words of
// their own, and the answer is a recorded payload) and every other value's
// as before. An answer that echoes a request's instant is served with the
// recorded one; an application that checks it goes red at replay, a false
// red, never a false green.
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
// layout in clockLayouts, what it is (key) and the resolution its text
// gives it (res). keyed is false for a time-ordered UUID's time groups,
// which name no moment a payload is searched for.
type instant struct {
	start, end, layout int
	key                instantKey
	res                time.Duration
	keyed              bool
}

// instantKeyOf reads s, a match of layout k, in either case: the moment, its
// resolution, and ok, false when its layout does not read it (out of range,
// no date).
func instantKeyOf(k int, s string) (instantKey, time.Duration, bool) {
	lay := clockLayouts[k]
	up := strings.ToUpper(s)
	t, res, ok := lay.read(up)
	if !ok {
		return instantKey{}, 0, false
	}
	if lay.wraps {
		return instantKey{at: int64(t.Hour())*int64(time.Hour) + int64(t.Minute())*int64(time.Minute) + int64(t.Second())*int64(time.Second) + int64(t.Nanosecond()), day: true}, res, true
	}
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
			return instantKey{}, 0, false
		}
		ns, err := strconv.ParseInt(frac+strings.Repeat("0", 9-len(frac)), 10, 64)
		if err != nil {
			return instantKey{}, 0, false
		}
		at = sec*int64(time.Second) + ns
	}
	return instantKey{at: at}, res, true
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
				key, res, ok := instantKeyOf(k, s[m[0]:m[1]])
				if !ok {
					continue
				}
				sp.key, sp.res, sp.keyed = key, res, true
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

// wholeInstant reports whether s is one keyed instant whole, of more than one
// word (rule 3's unit).
func wholeInstant(s string) bool {
	spans := instantSpans(s)
	return len(spans) == 1 && spans[0].keyed && spans[0].start == 0 && spans[0].end == len(s) && len(wordsBy(s, isWord)) > 1
}

// driftedInstant is a keyed instant of a drifted value that changed: the
// moment it was recorded, and the moment F saw in its place, the k-th of its
// layout in the live value.
type driftedInstant struct{ rec, live instantKey }

// driftedInstants are the keyed instants of a drifted value's recorded and
// live forms (instantSpans) that changed, the k-th of a layout in one against
// the k-th of it in the other, as clockIn pairs them; one the live form holds
// none of is no pair.
func driftedInstants(recorded, live string) []driftedInstant {
	rs := instantSpans(recorded)
	if len(rs) == 0 {
		return nil
	}
	byLayout := map[int][]instantKey{}
	for _, sp := range instantSpans(live) {
		if sp.keyed {
			byLayout[sp.layout] = append(byLayout[sp.layout], sp.key)
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
		if l := byLayout[r.layout]; n < len(l) && l[n] != r.key {
			out = append(out, driftedInstant{rec: r.key, live: l[n]})
		}
	}
	return out
}

// readInstants fills d's reading of its instants once its forms are read:
// the instants of its forms that changed (instants), the moments its forms
// hold, recorded and live (recKeys, liveKeys), and whether it is one unit
// (rule 3: the value, raw or decoded, recorded and live, is one instant
// whole of more than one word).
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
	d.unit = wholeInstant(d.rec) && wholeInstant(d.live) || d.decoded && wholeInstant(d.decRec) && wholeInstant(d.decLive)
}

// holdsMomentOf reports whether e, what F saw drift at an earlier place, held
// a moment a changed instant of the unit v names both ways (rule 1): v's
// recorded moment in e's recorded value and the moment F saw in v in e's
// live value, neither on e's other side.
func (e *valueDrift) holdsMomentOf(v *valueDrift) bool {
	for _, p := range v.instants {
		if e.recKeys[p.rec] && e.liveKeys[p.live] && !e.liveKeys[p.rec] && !e.recKeys[p.live] {
			return true
		}
	}
	return false
}

// echoedInstants are the recorded instants of v's that changed whose live
// instant, too, an earlier payload holds (texts: the payloads appearedIn
// searches, in every form, as they stand): what an application that stores
// an instant it was sent sends (rule 2). nil when none.
func (dc *DriftCache) echoedInstants(v *valueDrift, texts []string) map[instantKey]bool {
	if len(v.instants) == 0 {
		return nil
	}
	held := map[instantKey]bool{}
	for _, text := range texts {
		for _, sp := range dc.spansOf(text) {
			if sp.keyed {
				held[sp.key] = true
			}
		}
	}
	var out map[instantKey]bool
	for _, p := range v.instants {
		if held[p.live] {
			if out == nil {
				out = map[instantKey]bool{}
			}
			out[p.rec] = true
		}
	}
	return out
}

// holdsInstant reports whether text holds, whole, one of instants.
func (dc *DriftCache) holdsInstant(text string, instants map[instantKey]bool) bool {
	if len(instants) == 0 || text == "" {
		return false
	}
	for _, sp := range dc.spansOf(text) {
		if sp.keyed && instants[sp.key] {
			return true
		}
	}
	return false
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

// clockKeys are x's answer's clock reads (Raw.AnswerClock) as moments.
func clockKeys(x Raw) map[instantKey]bool {
	if len(x.AnswerClock) == 0 {
		return nil
	}
	out := make(map[instantKey]bool, len(x.AnswerClock))
	for _, t := range x.AnswerClock {
		out[instantKey{at: t.UnixNano()}] = true
	}
	return out
}

// rawKey is a payload's text with its answer's clock reads, as a cache key.
func rawKey(x Raw) string {
	if len(x.AnswerClock) == 0 {
		return x.Text
	}
	var sb strings.Builder
	sb.WriteString(x.Text)
	for _, t := range x.AnswerClock {
		sb.WriteByte(0)
		sb.WriteString(strconv.FormatInt(t.UnixNano(), 10))
	}
	return sb.String()
}

// searched is x as every search of a payload reads it (rule 2): its text and
// its other forms (rawExtra), with its answer's own clock reads blanked.
func (dc *DriftCache) searched(x Raw) [2]string {
	keys := clockKeys(x)
	if len(keys) == 0 {
		return [2]string{x.Text, dc.rawExtra(x.Text)}
	}
	k := rawKey(x)
	dc.mu.Lock()
	s, ok := dc.blanked[k]
	dc.mu.Unlock()
	if ok {
		return s
	}
	blank := func(sp instant) bool { return sp.keyed && keys[sp.key] }
	s = [2]string{blankSpans(x.Text, blank), blankSpans(dc.rawExtra(x.Text), blank)}
	dc.mu.Lock()
	if dc.blanked == nil {
		dc.blanked = map[string][2]string{}
	}
	dc.blanked[k] = s
	dc.mu.Unlock()
	return s
}

// searchedTokens are the words of x's text in every declared form, by
// reading, its answer's own clock reads blanked (rule 2).
func (dc *DriftCache) searchedTokens(x Raw) []map[string]bool {
	keys := clockKeys(x)
	if len(keys) == 0 {
		return dc.rawTokens(x.Text)
	}
	k := rawKey(x)
	dc.mu.Lock()
	t, ok := dc.blankTok[k]
	dc.mu.Unlock()
	if ok {
		return t
	}
	forms := textForms(x.Text)
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
