package learn

import (
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
	rec, live        string
	side             Side
	given, startUp   bool
	// gave: a value the test gave keploy (#5016), read by its text alone,
	// never by a moment; given without gave is the test case's request.
	gave bool
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
	// views: the texts of the views round F's serves gave where they
	// differ (Raw.ServedChanged); one that holds F's word leaves it unknown.
	fUnstated, fMay bool
	views           []string
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
		if e.Unread != "" {
			if unread == "" {
				unread = e.Mock + " (" + e.Unread + ")"
			}
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
				given: e.Given, gave: e.Gave, clock: s.Clock, startUp: startUp, otherTest: other, scope: scope,
				at: e.At, state: st, why: why,
			})
			if s.Side == Response && st == stateBefore && e.Payload.isRead() {
				p := &ps[len(ps)-1]
				switch a := e.Payload.answered; {
				case a.IsZero():
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
				unread = r.Mock() + " (keploy could not read its recording)"
			}
			continue
		}
		st, why := rawState(in, r)
		// F's text there: the view F served, or the recording where F
		// served it as recorded; else not stated, and it shows nothing.
		live, unstated := r.Text(), true
		var views []string
		switch {
		case r.live != nil:
			live, unstated = r.live.Text(), false
		case r.AsRecorded():
			unstated = false
		case r.unseen:
			for _, v := range r.views {
				views = append(views, v.Text())
			}
		}
		fUnstated := unstated || !in.Caps.FViewStated
		// Another test's recording, or one recorded after the call
		// (OfAnotherTest), is outside the call's scope.
		other := r.olderTest || r.elsewhere
		startUp, other, scope := inScope(in, r.Mock(), r.beforeTest && !other, other)
		ps = append(ps, evPlace{
			name: r.Mock(), mock: r.Mock(), rec: r.Text(), live: live, side: Response,
			startUp: startUp, otherTest: other, scope: scope,
			at: r.at, state: st, why: why, fUnstated: fUnstated, raw: i + 1,
			fMay: fUnstated && len(views) == 0 && r.served && !r.servedAfter, views: views,
		})
	}
	return ps, unread
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
	case r.noTime || r.answered.IsZero():
		return stateUnknown, noAnswerAt
	case r.answered.Equal(in.At) || r.at.Equal(in.At):
		return stateUnknown, atCallWhy
	case r.answered.After(in.At) || !r.served || r.servedAfter:
		return stateNotBefore, ""
	}
	return stateBefore, ""
}

// orderUnknown marks every candidate place unknown where the recording and
// round F gave the application their answers in another order, or either
// order cannot be told (follows.go's recordedOrder).
func orderUnknown(in Input, ps []evPlace, cand map[string]bool) {
	if len(cand) < 2 {
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

// Why a place's state in round F is unknown, as a refusal's line says it
// in parentheses.
const (
	swapNote   = "a connection start-up keploy may swap"
	atCallWhy  = "recorded at the call's own instant"
	noTimeWhy  = "recorded at a time keploy cannot read"
	noAnswerAt = "no answer time"
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
func swapUnknown(in Input, x *readerIndex, ps []evPlace, w readWord) {
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
		if p.raw == 0 || p.state != stateBefore || !evHolds(x, p.rec, w.Rec) {
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
func evidenceAt(x *readerIndex, p evPlace, r, f string) (ev wordEvidence, read bool) {
	hr, okR := x.holdsWord(p.rec, r)
	hf, okF := x.holdsWord(p.live, f)
	if !okR || !okF {
		return evNone, false
	}
	if p.fUnstated {
		if hr || hf || p.fMay {
			return evUnknown, true
		}
		for _, v := range p.views {
			if h, _ := x.holdsWord(v, f); h {
				return evUnknown, true
			}
		}
		return evNone, true
	}
	if p.rec == p.live {
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
	case nothingNew(p.rec, p.live):
		// F gave nothing new there: every word of F's text stood in R's.
		return evUnknown, true
	case hr && !strings.Contains(p.live, f):
		return evDisproven, true
	}
	return evUnknown, true
}

// countsByText reports whether a disproving place counts the word by its
// text: 32 bits or more (wordBits), alone or as a whole token around it
// the place also holds whole: an unchanged place that holds only one of the two words is chance.
func countsByText(x *readerIndex, p evPlace, w readWord) bool {
	if wordBits(w.Rec) >= 32 || wordBits(w.Live) >= 32 {
		return true
	}
	for _, t := range w.Tokens {
		if held, _ := x.holdsToken(p.rec, t.Kind, t.Rec); held && wordBits(t.Rec) >= 32 {
			return true
		}
		if held, _ := x.holdsToken(p.live, t.Kind, t.Live); held && wordBits(t.Live) >= 32 {
			return true
		}
	}
	return false
}

// heldWhole reports whether p, a place F moved, held the recorded value's
// whole token around w in R (a conflict in the call's scope
// or the start-up).
func heldWhole(x *readerIndex, p evPlace, w readWord, rec string) bool {
	if p.rec == p.live {
		return false
	}
	if held, _ := x.holdsToken(p.rec, tokenID, rec); held {
		return true
	}
	// A word that is a whole token of its own (no ID byte beside it).
	if len(w.Tokens) == 0 {
		if held, _ := x.holdsToken(p.rec, tokenID, w.Rec); held {
			return true
		}
	}
	for _, t := range w.Tokens {
		if held, _ := x.holdsToken(p.rec, t.Kind, t.Rec); held {
			return true
		}
	}
	return false
}

// nothingNew reports whether every word of live stands among rec's.
func nothingNew(rec, live string) bool {
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

// setWords are the words of each recording outside the call's scope that
// round F gave before it, read once for the call, as main's word rule reads
// them (offsetPlacesOf: the set's and raw's, each recording once).
func (dc *decider) setWords() []givenWords {
	if !dc.swDone {
		dc.swDone = true
		elsewhere := len(dc.set) > 0
		for _, r := range dc.in.Raw {
			elsewhere = elsewhere || r.elsewhere
		}
		if elsewhere {
			d := dc.in.Drifts
			if d == nil {
				d = NewDriftCache()
			}
			dc.sw = d.offsetPlacesOf(dc.in.At, dc.in.Earlier, dc.in.Raw, dc.set).words
		}
	}
	return dc.sw
}

// setEvidence is what g shows of the changed word r (R) to f (F), and
// whether round F changed it: the evidence table, read by words. Its
// recorded words count for R only where it was recorded before the call.
func setEvidence(g givenWords, r, f string, at time.Time) (ev wordEvidence, changed bool) {
	hr := listsHold(g.answerLists, r)
	before := !g.at.After(at)
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
	// Not served: round F gave nothing there (B4: never a source).
	if hr {
		return evUnknown, false
	}
	return evNone, false
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
			if !g.answerLists.holds(i, t, true) {
				return true
			}
		}
	}
	return false
}
