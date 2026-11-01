package learn

import (
	"cmp"
	"strconv"
	"strings"
	"time"

	"go.keploy.io/server/v3/pkg/variables"
)

// Evidence at a place. A place is something the application
// sent or got that may come before the call: a request it sent, an answer,
// the test case's request (a given). Each run's state there is before (sent
// or answered before the call reached keploy), not before, or unknown. Only
// a place before in both runs can show or disprove a changed word; any
// other place that holds it leaves it unknown, and an unknown is refused,
// never fresh.

// placeState is one place's state in both runs.
type placeState uint8

const (
	// stateBefore: before the call in both runs.
	stateBefore placeState = iota + 1
	// stateNotBefore: later than the call, or never, in a run.
	stateNotBefore
	// stateUnknown: keploy cannot tell (no answer time, the call's own
	// instant, served after the call arrived, another order).
	stateUnknown
)

// evPlace is one place a changed word may come from: where it stands
// ("mock-N path", its line name), its text in each run, its scope, its
// state and, where unknown, why.
type evPlace struct {
	name, mock, path string
	// rec and live are a leaf's value in each run. A recording's place
	// keeps its Raws instead (src: the recording; fSrc: what round F gave
	// there, its view or src), never their texts. t is how the call's index
	// reads each run's text (withTexts): a handle per text, so a question
	// reads a recording only where the index has not answered it yet.
	rec, live      string
	src, fSrc      *Raw
	t              *placeText
	side           Side
	given, startUp bool
	// gave: a value the test gave keploy (#5016), read by its text alone,
	// never by a moment; given without gave is the test case's request.
	gave bool
	// undeclared: a body of the test case's request that declares no media
	// type, read by its bytes (Seen.Undeclared): a word glued to the bytes
	// around it there may be one, and shows nothing (gluedAt).
	undeclared bool
	// clock: the earlier call learned this place as its own read of the
	// application's clock: a separate read, never a copy of another.
	clock bool
	// raw is the index in Input.Raw plus one of a recording's place; 0 for
	// any other.
	raw int
	// otherTest: an earlier test's place, or one served between tests:
	// outside the call's scope (Mode Own reads it only to name the edit).
	// scope names that scope for a line where mappings.yaml names it
	// ("worker:", a test's name); "" where the place keeps its marks.
	otherTest bool
	scope     string
	// leaf: an earlier exchange's leaf, which names its place (FromAt).
	leaf  Leaf
	at    time.Time
	state placeState
	why   string
	// fUnstated: the adapter does not state what round F's answer held
	// here, so its F side shows and disproves nothing. fMay: round F served
	// it before the call and no view of what it gave there is stated or
	// read, so it may hold round F's new word: unknown for every word.
	// views: the views round F's serves gave where they differ
	// (Raw.ServedChanged); one that holds F's word leaves it unknown.
	fUnstated, fMay bool
	views           []*Raw
	// fGave: round F gave the application this place before the call
	// reached keploy, whenever it was recorded.
	fGave bool
}

// placeText is one place's text in each run, and each view round F served
// there, as the call's index reads them (readerIndex.textOf for a
// recording): made once per place and shared by its copies, so the index
// finds each text's entry once, and the place keeps no recording's text.
// same: whether round F gave the place as recorded (0: not asked yet).
type placeText struct {
	rec, live *readerText
	views     []*readerText
	same      int8
}

// withTexts gives each of ps the handles its texts are read by, in x.
func withTexts(x *readerIndex, ps []evPlace) []evPlace {
	for i := range ps {
		p := &ps[i]
		t := &placeText{rec: x.text(p.rec), live: x.text(p.live)}
		if p.src != nil {
			t.rec = x.textOf(*p.src)
			t.live = t.rec
			if p.fSrc != p.src {
				t.live = x.textOf(*p.fSrc)
			}
		}
		for _, v := range p.views {
			t.views = append(t.views, x.textOf(*v))
		}
		p.t = t
	}
	return ps
}

// liveText is round F's text at p, read now where p is a recording; "" where
// keploy could not read it.
func (p evPlace) liveText() string {
	if p.t == nil {
		return p.live
	}
	s, _ := p.t.live.textNow()
	return s
}

// same reports whether round F gave p as recorded; a text keploy could not
// read is not (and the index refuses the call: fail closed).
func (p evPlace) same() bool {
	switch {
	case p.src != nil && p.src == p.fSrc:
		return true
	case p.t == nil:
		return p.rec == p.live
	case p.t.same == 0:
		r, okR := p.t.rec.textNow()
		l, okL := p.t.live.textNow()
		p.t.same = -1
		if okR && okL && r == l {
			p.t.same = 1
		}
	}
	return p.t.same > 0
}

// wordEvidence is what one place shows of a changed word.
type wordEvidence uint8

const (
	evNone      wordEvidence = iota // not a candidate: holds neither word
	evShown                         // a source
	evDisproven                     // a conflict or chance (evConflict decides)
	evUnknown
)

// placesOf lists in's places before the call and each one's state, and the
// first place keploy could not read ("" where it read every one): every
// change of the call is then refused.
func placesOf(in Input) (ps []evPlace, unread string) {
	for _, e := range in.Earlier {
		// An exchange keploy could not read (Earlier.Unread), or one F
		// served with no leaf and no payload keploy read, is a place it
		// cannot search, never one it drops.
		bad := e.Unread
		if bad == "" && len(e.Seen) == 0 && !e.Given && !e.Payload.Read() {
			bad = cmp.Or(e.Payload.unreadWhy(), "it was not read from its mock, learn.RawOf")
			if e.Payload.mock == "" {
				bad = "keploy was given no payload of it, learn.Earlier.Payload"
			}
		}
		if bad != "" {
			unread = cmp.Or(unread, unreadName(e.Mock, e)+" ("+bad+")")
			continue
		}
		st, why := earlierState(in, e)
		startUp, other, scope := inScope(in, e.Mock, e.StartUp, e.ofEarlierTest())
		for _, s := range e.Seen {
			path := s.Leaf.Path
			if path == "" {
				path = s.Place
			}
			ps = append(ps, evPlace{
				name: e.Mock + " " + path, mock: e.Mock, path: path,
				rec: s.Leaf.Value, live: s.Live, side: s.Side, leaf: s.Leaf,
				given: e.Given, gave: e.Gave, undeclared: s.Undeclared, clock: s.Clock, startUp: startUp, otherTest: other, scope: scope,
				at: e.At, state: st, why: why, fGave: !e.Payload.servedAfter,
			})
			if s.Side == Response && st == stateBefore && e.Payload.isRead() {
				p := &ps[len(ps)-1]
				switch a := e.Payload.answered; {
				case untimedAnswer(e.At, a):
					p.state, p.why = stateUnknown, noAnswerAt
				case a.Equal(in.At):
					p.state, p.why = stateUnknown, atCallWhy
				case a.After(in.At):
					p.state = stateNotBefore
				}
			}
		}
	}
	for i, r := range in.Raw {
		if !r.Read() {
			if unread == "" {
				why := r.unreadWhy()
				if why == "" {
					why = "it was not read from its mock, learn.RawOf"
				}
				unread = unreadName(r.Mock(), Earlier{}) + " (" + why + ")"
			}
			continue
		}
		st, why := rawState(in, r)
		// F's text there: the view F served, or the recording where F
		// served it as recorded; else not stated, and it shows nothing.
		src := &in.Raw[i]
		live, unstated := src, true
		var views []*Raw
		switch {
		case r.live != nil:
			live, unstated = r.live, false
		case r.AsRecorded():
			unstated = false
		case r.unseen:
			views = r.views
		}
		fUnstated := unstated || !in.Caps.FViewStated
		// Another test's recording, or one recorded after the call
		// (OfAnotherTest), is outside the call's scope.
		other := r.olderTest || r.elsewhere
		startUp, other, scope := inScope(in, r.Mock(), r.beforeTest && !other, other)
		ps = append(ps, evPlace{
			name: r.Mock(), mock: r.Mock(), src: src, fSrc: live, side: Response,
			startUp: startUp, otherTest: other, scope: scope,
			at: r.at, state: st, why: why, fUnstated: fUnstated, raw: i + 1,
			fMay: fUnstated && len(views) == 0 && r.served && !r.servedAfter, views: views,
			fGave: r.served && !r.servedAfter,
		})
	}
	return ps, unread
}

// unreadName names a place keploy could not read in its refusal: its mock,
// else what it is (the run's values, the test case's request, a
// recording of the test), never an empty name.
func unreadName(mock string, e Earlier) string {
	switch {
	case mock != "":
		return mock
	case e.Run:
		return "the run's values"
	case e.Given:
		return "the test case's request"
	}
	return "a recording of the test"
}

// inScope is a place's scope: mappings.yaml's (Input.Scope) where it names
// mock in one place, else where mock was recorded (startUp, other). This
// test's own list and startup: are the call's scope; worker:, another test's
// list or any other section are outside it, and scope names it for the
// line ("worker:", the other test's name). keploy never decides the scope:
// it reads mappings.yaml and names the edit.
func inScope(in Input, mock string, startUp, other bool) (bool, bool, string) {
	switch s := in.ScopeOf(mock); s {
	case ScopeUnknown:
		return startUp, other, ""
	case ScopeStartup:
		return true, false, ""
	case ScopeTest(in.Test):
		return false, false, ""
	default:
		return false, true, strings.TrimPrefix(string(s), "test ")
	}
}

// earlierState is the state of a place F served earlier: a given and an
// earlier test's place are before in both runs; a mock recorded before the
// call is, one recorded after it is not, and one with no time or at the
// call's own instant is unknown; one F served only after the call reached
// keploy is not before in F.
func earlierState(in Input, e Earlier) (placeState, string) {
	switch {
	case e.alwaysEarlier():
		return stateBefore, ""
	case e.noTime:
		return stateUnknown, noTimeWhy
	case e.Payload.servedAfter:
		return stateNotBefore, ""
	case e.At.Equal(in.At):
		return stateUnknown, atCallWhy
	case e.At.After(in.At):
		return stateNotBefore, ""
	}
	return stateBefore, ""
}

// rawState is a recording's state: answered before the call in R, and given
// the application by F before the call reached keploy.
func rawState(in Input, r Raw) (placeState, string) {
	switch {
	case r.noTime || untimedAnswer(r.at, r.answered):
		return stateUnknown, noAnswerAt
	case r.answered.Equal(in.At) || r.at.Equal(in.At):
		return stateUnknown, atCallWhy
	case r.answered.After(in.At) || !r.served || r.servedAfter:
		return stateNotBefore, ""
	}
	return stateBefore, ""
}

// untimedAnswer: a recording's answer time keploy cannot read, none or one
// before its own request was recorded (follows.go's recordedGave), so it
// cannot place the answer before the call.
func untimedAnswer(req, answered time.Time) bool {
	return answered.IsZero() || answered.Before(req)
}

// orderUnknown marks every candidate place unknown where the recording and
// round F gave the application their answers in another order, or either
// order cannot be told (follows.go's recordedOrder). One candidate has an
// order only where F served it more than once: the recording gave its one
// answer once, so an application copying by position may have taken
// another serve of it in F.
func orderUnknown(in Input, ps []evPlace, cand map[string]bool) {
	if len(cand) == 0 || len(cand) == 1 && !servedAgain(in, cand) {
		return
	}
	source := func(mock string) bool { return cand[mock] }
	rec, untoldRec := recordedOrder(in.At, in.Earlier, in.Raw, source)
	f, untoldF := servedOrder(in.Earlier, in.Raw, source)
	why := untoldRec
	if why == "" {
		why = untoldF
	}
	if why == "" {
		if d := orderDiff(rec, f); d != "" {
			why = "another order: " + d
		}
	}
	if why == "" {
		return
	}
	for i := range ps {
		if cand[ps[i].mock] && ps[i].state == stateBefore {
			ps[i].state, ps[i].why = stateUnknown, why
		}
	}
}

// servedAgain reports whether round F served a candidate of cand at two
// numbered serves (servedOrder's span).
func servedAgain(in Input, cand map[string]bool) bool {
	for _, e := range in.Earlier {
		if cand[e.Mock] && (e.Payload.Read() || e.Payload.ordered) && e.Payload.last != e.Payload.first {
			return true
		}
	}
	for _, x := range in.Raw {
		if cand[x.mock] && x.served && x.last != x.first {
			return true
		}
	}
	return false
}

// Why a place's state in round F is unknown, as a refusal's line says it
// in parentheses.
const (
	swapNote   = "a connection start-up keploy may swap"
	atCallWhy  = "recorded at the call's own instant"
	noTimeWhy  = "recorded at a time keploy cannot read"
	noAnswerAt = "no answer time keploy can read"
)

// swapUnknown marks unknown each place of ps, before the call in both runs,
// that is a connection's recorded start-up whose field a swap of start-ups
// can change (a backend pid, a secret key, a server parameter set per pool)
// and holds the recorded word w or a whole token around it (Raw's
// swapHolds). keploy gives each connection a recorded start-up by the order
// the connections open in, not by what each sent: round F's connection may
// have got another's, so keploy cannot tell what F gave there. A field every
// start-up carries alike is no swap's, and reads as any recording's. Only
// a place whose recorded text holds the word is asked: swapHolds reads a
// match it finds in no one field as a swap's.
func swapUnknown(in Input, ps []evPlace, w readWord) {
	holds := func(own string) bool {
		if holdsToken(own, w.Rec) {
			return true
		}
		for _, t := range w.Tokens {
			if holdsToken(own, t.Rec) {
				return true
			}
		}
		return false
	}
	for i := range ps {
		p := &ps[i]
		if p.raw == 0 || p.state != stateBefore || !evHolds(p.t.rec, w.Rec) {
			continue
		}
		if r := in.Raw[p.raw-1]; r.swapHolds(r.payload().text, in.Raw, holds) {
			p.state, p.why = stateUnknown, swapNote
		}
	}
}

// evidenceAt is what p shows of the changed word r (R) to f (F): the
// evidence table, read through x. read is false where p's text lay
// past the call's budget.
func evidenceAt(p evPlace, r, f string) (ev wordEvidence, read bool) {
	hr, okR := p.t.rec.holdsWord(r)
	hf, okF := p.t.live.holdsWord(f)
	if !okR || !okF {
		return evNone, false
	}
	if p.fUnstated {
		if hr || hf || p.fMay {
			return evUnknown, true
		}
		for _, v := range p.t.views {
			if h, _ := v.holdsWord(f); h {
				return evUnknown, true
			}
		}
		return evNone, true
	}
	if p.same() {
		// Unchanged: one text holds R's word, F's word, both or neither.
		if !hr && !hf {
			return evNone, true
		}
		if p.state != stateBefore {
			return evUnknown, true
		}
		if hr && hf {
			return evUnknown, true
		}
		return evDisproven, true
	}
	if !hr && !hf {
		return evNone, true
	}
	if p.state != stateBefore {
		return evUnknown, true
	}
	switch {
	case hr && hf:
		return evShown, true
	case nothingNew(p):
		// F gave nothing new there: every word of F's text stood in R's.
		return evUnknown, true
	case hr && !strings.Contains(p.liveText(), f):
		return evDisproven, true
	}
	return evUnknown, true
}

// movedField is why p does not show r and f in both runs though its texts
// hold them: no one field holds r in the recording and f in round F's view
// (inOneField), so F gave the word at another field than the recorded one
// (main reads a recording field by field). "" where one field holds both.
func movedField(p evPlace, r, f string) string {
	same, read := inOneField(p.t.rec, r, p.t.live, f)
	at, _ := p.t.live.fieldsWith(f)
	switch {
	case !read:
		return "past the call's budget"
	case same:
		return ""
	case len(at) == 0:
		return "its view holds " + strconv.Quote(f) + " at no one field"
	}
	return "its view gave " + strconv.Quote(f) + " at " + at[0].path + ", not at a field whose recorded text holds " + strconv.Quote(r)
}

// countsByText reports whether a disproving place counts the word by its
// text: 32 bits or more (wordBits), alone or as a whole token around it
// the place also holds whole: an unchanged place that holds only one of the two words is chance.
func countsByText(p evPlace, w readWord) bool {
	if wordBits(w.Rec) >= 32 || wordBits(w.Live) >= 32 {
		return true
	}
	for _, t := range w.Tokens {
		if held, _ := p.t.rec.holdsToken(t.Kind, t.Rec); held && wordBits(t.Rec) >= 32 {
			return true
		}
		if held, _ := p.t.live.holdsToken(t.Kind, t.Live); held && wordBits(t.Live) >= 32 {
			return true
		}
	}
	return false
}

// heldWhole reports whether p, a place F moved, held the recorded value's
// whole token around w in R (a conflict in the call's scope
// or the start-up).
func heldWhole(p evPlace, w readWord, rec string) bool {
	if p.same() {
		return false
	}
	if held, _ := p.t.rec.holdsToken(tokenID, rec); held {
		return true
	}
	// A word that is a whole token of its own (no ID byte beside it).
	if len(w.Tokens) == 0 {
		if held, _ := p.t.rec.holdsToken(tokenID, w.Rec); held {
			return true
		}
	}
	for _, t := range w.Tokens {
		if held, _ := p.t.rec.holdsToken(t.Kind, t.Rec); held {
			return true
		}
	}
	return false
}

// nothingNew reports whether every word of round F's text at p stands among
// the recording's: true where keploy could not read one (it shows nothing).
func nothingNew(p evPlace) bool {
	rec, okR := p.t.rec.textNow()
	live, okL := p.t.live.textNow()
	if !okR || !okL {
		return true
	}
	had := map[string]bool{}
	for _, w := range variables.Words(rec) {
		had[rec[w[0]:w[1]]] = true
	}
	for _, w := range variables.Words(live) {
		if !had[live[w[0]:w[1]]] {
			return false
		}
	}
	return true
}

// The recordings round F gave the application before the call that are
// outside its scope: another test's, or one recorded after the call
// (Raw.OfAnotherTest), and those the set read once (Input.Given). keploy
// keeps their answers' words, not their text, so each is judged by its
// words, as the evidence table judges a place. In Mode Own each lies
// outside the call's scope: a word it shows is refused naming the edit.

// setUnread names the first of the set's recordings keploy could not read
// back ("" where it read every one, or the call has none): every change of
// the call is then refused.
func setUnread(in Input) ([]GivenPlaces, string) {
	if in.Given == nil {
		return nil, ""
	}
	set, err := in.Given()
	if err != nil {
		return nil, "the recordings round F gave earlier in the test set: " + err.Error()
	}
	for _, g := range set {
		if !g.read {
			return nil, g.mock + ", a recording round F gave earlier in the test set"
		}
	}
	return set, ""
}

// outside are the recordings outside the call's scope that round F gave
// before it, read once for the call, as main's word rule and echo read them
// (offsetPlacesOf: the set's and raw's, each recording once): their
// answers' words and instants. nil where the call has none.
func (dc *decider) outside() *offsetPlaces {
	if !dc.opDone {
		dc.opDone = true
		elsewhere := len(dc.set) > 0
		for _, r := range dc.in.Raw {
			elsewhere = elsewhere || r.elsewhere
		}
		if elsewhere {
			dc.op = dc.drifts().offsetPlacesOf(dc.in.At, dc.in.Earlier, dc.in.Raw, dc.set)
		}
	}
	return dc.op
}

// setWords are the words of each recording outside the call's scope that
// round F gave before it (outside).
func (dc *decider) setWords() []givenWords {
	if op := dc.outside(); op != nil {
		return op.words
	}
	return nil
}

// drifts is the call's drift cache, or an empty one.
func (dc *decider) drifts() *DriftCache {
	if dc.in.Drifts != nil {
		return dc.in.Drifts
	}
	return NewDriftCache()
}

// setSecondSource names a recording of the set (Input.Given), which round
// F gave the application in an earlier test, that may be where the moment
// of c came from beside an answer of the call's scope, and says how it
// stands there: one recorded at or after the call, or at a time keploy
// cannot read, whose answer as F gave it (its view, else as recorded: the
// fail-closed side where the view is not stated) names F's moment by a cut
// that does not name R's; one recorded at the call's own instant, or at no
// readable time, whose answer names R's moment so (R may have had it before
// the call); and one recorded before the call whose view in round F keploy
// cannot tell, whose answer names R's moment so. "" where none does.
func (dc *decider) setSecondSource(c changedInstant) (mock, why string) {
	if c.rm == nil || c.lm == nil {
		return "", ""
	}
	names := func(ms []givenMoment, m, other moment) bool {
		for _, v := range ms {
			if namesMomentApart(v.m, m, other, nil) {
				return true
			}
		}
		return false
	}
	for _, g := range dc.set {
		atCall := g.at.IsZero() || g.words.noTime || g.at.Equal(dc.in.At)
		when := "after it"
		if atCall {
			when = "at its own instant or at a time keploy cannot read"
		}
		ms := g.fView
		if len(ms) == 0 {
			ms = g.answer
		}
		switch {
		case !atCall && g.at.Before(dc.in.At):
			if (g.words.unstated || g.words.unread) && names(g.answer, *c.rm, *c.lm) {
				return g.mock, "the recorded moment stands in " + g.mock + ", another test's answer given before the call, whose view in round F keploy cannot tell"
			}
		case names(ms, *c.lm, *c.rm):
			return g.mock, "round F's moment stands in " + g.mock + ", which round F gave before the call but which was recorded " + when
		case atCall && names(g.answer, *c.rm, *c.lm):
			return g.mock, "the recorded moment stands in " + g.mock + ", recorded " + when + ", which the recording may have given before the call"
		}
	}
	return "", ""
}

// echoedOutside names the recording outside the call's scope whose answer,
// as round F gave it before the call, holds the changed instant c: round F's
// moment, or the recorded one where it was recorded before the call, by a
// cut that does not name the other run's too. ok where main's echo of those
// places (echoedElsewhere) refuses c, or where round F's view of a recording
// of the set (GivenPlaces.fView: what F's serve of it was told to give)
// holds F's moment so; mock "" where no one recording names it alone. An
// instant there is a copy of what F gave the application, or a pick from
// it, never a read of the clock.
func (dc *decider) echoedOutside(c changedInstant) (mock string, ok bool) {
	if c.rm == nil || c.lm == nil {
		return "", false
	}
	for _, g := range dc.set {
		for _, v := range g.fView {
			if namesMomentApart(v.m, *c.lm, *c.rm, nil) {
				return g.mock, true
			}
		}
	}
	op := dc.outside()
	if op == nil || len(op.answers) == 0 {
		return "", false
	}
	pair := []driftedInstant{{rec: *c.rm, live: *c.lm}}
	if dc.drifts().echoedElsewhere(pair, dc.in.At, dc.in.Earlier, dc.in.Raw, func() *offsetPlaces { return op }) == "" {
		return "", false
	}
	for _, g := range op.answers {
		if namesMomentApart(g.m, *c.lm, *c.rm, nil) || !g.at.After(dc.in.At) && namesMomentApart(g.m, *c.rm, *c.lm, nil) {
			return g.mock, true
		}
	}
	return "", true
}

// setEvidence is what g shows of the changed word r (R) to f (F), and
// whether round F changed it: the evidence table, read by words. Its
// recorded words count for R only where it was recorded before the call.
// It shows or disproves only where it is before the call in both runs, as a
// recording handed to the call is (rawState): one with no answer time, or
// recorded or answered at the call's own instant, is unknown there.
func setEvidence(g givenWords, r, f string, at time.Time) (ev wordEvidence, changed bool) {
	// A recording made after the call gave R's application nothing before
	// it: R's word there decides nothing (F's still does).
	hr := listsHold(g.answerLists, r) && (g.noTime || !g.at.After(at))
	before := g.at.Before(at) && !g.noTime && !untimedAnswer(g.at, g.answered) && g.answered.Before(at)
	switch {
	case g.unstated || g.unread:
		// Round F served it and keploy cannot tell what it gave there.
		return evUnknown, true
	case len(g.views) > 0:
		for _, v := range g.views {
			if listsHold(v, f) {
				return evUnknown, true
			}
		}
		if hr {
			return evUnknown, true
		}
		return evNone, true
	case len(g.view.words) > 0:
		hf := listsHold(g.view, f)
		switch {
		case !hr && !hf:
			return evNone, true
		case !before || !viewNew(g):
			return evUnknown, true
		case hr && hf:
			return evShown, true
		case hr:
			return evDisproven, true
		}
		return evUnknown, true
	case len(g.same) > 0:
		// Served as recorded: unchanged.
		hf := listsHold(g.answerLists, f)
		switch {
		case !hr && !hf:
			return evNone, false
		case !before || hr && hf:
			return evUnknown, false
		}
		return evDisproven, false
	}
	// Not served: round F gave nothing there (never a source).
	if hr {
		return evUnknown, false
	}
	return evNone, false
}

// gluedAt reports whether p, a body that declares no media type, holds R's
// or F's word glued to the bytes around it, in any form it declares (main's
// gluedIn): keploy cannot tell its words from the bytes that frame them.
func (dc *decider) gluedAt(p evPlace, w readWord) bool {
	if !p.undeclared {
		return false
	}
	ed := dc.drifts().of(p.rec, p.live)
	return gluedIn(ed, w.Rec, true) || gluedIn(ed, w.Live, false)
}

// setUnknownWhy is why a recording of the set shows nothing of a word: its
// time, where keploy cannot place it before the call (rawState), else that
// keploy keeps only its words.
func setUnknownWhy(g givenWords, at time.Time) string {
	switch {
	case g.noTime || untimedAnswer(g.at, g.answered):
		return noAnswerAt
	case g.at.Equal(at) || g.answered.Equal(at):
		return atCallWhy
	case g.at.After(at) || g.answered.After(at):
		return "the recording did not give the application that word before the call"
	}
	return "round F gave it there; keploy keeps its words, and cannot tell this word did not come from it"
}

// listsHold reports whether l holds the word t in any reading, stamps read
// as any text (the side that finds more, so fails closed).
func listsHold(l answerLists, t string) bool {
	for i := 0; i < len(l.words) || i < len(l.stamped); i++ {
		if l.holds(i, t, true) {
			return true
		}
	}
	return false
}

// viewNew reports whether round F's view of g holds a word its recording
// does not: F gave something new there.
func viewNew(g givenWords) bool {
	for i, list := range g.view.words {
		for _, t := range list {
			if !g.holds(i, t, true) {
				return true
			}
		}
	}
	return false
}

// BoundWord is a variable of a test bound at one word of an earlier request
// leaf: the word's index and its text in each run.
type BoundWord struct {
	Name           string
	Word           int
	Recorded, Live string
}

// BoundWords is each variable of test number n bound at a word of s, a
// request leaf of mock, in word order: Name and RunName keep such a variable
// under "<place>#<word>", which Named, reading whole places, never reaches.
// An adapter answers a later answer leaf that holds s's recorded text whole
// at the same words. Two runs' texts of different word counts name none: no
// word of one is known to be the other's, and the answer stays as recorded.
func (t *Tracker) BoundWords(n uint64, mock string, s Seen) []BoundWord {
	if t == nil || s.Side != Request || s.Place == "" {
		return nil
	}
	rec, live := variables.Words(s.Leaf.Value), variables.Words(s.Live)
	if len(rec) != len(live) {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	var out []BoundWord
	for k := range rec {
		key := mock + "\x00" + s.Place + "#" + strconv.Itoa(k)
		name, ok := "", false
		if n >= t.setEpoch {
			name, ok = t.runNames[key]
		}
		if !ok && t.epoch == n {
			name, ok = t.names[key]
		}
		if ok {
			out = append(out, BoundWord{Name: name, Word: k, Recorded: s.Leaf.Value[rec[k][0]:rec[k][1]], Live: s.Live[live[k][0]:live[k][1]]})
		}
	}
	return out
}

// RecBegan is the instant the running test began at in its recording
// (RecordedBegin), zero where none was given: Input.RecBegan.
func (t *Tracker) RecBegan() time.Time {
	if t == nil {
		return time.Time{}
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.recBegan
}
