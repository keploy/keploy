package learn

import (
	"encoding/hex"
	"encoding/json"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf16"

	"go.keploy.io/server/v3/pkg/models"
)

// Side is the half of an exchange a value is in.
type Side string

// The two sides.
const (
	Request  Side = "request"
	Response Side = "response"
)

// Seen is one value of an exchange F served earlier in the test: the leaf
// the recording holds there, the value F saw there (the live request's, or
// the answer keploy served, with its variables' values), and the leaf's
// variable place ("" when no variable place names it: VarPlace).
type Seen struct {
	Side  Side
	Leaf  Leaf
	Live  string
	Place string
	// Written: the request stores the value at Place (a body it creates or
	// updates a record with), rather than only naming or comparing with it
	// (ReadBack).
	Written bool
	// Record names the record a Written place stores its value in, as the
	// adapter names it (its collection, and the identity the record has in
	// this run); "" when the adapter names none. A read of that record links
	// its answer to this write (ReadBack): the adapter states the link, keploy
	// never guesses it from the value.
	Record string
	// Name is the variable the learn step put at Place (bound or used
	// there), "" for none (Named).
	Name string
}

// Earlier is an HTTP mock the learn step served earlier in the same test,
// leaf by leaf: a variable found there has a place a runtime can bind it at.
type Earlier struct {
	Mock string
	// At is when the mock's request was recorded.
	At time.Time
	// Own: the mock is one of the test's own, not the pool's. Only a test's
	// own mocks carry its variables.
	Own bool
	// Run: served while no test was open (Tracker.Served): a variable bound
	// at it is the run's, which every later test uses (Tracker.RunName).
	Run  bool
	Seen []Seen
}

// Raw is any other mock of the test's recording, of any kind: served earlier
// in the test (Served), or recorded before the request L classifies (At).
// Text is its recorded request and answer, the payload only, as one text: a
// value found there flowed from that mock, at a place no leaf names.
type Raw struct {
	Mock   string
	At     time.Time
	Served bool
	Text   string
}

// Kind is what a drifted leaf is learned as.
type Kind string

// The three kinds (design §7.2 step 6; an echo is the design's
// correlation). They never collapse into each other: noise is not compared,
// an echo is bound at the request and answered, a variable is asserted equal
// to the value at its earlier place.
const (
	KindNoise    Kind = "noise"
	KindEcho     Kind = "echo"
	KindVariable Kind = "variable"
	// KindUnaccounted is a drift keploy can learn as none of the three:
	// no earlier place is its source, and yet its readings cannot show it
	// came from nowhere (unaccounted). The request is refused.
	KindUnaccounted Kind = "unaccounted"
)

// Place is where a value is: a mock, and, when a leaf names it, a side and
// the leaf. A place found in a mock's raw text has no side and no path.
type Place struct {
	Mock  string
	Side  Side
	Path  string
	Noise string
	// Var is the leaf's variable place, "" when it has none (Seen.Place).
	Var string
	// Live is the value F saw at the leaf.
	Live string
	// Whole: the leaf held the value whole (not as a word of a longer
	// value), so the value can be bound there.
	Whole bool
	// Own: the mock is one of the test's own (Earlier.Own).
	Own bool
	// Run: the mock was served while no test was open (Earlier.Run).
	Run bool
	// Name is the variable already at Var (Seen.Name), "" for none.
	Name string
}

// Learned is what L learned about one drifted leaf of the paired mock.
type Learned struct {
	Kind     Kind
	Recorded Leaf
	Live     Leaf
	// From is a variable's earlier place, where it is bound.
	From Place
	// Echo are an echo's places in the paired mock's recorded answer.
	Echo []Leaf
	// Why, of an unaccounted drift, is why it cannot be noise. It never
	// holds a value.
	Why string
}

// Classify decides each drifted leaf of the mock rec was paired with
// (design §7.2 step 6). at is when that mock's request was recorded;
// earlier are the HTTP mocks F served before it in the same test, leaf by
// leaf, and raw every other mock of the test's recording that came before
// it; resp are the leaves of its recorded answer.
//
//   - Variable: the recorded value appeared earlier in the test's recording,
//     whatever its type there, in any kind of mock: whole, or a part of it
//     that drifted together with a part of the earlier value (sources). It
//     is asserted equal to the live value at that earlier place, wherever F
//     saw what: a value that came from somewhere is never stopped being
//     compared. The place a leaf names binds it: where the value was sent
//     whole, the test's own call before the run's (appeared); failing that,
//     the first raw mock holding it.
//
//   - Echo: the recorded answer echoes the recorded value (a scalar equal to
//     it, or a string holding it as a token).
//
//   - Noise otherwise: not compared, its type still enforced.
//
//   - Unaccounted: none of these, and yet the declared readings cannot show
//     it came from nowhere (unaccounted): refused, never noise.
//
// An empty recorded value is never followed: it carries nothing to follow.
// dc keeps what each value drifted for the test (nil: for this call).
func Classify(dc *DriftCache, drift []string, live, rec Shape, at time.Time, earlier []Earlier, raw []Raw, resp []Leaf) []Learned {
	if dc == nil {
		dc = NewDriftCache()
	}
	served := append([]Earlier(nil), earlier...)
	sort.SliceStable(served, func(i, j int) bool { return served[i].At.Before(served[j].At) })
	out := make([]Learned, 0, len(drift))
	for _, p := range drift {
		r, l := rec.Leaves[p], live.Leaves[p]
		ln := Learned{Kind: KindNoise, Recorded: r, Live: l}
		if r.Value != "" {
			if from, ok := appeared(dc, r.Value, l.Value, at, served, raw); ok {
				ln.Kind, ln.From = KindVariable, from
			} else if echo := Echoes(dc, r, resp); len(echo) > 0 {
				ln.Kind, ln.Echo = KindEcho, echo
			}
		}
		// Noise, or an echo, only where its readings account for all it
		// changed: an echo is answered, never asserted against a source.
		if ln.Kind == KindNoise || ln.Kind == KindEcho {
			if why := unaccounted(dc, dc.of(r.Value, l.Value), at, served, raw); why != "" {
				ln.Kind, ln.Why, ln.Echo = KindUnaccounted, why, nil
			}
		}
		// Noise only where no word of what changed comes back in the
		// call's own answer: an id sent inside a longer value (a GraphQL
		// argument, an OData filter) that the answer holds is an echo keploy
		// cannot answer with this run's value.
		if ln.Kind == KindNoise {
			if why := echoedPart(dc, dc.of(r.Value, l.Value), resp); why != "" {
				ln.Kind, ln.Why = KindUnaccounted, why
			}
		}
		out = append(out, ln)
	}
	return out
}

// echoedPart says where the call's recorded answer (resp) holds a word of
// what drifted value v changed (a word it lost, or one outside the longest
// common subsequences, recorded side; no part of a clock read the lead
// moved), in any declared form of the answer, in its case or folded; "" when
// nowhere.
func echoedPart(dc *DriftCache, v *valueDrift, resp []Leaf) string {
	for i := range readings {
		words := map[string]bool{}
		for t := range v.lost[i] {
			if !clockWord(t, true, v, v.clock) {
				words[t] = true
			}
		}
		for t := range v.movedRec[i] {
			if !allClock(t, true, v.clock) {
				words[t] = true
			}
		}
		if len(words) == 0 {
			continue
		}
		for _, a := range resp {
			if a.Value == "" {
				continue
			}
			toks := dc.rawTokens(a.Value)[i]
			for t := range words {
				if toks[t] || toks[strings.ToLower(t)] {
					return "a word of it comes back in the answer at " + a.Path + ", which keploy cannot answer with this run's value"
				}
			}
		}
	}
	return ""
}

// run is a part of a value that drifted between record and F: what the
// recording held there and what F saw there.
type run struct{ rec, live string }

// segment is a stretch of a value that is all word characters (letters and
// digits) or none.
type segment struct {
	text string
	word bool
}

// wordByte reports whether c continues a word of a value: an ASCII letter
// or digit, or any byte of a character beyond ASCII (an emoji, a CJK or
// Cyrillic word drifts as a word too).
func wordByte(c byte) bool { return isWord(c) || c >= 0x80 }

// idByte reports whether c continues an id of a value: a word byte, or '-'
// or '_', which ids of the URL-safe alphabets carry inside them (nanoid,
// base64url, token_urlsafe), so the same id splits into a different number
// of words run to run.
func idByte(c byte) bool { return wordByte(c) || c == '-' || c == '_' }

// segmentsBy are the stretches of s that are all bytes word says continue a
// word, or none.
func segmentsBy(s string, word func(byte) bool) []segment {
	var out []segment
	for i := 0; i < len(s); {
		w, j := word(s[i]), i+1
		for j < len(s) && word(s[j]) == w {
			j++
		}
		out = append(out, segment{s[i:j], w})
		i = j
	}
	return out
}

// runsBy are the parts of a drifted value that drifted (its runs):
// recorded and live read word by word (as word says), each maximal stretch
// of changed words that no unchanged word interrupts, with what separates
// them ("<uuid>" of "<uuid>.json", "00:41Z" of a clock reading, both ids of
// "a-<id>-b-<id>"). A recorded and a live value of different makes are one
// stretch: what lies between their common start and end, with the words it
// touches.
func runsBy(recorded, live string, word func(byte) bool) []run {
	r, l := segmentsBy(recorded, word), segmentsBy(live, word)
	aligned := len(r) == len(l) && len(r) > 0 && r[0].word == l[0].word
	if !aligned {
		p, q := 0, 0
		for p < len(recorded) && p < len(live) && recorded[p] == live[p] {
			p++
		}
		for q < len(recorded)-p && q < len(live)-p && recorded[len(recorded)-1-q] == live[len(live)-1-q] {
			q++
		}
		rs, re := widen(recorded, p, len(recorded)-q, word)
		ls, le := widen(live, p, len(live)-q, word)
		if !hasWord(recorded[rs:re]) || !hasWord(live[ls:le]) {
			return nil
		}
		return []run{{recorded[rs:re], live[ls:le]}}
	}
	var out []run
	from := -1 // the run's first changed word
	last := -1 // its last
	flush := func() {
		if from >= 0 {
			var rb, lb strings.Builder
			for i := from; i <= last; i++ {
				rb.WriteString(r[i].text)
				lb.WriteString(l[i].text)
			}
			out = append(out, run{rb.String(), lb.String()})
		}
		from, last = -1, -1
	}
	for i := range r {
		if !r[i].word {
			continue
		}
		if r[i].text == l[i].text {
			flush()
			continue
		}
		if from < 0 {
			from = i
		}
		last = i
	}
	flush()
	return out
}

// widen is [i, j) of s with the words it touches at either end.
func widen(s string, i, j int, word func(byte) bool) (int, int) {
	if i > j {
		i = j
	}
	for i > 0 && word(s[i-1]) {
		i--
	}
	for j < len(s) && word(s[j]) {
		j++
	}
	return i, j
}

func hasWord(s string) bool {
	for i := 0; i < len(s); i++ {
		if wordByte(s[i]) {
			return true
		}
	}
	return false
}

// wordPairsBy are the changed words of a run, as word reads them, recorded
// and live, when both sides of it hold as many words (nil otherwise).
func wordPairsBy(x run, word func(byte) bool) []run {
	r, l := segmentsBy(x.rec, word), segmentsBy(x.live, word)
	if len(r) != len(l) {
		return nil
	}
	var out []run
	for i := range r {
		if r[i].word && l[i].word && r[i].text != l[i].text {
			out = append(out, run{r[i].text, l[i].text})
		}
	}
	return out
}

// readings are the ways a value is read into words: ASCII letters and
// digits (an id inside text of another script, "订单A7K29Q已发货"), those and
// any character beyond ASCII (an emoji, a CJK or Cyrillic word), and ids
// with '-' and '_' inside them (nanoid, base64url, token_urlsafe).
var readings = []func(byte) bool{isWord, wordByte, idByte}

// pieces are the parts of a drifted value that drifted, each recorded and
// live: its runs and their changed words, in every reading.
func pieces(recorded, live string) []run {
	seen := map[run]bool{}
	var out []run
	add := func(x run) {
		if !seen[x] {
			seen[x] = true
			out = append(out, x)
		}
	}
	for _, word := range readings {
		for _, x := range runsBy(recorded, live, word) {
			add(x)
			for _, w := range wordPairsBy(x, word) {
				add(w)
			}
		}
	}
	return out
}

// maxMovedCells bounds the table movedWords compares two word lists in;
// beyond it every word between their common start and end has moved.
const maxMovedCells = 1 << 18

// movedWords are the words of rec and of live (a value's words, recorded and
// live, in order) that are no part of a longest common subsequence of the
// two: a word lost or gained, one held once more, and one of each pair held
// in another order. Words that kept their order stay, however much changed
// around them. Two longest subsequences are taken, ties broken each way, and
// a word outside either has moved, so of two words that swapped both have.
func movedWords(rec, live []string) (movedRec, movedLive []string) {
	p := 0
	for p < len(rec) && p < len(live) && rec[p] == live[p] {
		p++
	}
	q := 0
	for q < len(rec)-p && q < len(live)-p && rec[len(rec)-1-q] == live[len(live)-1-q] {
		q++
	}
	r, l := rec[p:len(rec)-q], live[p:len(live)-q]
	if len(r) == 0 || len(l) == 0 || len(r)*len(l) > maxMovedCells {
		return r, l
	}
	// lcs[i][j] is the length of a longest common subsequence of r[i:] and
	// l[j:].
	w := len(l) + 1
	lcs := make([]int32, (len(r)+1)*w)
	for i := len(r) - 1; i >= 0; i-- {
		for j := len(l) - 1; j >= 0; j-- {
			switch {
			case r[i] == l[j]:
				lcs[i*w+j] = lcs[(i+1)*w+j+1] + 1
			case lcs[(i+1)*w+j] >= lcs[i*w+j+1]:
				lcs[i*w+j] = lcs[(i+1)*w+j]
			default:
				lcs[i*w+j] = lcs[i*w+j+1]
			}
		}
	}
	keptR, keptL := make([]int, len(r)), make([]int, len(l))
	for _, skipRec := range []bool{true, false} {
		i, j := 0, 0
		for i < len(r) && j < len(l) {
			switch {
			case r[i] == l[j] && lcs[i*w+j] == lcs[(i+1)*w+j+1]+1:
				keptR[i]++
				keptL[j]++
				i, j = i+1, j+1
			case lcs[(i+1)*w+j] > lcs[i*w+j+1] || lcs[(i+1)*w+j] == lcs[i*w+j+1] && skipRec:
				i++
			default:
				j++
			}
		}
	}
	for i, k := range keptR {
		if k < 2 {
			movedRec = append(movedRec, r[i])
		}
	}
	for j, k := range keptL {
		if k < 2 {
			movedLive = append(movedLive, l[j])
		}
	}
	return movedRec, movedLive
}

// isClock reports whether p (what a value held when recorded, and in F)
// drifted only with F's clock: each ASCII word of it that changed (that the
// other side does not hold) is a part of a clock read that moved by the
// lead, in either value (cs), and one did.
func isClock(p run, cs ...clockSet) bool {
	r, l := tokensBy(p.rec, isWord), tokensBy(p.live, isWord)
	changed := false
	for w := range r {
		if l[w] {
			continue
		}
		changed = true
		if !clockSide(w, true, cs) {
			return false
		}
	}
	for w := range l {
		if r[w] {
			continue
		}
		changed = true
		if !clockSide(w, false, cs) {
			return false
		}
	}
	return changed
}

// clockSide reports whether word w is a clock word of one of cs, on its
// recorded side or its live one.
func clockSide(w string, recorded bool, cs []clockSet) bool {
	for _, c := range cs {
		if recorded && c.rec[w] || !recorded && c.live[w] {
			return true
		}
	}
	return false
}

// tokensBy are the words of s as word reads it.
func tokensBy(s string, word func(byte) bool) map[string]bool {
	out := map[string]bool{}
	for _, seg := range segmentsBy(s, word) {
		if seg.word {
			out[seg.text] = true
		}
	}
	return out
}

// wordsBy are the words of s as word reads it, in order.
func wordsBy(s string, word func(byte) bool) []string {
	var out []string
	for _, seg := range segmentsBy(s, word) {
		if seg.word {
			out = append(out, seg.text)
		}
	}
	return out
}

// valueDrift is what one drifted value (rec, live) changed, read once per
// test: its clock reads the lead moved, the words each reading finds the
// recorded value lost and the live one gained (in every declared form), its
// pieces, and its decoded form.
type valueDrift struct {
	rec, live    string
	clock        clockSet
	lost, gained []map[string]bool // by reading
	// movedRec and movedLive are the words of what changed, recorded and
	// live, by reading, in every form: the words outside a longest common
	// subsequence of the recorded and the live words (movedWords). A word
	// held in another order, or once more, is one.
	movedRec, movedLive []map[string]bool
	pieces              []run
	decRec, decLive     string
	decoded             bool
	// forms are the value in the declared forms: raw where it has escapes,
	// and formsOf.
	forms [][2]string
	// recTok and liveTok are every word of the value, recorded and live,
	// in every form, by reading.
	recTok, liveTok []map[string]bool
	// encodedRec is the text the recorded value's encoded runs decode to
	// (b64Text), "" when none.
	encodedRec string
	// unparsed says what of its drift the declared readings cannot read
	// (unparsed).
	unparsed string
	// wholes are the value whole, recorded, in each declared form.
	wholes map[string]bool
}

// DriftCache keeps each value's drift for one test of the learn step: the
// earlier places of a test are read once, not once per request.
type DriftCache struct {
	mu     sync.Mutex
	m      map[run]*valueDrift
	raw    map[string]string
	rawTok map[string][]map[string]bool
	// mockText is each mock's text as Classify reads it (RawOf), by mock:
	// the store's mocks are read once per test, not once per request.
	mockText map[*models.Mock]string
	// readText is each mock's text with its answer read back (ReadRaw), by
	// mock: apart from mockText, which RawOf fills with a parked mock's
	// text without its answer.
	readText map[*models.Mock]string
}

// NewDriftCache makes an empty cache.
func NewDriftCache() *DriftCache {
	return &DriftCache{m: map[run]*valueDrift{}, raw: map[string]string{}, rawTok: map[string][]map[string]bool{}, mockText: map[*models.Mock]string{}, readText: map[*models.Mock]string{}}
}

// ReadRaw is RawOf(m, served) with m's answer read back first when the agent
// parked it on disk (models.Mock.HasSpilledResponse), m's text read once for
// the test (a nil cache reads it now). RawOf reads a parked mock as it is,
// without its answer, so a value its answer held reads as appearing nowhere.
// err is non-nil when the answer cannot be read back: what it held cannot be
// told, and nothing may be learned on the strength of its absence.
func (dc *DriftCache) ReadRaw(m *models.Mock, served bool) (Raw, error) {
	if dc == nil || !m.HasSpilledResponse() {
		if dc == nil {
			return ReadRaw(m, served)
		}
		return dc.RawOf(m, served), nil
	}
	dc.mu.Lock()
	text, ok := dc.readText[m]
	dc.mu.Unlock()
	if ok {
		return Raw{Mock: m.Name, At: m.Spec.ReqTimestampMock, Served: served, Text: text}, nil
	}
	r, err := ReadRaw(m, served)
	if err != nil {
		return r, err
	}
	dc.mu.Lock()
	if dc.readText == nil {
		dc.readText = map[*models.Mock]string{}
	}
	dc.readText[m] = r.Text
	dc.mu.Unlock()
	return r, nil
}

// ReadRaw is RawOf with m's answer read back first when the agent parked it
// on disk, on a copy (the pooled mock keeps it parked); err is why it cannot
// be read back.
func ReadRaw(m *models.Mock, served bool) (Raw, error) {
	if !m.HasSpilledResponse() {
		return RawOf(m, served), nil
	}
	full := m.ShallowCopy()
	if err := full.HydrateResponse(); err != nil {
		return Raw{Mock: m.Name, At: m.Spec.ReqTimestampMock, Served: served}, err
	}
	return RawOf(full, served), nil
}

// RawOf is RawOf(m, served), m's text read once for the test (a nil cache
// reads it now).
func (dc *DriftCache) RawOf(m *models.Mock, served bool) Raw {
	if dc == nil {
		return RawOf(m, served)
	}
	dc.mu.Lock()
	text, ok := dc.mockText[m]
	dc.mu.Unlock()
	if ok {
		return Raw{Mock: m.Name, At: m.Spec.ReqTimestampMock, Served: served, Text: text}
	}
	r := RawOf(m, served)
	dc.mu.Lock()
	dc.mockText[m] = r.Text
	dc.mu.Unlock()
	return r
}

// rawTokens are every word of a payload's text in every declared form, by
// reading.
func (dc *DriftCache) rawTokens(text string) []map[string]bool {
	dc.mu.Lock()
	t, ok := dc.rawTok[text]
	dc.mu.Unlock()
	if ok {
		return t
	}
	forms := textForms(text)
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
	dc.rawTok[text] = t
	dc.mu.Unlock()
	return t
}

// rawExtra is what a payload's text is searched in besides itself: its
// other declared forms (textForms: decoded, case-folded, and the text its
// encoded runs decode to), one per line.
func (dc *DriftCache) rawExtra(text string) string {
	dc.mu.Lock()
	t, ok := dc.raw[text]
	dc.mu.Unlock()
	if ok {
		return t
	}
	t = strings.Join(textForms(text)[1:], "\n")
	dc.mu.Lock()
	dc.raw[text] = t
	dc.mu.Unlock()
	return t
}

func (dc *DriftCache) of(rec, live string) *valueDrift {
	k := run{rec, live}
	dc.mu.Lock()
	d, ok := dc.m[k]
	dc.mu.Unlock()
	if ok {
		return d
	}
	d = &valueDrift{rec: rec, live: live, clock: clockOf(rec, live)}
	d.decRec, d.decLive = unescaped(rec), unescaped(live)
	d.decoded = d.decRec != rec || d.decLive != live
	// A value with escapes is read raw and decoded: a '%' or '\' may be
	// literal ("%<ref>%" of a LIKE, a Windows path), and a decode that
	// mangles it must not hide the word it stands before.
	merge := func(c clockSet) {
		for w := range c.rec {
			d.clock.rec[w] = true
		}
		for w := range c.live {
			d.clock.live[w] = true
		}
	}
	var dclock clockSet
	if d.decoded {
		dclock = clockOf(d.decRec, d.decLive)
		merge(dclock)
	}
	forms, encoded := formsOf(rec, live, d.decRec, d.decLive, d.clock)
	d.encodedRec = encoded[0]
	// The clock reads of every other form (the first is the decoded value,
	// read above).
	for _, f := range forms[1:] {
		merge(clockOf(f[0], f[1]))
	}
	if d.decoded {
		forms = append([][2]string{{rec, live}}, forms...)
		d.clock.whole = d.clock.whole || dclock.whole
		// Read raw, a clock read's escapes are no words of their own: an
		// escape's remnant glued to a clock word is one ("3A41Z" of
		// "%3A41Z", "n2026" of "\n2026").
		escapeClock(rec, d.clock.rec)
		escapeClock(live, d.clock.live)
	}
	// A case-folded form reads the clock's words case-folded too.
	for w := range d.clock.rec {
		d.clock.rec[strings.ToLower(w)] = true
	}
	for w := range d.clock.live {
		d.clock.live[strings.ToLower(w)] = true
	}
	d.forms = forms
	// What the readings cannot read whole, in every form but the raw one
	// of a value with escapes (whose '%' and '\' are the escapes).
	for i, f := range forms {
		if d.decoded && i == 0 {
			continue
		}
		if why := unparsed(f[0], f[1], d.clock); why != "" {
			d.unparsed = why
			break
		}
	}
	// The value whole in each form (each line of its decoded text) that
	// drifted: the same value whole in another place, in any reading, is
	// followed. A line both sides hold (a JWT's header) did not drift.
	liveLines := map[string]bool{}
	for _, f := range forms {
		for _, w := range strings.Split(f[1], "\n") {
			liveLines[w] = true
		}
	}
	for _, f := range forms {
		for _, w := range strings.Split(f[0], "\n") {
			if w != "" && !liveLines[w] {
				if d.wholes == nil {
					d.wholes = map[string]bool{}
				}
				d.wholes[w] = true
			}
		}
	}
	for _, word := range readings {
		lost, gained := map[string]bool{}, map[string]bool{}
		recTok, liveTok := map[string]bool{}, map[string]bool{}
		for _, f := range forms {
			r, l := tokensBy(f[0], word), tokensBy(f[1], word)
			for t := range r {
				recTok[t] = true
				if !l[t] {
					lost[t] = true
				}
			}
			for t := range l {
				liveTok[t] = true
				if !r[t] {
					gained[t] = true
				}
			}
		}
		d.lost, d.gained = append(d.lost, lost), append(d.gained, gained)
		d.recTok, d.liveTok = append(d.recTok, recTok), append(d.liveTok, liveTok)
		movedRec, movedLive := map[string]bool{}, map[string]bool{}
		for _, f := range forms {
			mr, ml := movedWords(wordsBy(f[0], word), wordsBy(f[1], word))
			for _, t := range mr {
				movedRec[t] = true
			}
			for _, t := range ml {
				movedLive[t] = true
			}
		}
		d.movedRec, d.movedLive = append(d.movedRec, movedRec), append(d.movedLive, movedLive)
	}
	// Pieces of the value in every form.
	for _, f := range forms {
		d.pieces = append(d.pieces, pieces(f[0], f[1])...)
	}
	dc.mu.Lock()
	dc.m[k] = d
	dc.mu.Unlock()
	return d
}

// clockWord reports whether token t, which value d lost (recorded) or
// gained, changed only with F's clock: every ASCII word of it the other side
// of d does not hold is a clock word of one of cs (a token whose words the
// other side all holds is only regrouped: nothing new).
func clockWord(t string, recorded bool, d *valueDrift, cs ...clockSet) bool {
	other := map[string]bool{}
	for _, f := range d.forms {
		side := f[1]
		if !recorded {
			side = f[0]
		}
		for w := range tokensBy(side, isWord) {
			other[w] = true
		}
	}
	ws := tokensBy(t, isWord)
	if len(ws) == 0 {
		// No word of a clock read: never one.
		return false
	}
	for w := range ws {
		if !other[w] && !clockSide(w, recorded, cs) {
			return false
		}
	}
	return true
}

// unescaped is s with its percent-escapes decoded and its backslash escapes
// read: "\uXXXX" (and an escaped surrogate pair) as the character it names,
// "\\" as "\", and the others of a JSON string ("\n", "\t", "\"", "\/") as
// a space, a separator: a value right after one ("uploads%2F<id>",
// "\n<id>") is a word of its own there.
func unescaped(s string) string {
	// Each escape on its own (a stray '%' leaves the others to decode), and
	// again while that changes anything, a few times at most (a value
	// encoded inside a value encoded again: "%252F", "\\\\n").
	for i := 0; i < 4; i++ {
		d := percentEscape.ReplaceAllStringFunc(s, func(m string) string {
			b, err := hex.DecodeString(m[1:])
			if err != nil {
				return m
			}
			return string(b)
		})
		d = escapedSurrogates.ReplaceAllStringFunc(d, func(m string) string {
			// Four hex digits each (escapedSurrogates): 16 bits.
			hi, _ := strconv.ParseUint(m[2:6], 16, 16)
			lo, _ := strconv.ParseUint(m[8:12], 16, 16)
			return string(utf16.DecodeRune(rune(hi), rune(lo)))
		})
		d = backslashEscape.ReplaceAllStringFunc(d, func(m string) string {
			switch {
			case m == `\\`:
				return `\`
			case m[1] == 'u':
				// The character it names (a JSON writer's escape of what
				// is beyond ASCII).
				// Four hex digits (backslashEscape): 16 bits.
				if r, err := strconv.ParseUint(m[2:], 16, 16); err == nil {
					return string(rune(r))
				}
			}
			return " "
		})

		if d == s {
			break
		}
		s = d
	}
	return s
}

var (
	// escapedSurrogates are a high and a low surrogate escaped as two
	// \uXXXX: one character beyond the basic plane (an emoji).
	escapedSurrogates = regexp.MustCompile(`\\u[dD][89abAB][0-9a-fA-F]{2}\\u[dD][c-fC-F][0-9a-fA-F]{2}`)
	percentEscape     = regexp.MustCompile(`%[0-9a-fA-F]{2}`)
	backslashEscape   = regexp.MustCompile(`\\(?:u[0-9a-fA-F]{4}|[nrtbf"\\/])`)
)

// sources reports whether the earlier place e (what F saw drift there) is
// where the drifted value v came from, whole or in part (whole: the earlier
// place held v's recorded value whole, the recorded value appeared there:
// design v14 §7.2 step 6; a whole repeated clock value is followed too, not
// learned as noise). A part counts where the two drifted together
// (co-drift), recorded and live alike, however short:
//
//   - in one reading of the values or of their percent- and
//     backslash-decoded forms, the two both lost a word and both gained a
//     word, wherever they sit and whatever else changed around them
//     ("<id>" of "<id>.json", of "doc:<id>", of "uploads%2F<id>"; "<trace>"
//     of "<trace>:<span1>" and "<trace>:<span2>"; a ref next to a fresh
//     base64 signature in two callback URLs);
//   - or a piece of one (a run or a changed word) is a token of the other
//     where the other drifted: its recorded text in the recorded value and
//     its live text in the live one, neither in the other side.
//
// What drifted only because F's clock runs FullRoundLead past the recording
// is no part: a word that is a part of a clock read that moved by exactly
// the lead, in either value (two clock reads of one second in other
// precisions or forms share their minute and second; two UUID v7s of one
// minute share their time group). Every other shared changed word is a value
// that flowed.
func sources(e, v *valueDrift) (found, whole bool) {
	if e.rec == v.rec {
		return true, true
	}
	// The same value whole in another reading (decoded, a base64 run's
	// text): it flowed, and cannot be bound there.
	for w := range v.wholes {
		if e.wholes[w] {
			return true, false
		}
	}
	// A whole clock read standing whole in the other's base64 text (a
	// JWT's iat): a whole repeated value, as in a payload.
	if e.clock.whole && v.b64Holds(e) || v.clock.whole && e.b64Holds(v) {
		return true, false
	}
	for i := range readings {
		lost, gained := false, false
		for t := range v.lost[i] {
			if e.lost[i][t] && !clockWord(t, true, v, v.clock, e.clock) {
				lost = true
				break
			}
		}
		for t := range v.gained[i] {
			if e.gained[i][t] && !clockWord(t, false, v, v.clock, e.clock) {
				gained = true
				break
			}
		}
		if lost && gained {
			return true, false
		}
	}
	drifted := func(p run, rec, live string) bool {
		return ContainsToken(rec, p.rec) && ContainsToken(live, p.live) && !ContainsToken(live, p.rec) && !ContainsToken(rec, p.live)
	}
	for _, p := range v.pieces {
		if !isClock(p, v.clock, e.clock) && (drifted(p, e.rec, e.live) || e.decoded && drifted(p, e.decRec, e.decLive)) {
			return true, false
		}
	}
	for _, p := range e.pieces {
		if !isClock(p, e.clock, v.clock) && (drifted(p, v.rec, v.live) || v.decoded && drifted(p, v.decRec, v.decLive)) {
			return true, false
		}
	}
	return false, false
}

// appeared is the earlier place of the test's recording the drifted value
// (recorded, now) came from: a leaf of an HTTP mock F served before (every
// one of them came earlier in the test) that F saw drift (sources), else a
// raw mock served before or recorded before at (of which F saw no live
// value).
func appeared(dc *DriftCache, recorded, now string, at time.Time, served []Earlier, raw []Raw) (Place, bool) {
	v := dc.of(recorded, now)
	// The place is where the value was sent whole, not where it was answered
	// or sent inside a longer value (a variable is bound where the
	// application sent it, whole): the test's own call that sent it first,
	// else the run's, else another's; then the same inside a longer value;
	// an answer that gave it only when no call sent it. Earliest within each.
	var best Place
	rank := 7
	for _, e := range served {
		for _, s := range e.Seen {
			// A place F saw hold its recorded value did not drift: the
			// value there is the same every run, and a value that drifted
			// here did not come from it.
			if s.Live == s.Leaf.Value {
				continue
			}
			ok, whole := sources(dc.of(s.Leaf.Value, s.Live), v)
			if !ok {
				continue
			}
			r := 6
			if s.Side == Request {
				switch {
				case e.Own:
					r = 0
				case e.Run:
					r = 1
				default:
					r = 2
				}
				if !whole {
					r += 3
				}
			}
			if r < rank {
				best, rank = Place{Mock: e.Mock, Side: s.Side, Path: s.Leaf.Path, Noise: s.Leaf.Noise,
					Var: s.Place, Live: s.Live, Whole: whole, Own: e.Own, Run: e.Run, Name: s.Name}, r
			}
			if rank == 0 {
				return best, true
			}
		}
	}
	if rank < 7 {
		return best, true
	}
	// Of a mock with no live view, F cannot tell whether a place drifted:
	// the recorded value, or what of it drifted (a changed word, a piece),
	// counts as a token of its payload (a token stored in a row before it
	// went out in "Bearer <token>"), but for what drifted with F's clock. A
	// whole clock read the lead moved counts only where it stands whole
	// there (a payload holding it with more precision, a zone or a date
	// around it holds a clock read of its own form, which is noise). It errs
	// to refusing.
	var searched []string
	if !v.clock.whole {
		searched = append(searched, recorded)
	}
	add := func(p run) {
		if p.rec != recorded && !isClock(p, v.clock) {
			searched = append(searched, p.rec)
		}
	}
	for i := range readings {
		for t := range v.lost[i] {
			if t != recorded && !clockWord(t, true, v, v.clock) {
				searched = append(searched, t)
			}
		}
	}
	for _, p := range v.pieces {
		add(p)
	}
	for _, x := range raw {
		if !x.Served && !x.At.Before(at) {
			continue
		}
		texts := []string{x.Text, dc.rawExtra(x.Text)}
		for _, text := range texts {
			if v.clock.whole && (containsWholeClock(text, recorded) || v.decoded && containsWholeClock(text, v.decRec)) {
				return Place{Mock: x.Mock}, true
			}
		}
		for _, w := range searched {
			if ContainsToken(texts[0], w) || ContainsToken(texts[1], w) {
				return Place{Mock: x.Mock}, true
			}
		}
	}
	return Place{}, false
}

// Echoes are the answer leaves that hold r's recorded value: a scalar equal
// to it (WholeEcho, a place it can be answered at), and every other place
// keploy cannot answer it at: a string or a key of the answer's JSON objects
// (Keys) holding it as a token, in any declared form (rawExtra: decoded,
// case-folded, inside base64), and an unnamed leaf equal to it.
func Echoes(dc *DriftCache, r Leaf, resp []Leaf) []Leaf {
	if r.Value == "" {
		return nil
	}
	if dc == nil {
		dc = NewDriftCache()
	}
	var out []Leaf
	for _, a := range resp {
		switch {
		case WholeEcho(r, a), a.Unnamed && a.Value == r.Value:
			out = append(out, a)
		case a.Type == String || a.Type == Keys:
			if ContainsToken(a.Value, r.Value) {
				out = append(out, a)
				continue
			}
			if ContainsToken(strings.ToLower(a.Value), strings.ToLower(r.Value)) {
				out = append(out, a)
				continue
			}
			if f := dc.rawExtra(a.Value); f != "" && (ContainsToken(f, r.Value) || ContainsToken(f, strings.ToLower(r.Value))) {
				out = append(out, a)
			}
		}
	}
	return out
}

// WholeEcho reports whether answer leaf a holds r's recorded value as its
// whole value, a string, a number or a boolean (an id sent as text may come
// back as a number): a place the value can be answered at.
func WholeEcho(r, a Leaf) bool {
	if a.Unnamed || r.Value == "" || a.Value != r.Value {
		return false
	}
	switch a.Type {
	case String, Number, Bool:
		return true
	}
	return false
}

// payloadKeys are the fields of a mock's spec that say where and when it was
// recorded, not what it carried: a value found in them did not flow from the
// mock.
var payloadKeys = map[string]bool{
	"metadata": true, "created": true, "reqtimestampmock": true, "restimestampmock": true,
	"reqbodynoise": true, "correlations": true, "variables": true, "timestamp": true, "async": true,
}

// RawOf is a mock as Classify reads it when no leaf names its values: its
// recorded request and answer, the payload only, as one text.
func RawOf(m *models.Mock, served bool) Raw {
	r := Raw{Mock: m.Name, At: m.Spec.ReqTimestampMock, Served: served}
	b, err := json.Marshal(m.Spec)
	if err != nil {
		return r
	}
	v, ok := DecodeJSON(b)
	if !ok {
		return r
	}
	// The payload's own text, not its JSON: a JSON string escapes what
	// separates a value from its neighbours ("\r\n" of a RESP frame), and a
	// value after an escape would be no token there.
	var sb strings.Builder
	textOf(dropKeys(v, 0), &sb)
	r.Text = sb.String()
	return r
}

// textOf writes v's keys and scalars to sb, each on a line of its own, in a
// stable order.
func textOf(v interface{}, sb *strings.Builder) {
	switch t := v.(type) {
	case map[string]interface{}:
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			sb.WriteString(k)
			sb.WriteByte('\n')
			textOf(t[k], sb)
		}
	case []interface{}:
		for _, child := range t {
			textOf(child, sb)
		}
	case string:
		sb.WriteString(t)
		sb.WriteByte('\n')
	case json.Number:
		sb.WriteString(t.String())
		sb.WriteByte('\n')
	case bool:
		sb.WriteString(strconv.FormatBool(t))
		sb.WriteByte('\n')
	}
}

// dropKeys is v without what payloadKeys names of the spec itself (depth
// 0) and the timestamps of its requests and answers (depth 1): never a key
// of the payload's own values (a jsonb document's "metadata" or "created").
func dropKeys(v interface{}, depth int) interface{} {
	switch t := v.(type) {
	case map[string]interface{}:
		out := make(map[string]interface{}, len(t))
		for k, child := range t {
			lk := strings.ToLower(k)
			if depth == 0 && payloadKeys[lk] || depth == 1 && lk == "timestamp" {
				continue
			}
			out[k] = dropKeys(child, depth+1)
		}
		return out
	case []interface{}:
		out := make([]interface{}, len(t))
		for i, child := range t {
			out[i] = dropKeys(child, depth+1)
		}
		return out
	}
	return v
}

// commaFraction is a decimal comma's fraction after a time (a Python or
// Log4j stamp, "10:00:41,123 "): milli-, micro- or nanoseconds, then a
// zone, a space or the end.
var commaFraction = regexp.MustCompile(`^,(?:\d{3}){1,3}(?:[ Z+-]|$)`)

// containsWholeClock reports whether text holds clock read v as it stands:
// as a token that no longer clock read of the text strictly holds (one with
// more precision, a zone, or a date around it is a clock read of its own
// form). The same value whole is a whole repeated value, followed (a
// refusal, where it cannot be bound); "1760436041,42", the next column of a
// row, is still the second whole.
func containsWholeClock(text, v string) bool {
	if v == "" {
		return false
	}
	var spans [][]int
	for _, lay := range clockLayouts {
		spans = append(spans, lay.re.FindAllStringIndex(text, -1)...)
	}
	for i := 0; i <= len(text)-len(v); {
		j := strings.Index(text[i:], v)
		if j < 0 {
			return false
		}
		at := i + j
		if tokenAt(text, v, at) {
			longer := false
			for _, sp := range spans {
				if sp[0] <= at && at+len(v) <= sp[1] && sp[1]-sp[0] > len(v) {
					// A comma after a datetime is as likely the next
					// field of a row as a decimal comma: it errs to
					// the whole value.
					if sp[0] == at && strings.HasPrefix(text[at+len(v):sp[1]], ",") && !commaFraction.MatchString(text[at+len(v):]) {
						continue
					}
					longer = true
					break
				}
			}
			if !longer {
				return true
			}
		}
		i = at + 1
	}
	return false
}

// escapeClock adds to set (clock words of a value) each raw word of s that
// is an escape's remnant glued to one ("3A41Z" of "%3A41Z", "n2026" of
// "\\n2026").
func escapeClock(s string, set map[string]bool) {
	hexDigit := func(c byte) bool { return c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F' }
	i := 0
	for _, seg := range segmentsBy(s, isWord) {
		w := seg.text
		if seg.word && i > 0 {
			switch {
			case s[i-1] == '%' && len(w) > 2 && hexDigit(w[0]) && hexDigit(w[1]) && set[w[2:]]:
				set[w] = true
			case s[i-1] == '\\' && len(w) > 1 && strings.IndexByte("nrtbf", w[0]) >= 0 && set[w[1:]]:
				set[w] = true
			}
		}
		i += len(w)
	}
}

// b64Holds reports whether the text d's recorded encoded runs decode to
// (a JWT's claims) holds the recorded value of o whole, as it stands
// (containsWholeClock).
func (d *valueDrift) b64Holds(o *valueDrift) bool {
	if d.encodedRec == "" {
		return false
	}
	return containsWholeClock(d.encodedRec, o.rec) || o.decoded && containsWholeClock(d.encodedRec, o.decRec)
}

// ReadBack is where a value of a recorded answer was written earlier in this
// run, by a write the read is linked to, with another value than its
// recording's: the request place, held whole, of the earliest exchange served
// before that marked it Written into record (Seen.Record), whose recorded
// value is r's and whose live value differs from it. record is the record
// the read names, as the adapter names it (the record its filter or its
// identity names); "" for a read the adapter links to no record, which reads
// nothing back.
//
// That is where the value was first stored, so every read of it, and every
// later write that carries it on, follows one variable. A call that answers
// with what an earlier call wrote (a time, a code the application made and
// stored) is then answered with what this run wrote there: r's place is an
// answer place of the variable bound at that earlier place.
//
// What decides is the evidence alone: the write drifted, the adapter marked
// the place Written, and the read names the record it wrote. A value is read
// back whatever its length, script or type (the request place's type is not
// compared with r's: an id stored as a number is read back as text), and a
// value no linked write stored is never read back, however it looks.
//
// Two fields of the record written with r's recorded value that drifted to
// two live values (each field's first write) are two values r may be: the
// evidence does not say which field r reads, so r is read back from neither,
// and its drift stays unexplained.
func ReadBack(r Leaf, record string, earlier []Earlier) (Place, bool) {
	if r.Unnamed || r.Value == "" || record == "" {
		return Place{}, false
	}
	served := append([]Earlier(nil), earlier...)
	sort.SliceStable(served, func(i, j int) bool { return served[i].At.Before(served[j].At) })
	var first Place
	found := false
	// The live value of each field's first write: a later write of a field
	// carries it on.
	firstLive := map[string]string{}
	for _, e := range served {
		for _, s := range e.Seen {
			if s.Side != Request || !s.Written || s.Record != record || s.Place == "" || s.Leaf.Value != r.Value || s.Live == s.Leaf.Value || s.Live == "" {
				continue
			}
			if _, ok := firstLive[s.Place]; !ok {
				firstLive[s.Place] = s.Live
			}
			if !found {
				first = Place{Mock: e.Mock, Side: Request, Path: s.Leaf.Path, Noise: s.Leaf.Noise,
					Var: s.Place, Live: s.Live, Whole: true, Own: e.Own, Run: e.Run, Name: s.Name}
				found = true
			}
		}
	}
	for _, live := range firstLive {
		if live != first.Live {
			return Place{}, false
		}
	}
	return first, found
}

// Named is e with each request place that holds one of vars (as its bind or
// a use) naming that variable (Seen.Name), the first by name when two hold
// one place. An echo is not named: it is its call's alone, and a later call
// that reads its value back binds it there as a variable of the test
// (ReadBack). e's own Seen is left as it was: the result holds a copy, with
// no name at any other place.
func Named(e Earlier, vars models.MockVariables) Earlier {
	names := make([]string, 0, len(vars))
	for name := range vars {
		names = append(names, name)
	}
	sort.Strings(names)
	at := map[string]string{}
	for _, name := range names {
		v := vars[name]
		if v.Echo {
			continue
		}
		if v.Bind != "" && at[v.Bind] == "" {
			at[v.Bind] = name
		}
		for _, u := range v.Use {
			if at[u] == "" {
				at[u] = name
			}
		}
	}
	e.Seen = append([]Seen(nil), e.Seen...)
	for i, s := range e.Seen {
		e.Seen[i].Name = ""
		if s.Side == Request && s.Place != "" {
			e.Seen[i].Name = at[s.Place]
		}
	}
	return e
}
