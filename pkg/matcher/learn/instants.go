package learn

import (
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// AN INSTANT IS COMPARED AS ONE UNIT.
//
// A drifted value holding an instant in a declared layout (clockLayouts: RFC
// 1123, ISO 8601, compact, an epoch, a time of day, a time-ordered UUID's
// time) came from an earlier place by that instant only where the place holds
// the same whole instant. Never because the two share words of their date or
// time (a year, a day, an hour, a minute, a second), or a prefix of their
// epoch digits: two instants of one minute share their minute whatever made
// them, so whether such words matched would depend only on when each was
// read. Nor may the verdict depend on whether keploy reads the instant as a
// clock read (clockIn: moved by the lead, or read in its call's window): that
// turns on when the application read it against its other calls, another
// goroutine's included (S12 E2, leg (b): an insert's processed_at read 2 ms
// before a concurrent handler's call, so outside its window, whose minute the
// earlier receive's Date header held, was refused; read after it, it was
// noise).
//
// So each search for where a drifted value came from (appearedIn, sources),
// whether its change is accounted for (unaccounted), or whether the call's
// answer holds what it changed (echoedPart) reads instants as units:
//
//   - a word or a piece of the value inside one of its instants is never
//     searched on its own (valueDrift.outRec, outLive, maskedRec, maskedLive);
//   - a word is never found inside an instant of the other text (holdsUnits,
//     rawTokensOutside, valueDrift.blankRec and the rest);
//   - the value whole is searched as before, a clock read or not: the same
//     value whole elsewhere is followed (a founder decision, for clock reads
//     too), at a leaf and in a payload alike;
//   - in an earlier payload, an instant of the value that changed is found,
//     in any declared layout, where the payloads hold it both ways: its
//     recorded instant, and the instant F saw in its place
//     (echoedInstants). That is an application storing an instant it was
//     sent: in F it sends the instant of the answer F served, a recording, as
//     it sent the recorded one at record. A read of the clock is no such
//     echo, in or out of its window: F's clock is in no recording.
//
// Instants are equal when they name one moment (instantKey: a second and a
// millisecond of it are two; a millisecond written with its trailing zeros
// and without them is one). A time-ordered UUID is an id as well as an
// instant: only its time groups are its instant (uuidTimeWord), masked and
// never searched; its other groups stay words of the id, so the same UUID is
// still followed, and two made in one minute are not.
//
// The word-level learning build rewrites ClassifyIn with instant slots: it
// keeps this rule.

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
// layout in clockLayouts, and what it is (key). keyed is false for a
// time-ordered UUID's time groups, which are masked and never searched.
type instant struct {
	start, end, layout int
	key                instantKey
	keyed              bool
}

// instantKeyOf reads s, a match of layout k, in either case: ok is false when
// its layout does not read it (out of range, no date).
func instantKeyOf(k int, s string) (instantKey, bool) {
	lay := clockLayouts[k]
	up := strings.ToUpper(s)
	t, _, ok := lay.read(up)
	if !ok {
		return instantKey{}, false
	}
	if lay.wraps {
		return instantKey{at: int64(t.Hour())*int64(time.Hour) + int64(t.Minute())*int64(time.Minute) + int64(t.Second())*int64(time.Second) + int64(t.Nanosecond()), day: true}, true
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
			return instantKey{}, false
		}
		ns, err := strconv.ParseInt(frac+strings.Repeat("0", 9-len(frac)), 10, 64)
		if err != nil {
			return instantKey{}, false
		}
		at = sec*int64(time.Second) + ns
	}
	return instantKey{at: at}, true
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
// its time groups.
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
				key, ok := instantKeyOf(k, s[m[0]:m[1]])
				if !ok {
					continue
				}
				sp.key, sp.keyed = key, true
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

// blankInstants is s with each instant it holds (instantSpans) blanked:
// spaces in its place, so no word of it is a word of s, and every other byte
// where it stood.
func blankInstants(s string) string {
	spans := instantSpans(s)
	if len(spans) == 0 {
		return s
	}
	b := []byte(s)
	for _, sp := range spans {
		for i := sp.start; i < sp.end; i++ {
			b[i] = ' '
		}
	}
	return string(b)
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

// echoedInstants are the recorded instants of v's that changed whose live
// instant, too, an earlier payload holds (texts: the payloads appearedIn
// searches, in every form): what an application that stores an instant it
// was sent sends (instants.go). nil when none.
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

// holdsUnits reports whether text holds w at ASCII word boundaries
// (ContainsToken) where every instant of text the occurrence touches lies
// wholly inside it: an instant of text is one unit, which w holds whole or
// not at all. A word of a value is never found in a word of another text's
// date or time, nor an instant inside a longer one ("2026-10-22 11:14"
// inside "2026-10-22 11:14:16").
func (dc *DriftCache) holdsUnits(text, w string) bool {
	if w == "" || text == "" {
		return false
	}
	var spans []instant
	read := false
	for i := 0; i <= len(text)-len(w); {
		j := strings.Index(text[i:], w)
		if j < 0 {
			return false
		}
		at := i + j
		if tokenAt(text, w, at) {
			if !read {
				spans, read = dc.spansOf(text), true
			}
			cut := false
			for _, sp := range spans {
				if sp.start < at+len(w) && at < sp.end && (sp.start < at || sp.end > at+len(w)) {
					cut = true
					break
				}
			}
			if !cut {
				return true
			}
		}
		i = at + 1
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

// rawTokensOutside are the words of a payload's text in every declared form,
// by reading, outside every instant the form holds (rawTokens of its forms,
// each instant blanked): a word of another text's date or time is no word a
// value came from.
func (dc *DriftCache) rawTokensOutside(text string) []map[string]bool {
	dc.mu.Lock()
	t, ok := dc.rawTokOut[text]
	dc.mu.Unlock()
	if ok {
		return t
	}
	forms := textForms(text)
	for i := range forms {
		forms[i] = blankInstants(forms[i])
	}
	for _, word := range readings {
		set := map[string]bool{}
		for _, f := range forms {
			for w := range tokensBy(f, word) {
				set[w] = true
			}
		}
		t = append(t, set)
	}
	dc.mu.Lock()
	if dc.rawTokOut == nil {
		dc.rawTokOut = map[string][]map[string]bool{}
	}
	dc.rawTokOut[text] = t
	dc.mu.Unlock()
	return t
}

// readInstants fills d's reading of its instants (outRec, outLive,
// maskedRec, maskedLive, blank*, instants), once its forms are read.
func (d *valueDrift) readInstants() {
	d.blankRec, d.blankLive = blankInstants(d.rec), blankInstants(d.live)
	d.blankDecRec, d.blankDecLive = blankInstants(d.decRec), blankInstants(d.decLive)
	var mr, ml []string
	seen := map[driftedInstant]bool{}
	for _, f := range d.forms {
		mr, ml = append(mr, blankInstants(f[0])), append(ml, blankInstants(f[1]))
		for _, p := range driftedInstants(f[0], f[1]) {
			if !seen[p] {
				seen[p] = true
				d.instants = append(d.instants, p)
			}
		}
	}
	d.maskedRec, d.maskedLive = strings.Join(mr, "\n"), strings.Join(ml, "\n")
	for _, word := range readings {
		recOut, liveOut := map[string]bool{}, map[string]bool{}
		for i := range mr {
			for w := range tokensBy(mr[i], word) {
				recOut[w] = true
			}
			for w := range tokensBy(ml[i], word) {
				liveOut[w] = true
			}
		}
		d.outRec, d.outLive = append(d.outRec, recOut), append(d.outLive, liveOut)
	}
}

// outsidePiece reports whether piece p of d (its recorded and live text)
// stands outside every instant of d, in some form: a piece inside an
// instant, or cutting one, is compared with the instant, whole.
func (d *valueDrift) outsidePiece(p run) bool {
	return (p.rec == "" || ContainsToken(d.maskedRec, p.rec)) && (p.live == "" || ContainsToken(d.maskedLive, p.live))
}
