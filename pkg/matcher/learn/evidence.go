package learn

import (
	"cmp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"
	"weak"

	"go.keploy.io/server/v3/pkg/models"
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
	// undeclared: a body of the test case's request that declares no media
	// type, read by its bytes (Seen.Undeclared): a word glued to the bytes
	// around it there may be one, and shows nothing (gluedAt).
	// clock: the earlier call learned this place as its own read of the
	// application's clock: a separate read, never a copy of another.
	gave, undeclared, clock bool
	// offset: the read of the clock, with its offset, the earlier call
	// learned this place as (Seen.Offset); nil where it learned none.
	offset *models.ClockOffset
	// raw is the index in Input.Raw plus one of a recording's place; 0 for
	// any other.
	raw int
	// otherTest: an earlier test's place, or one served between tests:
	// outside the call's scope (Mode Own reads it only to name the edit).
	// scope names that scope for a line where mappings.yaml names it
	// ("worker:", a test's name); "" where the place keeps its marks.
	otherTest bool
	// trailed: a place the test the call trails left the run (Input.Trails):
	// in the call's scope, and a value bound there is the run's variable.
	trailed bool
	// outsideTest: why a place F gave the application only before the
	// call's test began says nothing of what the application did with a
	// value since ("" of a place of the call's own test), as main's
	// late-call rule reads it (outsideTest, Earlier.outsideTest): a
	// recording F gave only in an older test of the set than the call's
	// (Raw.InAnOlderTest) or only before the test, at the set's start-up or
	// between tests (Raw.BeforeTheTest); or an exchange F served only
	// before it (Earlier.beforeTest). No whole word is set aside there (a
	// founder ruling, 2026-11-02); a piece of a longer value still is, as
	// chance. lateCall: why a request an older test's call left the run
	// (Earlier.ofEarlierTest) may be a late call's: a whole word there is
	// set aside only where the value that lacks it took no word of another
	// place (lateCallTook), as main's olderTook reads it.
	outsideTest, lateCall string
	scope                 string
	// leaf: an earlier exchange's leaf, which names its place (FromAt).
	leaf  Leaf
	at    time.Time
	state placeState
	why   string
	// fUnstated: the adapter does not state what round F's answer held
	// here, so its F side shows and disproves nothing. fMay: round F served
	// it before the call and no view of what it gave there is stated or
	// read, so it may hold round F's new word: unknown for every word.
	// fGave: round F gave the application this place before the call
	// reached keploy, whenever it was recorded. recAfter: answered after
	// the call (answerState), so its answer gave R's application nothing
	// before it; R's word there decides nothing (setEvidence's rule for a
	// set entry's answer). A word its request holds is placed by the
	// request's time instead (req), as an exchange's request leaf is.
	// views: the views round F's serves gave where they differ
	// (Raw.ServedChanged); one that holds F's word leaves it unknown.
	// rBefore: the recording gave the application this place before the
	// call, by times keploy reads (its answer's, answerState, where it holds
	// one): R's application had it to cut a value from. Unlike recAfter, a
	// place with no time keploy reads, or at the call's own instant, is not.
	fUnstated, fMay, fGave, recAfter, rBefore bool
	views                                     []*Raw
	// req: a recording's state by its request's time, where that is not
	// its state by its answer's (rawRequestState): a word its request
	// holds is placed so (placedFor), as an exchange's request leaf is.
	req *reqPlace
}

// reqPlace is a place's state, why and recAfter by its request's time.
type reqPlace struct {
	state placeState
	why   string
	after bool
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
	// One block for the places' handles: one allocation per call, not one
	// per place.
	ts := make([]placeText, len(ps))
	for i := range ps {
		p, t := &ps[i], &ts[i]
		if p.src == nil {
			t.rec, t.live = x.text(p.rec), x.text(p.live)
		} else {
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

// payloadless is the first exchange of in served leaf by leaf whose
// payload keploy was not given or could not read (Earlier.Payload), and
// main's reason (payloadUnread): its answer's places cannot be searched.
// The test case's own request is no exchange, and an earlier test's place
// keeps no payload by design (ranCalls). "" where there is none.
func payloadless(in Input) (mock, why string) {
	for _, e := range in.Earlier {
		if !e.Given && !e.ofEarlierTest() && !e.Payload.Read() {
			return unreadName(e.Mock, e), payloadUnread(e)
		}
	}
	return "", ""
}

// placesOf lists in's places before the call and each one's state, and the
// first place keploy could not read ("" where it read every one): every
// change of the call is then refused.
//
// byWords are the client tests' requests keploy could not read that keep
// their words (Earlier.unreadWords): each is unread for a value whose change
// it may hold (mayHold), and no source of any other (decider.leaf).
func placesOf(in Input) (ps []evPlace, unread string, byWords []Earlier) {
	// Sized once: a call of a long test has thousands of places, and grown
	// by append the list was copied at each growth (a quarter more each
	// time past 256), about five times its size allocated per call.
	n := len(in.Raw)
	for _, e := range in.Earlier {
		n += len(e.Seen)
	}
	ps = make([]evPlace, 0, n)
	for _, e := range in.Earlier {
		// An exchange keploy could not read (Earlier.Unread), or one F
		// served with no leaf and no payload keploy read, is a place it
		// cannot search, never one it drops.
		bad := e.Unread
		if bad == "" && len(e.Seen) == 0 && !e.Given && !e.Payload.Read() {
			bad = cmp.Or(e.Payload.unreadWhy(), notFromMock)
			if e.Payload.mock == "" {
				bad = "keploy was given no payload of it, learn.Earlier.Payload"
			}
		}
		if bad != "" {
			if e.unreadWords != nil {
				byWords = append(byWords, e)
				continue
			}
			unread = cmp.Or(unread, unreadName(e.Mock, e)+" ("+bad+")")
			continue
		}
		st, why := earlierState(in, e)
		startUp, other, scope := inScope(in, e.Mock, e.StartUp, e.ofEarlierTest())
		// A place the test the call trails left the run is the call's own
		// (Input.Trails), and so is one that test was served in its own
		// window where the call lands in it too (the application answered
		// the test before its clean-up ran, but the round ran it sooner):
		// bound there, a value is the run's variable, so a replay binds it
		// in whichever window the call lands.
		trailed := in.trails(e.Mock) && !startUp && (other && e.ofEarlierTest() || !other && in.Test == in.Trails.Test)
		if trailed {
			other = false
		}
		// Of the exchange, not of a leaf: built once for its leaves.
		outsideTest, lateCall := earlierOutsideTest(e), earlierLateCall(e)
		// rBefore of its leaves: a request leaf by the request's time, an
		// answer leaf by the answer's (none where keploy was given no
		// payload: the zero time).
		sentBefore := e.alwaysEarlier() || !e.noTime && e.At.Before(in.At)
		answeredBefore := e.alwaysEarlier() || sentBefore && answeredBy(e.At, e.Payload.answered, in.At)
		for _, s := range e.Seen {
			path := cmp.Or(s.Leaf.Path, s.Place)
			ps = append(ps, evPlace{
				name: e.Mock + " " + path, mock: e.Mock, path: path,
				rec: s.Leaf.Value, live: s.Live, side: s.Side, leaf: s.Leaf,
				given: e.Given, gave: e.Gave, undeclared: s.Undeclared, clock: s.Clock, offset: s.Offset, startUp: startUp, otherTest: other, trailed: trailed, scope: scope,
				at: e.At, state: st, why: why, fGave: !e.Payload.servedAfter, outsideTest: outsideTest, lateCall: lateCall,
				recAfter: !e.alwaysEarlier() && !e.noTime && e.At.After(in.At),
				rBefore:  s.Side != Response && sentBefore || s.Side == Response && answeredBefore,
			})
			// What the application got in an answer it got at the answer's
			// time (answerState), its request before or at the call's instant.
			if s.Side == Response && (st == stateBefore || why == atCallWhy) && e.Payload.isRead() {
				p := &ps[len(ps)-1]
				if ast, awhy := answerState(e.At, e.Payload.answered, in.At); ast != stateBefore {
					p.state, p.why, p.recAfter = ast, awhy, ast == stateNotBefore && !e.alwaysEarlier()
				}
			}
		}
	}
	for i, r := range in.Raw {
		if !r.Read() {
			unread = cmp.Or(unread, unreadName(r.Mock(), Earlier{})+" ("+cmp.Or(r.unreadWhy(), notFromMock)+")")
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
		// (OfAnotherTest), is outside the call's scope. A word is read in
		// its text whole, placed by its request's time where its request
		// holds it (placedFor); an instant in each half, its request a send
		// and its answer an answer (halves).
		other := r.olderTest || r.elsewhere
		startUp, other, scope := inScope(in, r.Mock(), r.beforeTest && !other, other)
		// A recording of the test the call trails is the call's own place
		// (Input.Trails), as an exchange of it is (above), where keploy
		// served that test's call from its mappings and so kept no
		// exchange of it leaf by leaf: one F gave the application only in
		// that test, older than the call's, and one F gave in that test's
		// own window where the call lands in it too. Bound there, a value
		// is the run's variable, so a replay binds it in whichever window
		// the call lands. One mappings.yaml lists in another test than the
		// trailed one stays that test's.
		trailed := in.trails(r.Mock()) && !startUp && (scope == "" || scope == in.Trails.Test) && (r.InAnOlderTest() || !other && in.Test == in.Trails.Test)
		if trailed {
			other = false
		}
		p := evPlace{
			name: r.Mock(), mock: r.Mock(), src: src, fSrc: live, side: Response,
			startUp: startUp, otherTest: other, trailed: trailed, outsideTest: rawOutsideTest(r), scope: scope,
			at: r.at, state: st, why: why, fUnstated: fUnstated, raw: i + 1,
			fMay: fUnstated && len(views) == 0 && r.served && !r.servedAfter, views: views,
			fGave: r.served && !r.servedAfter, recAfter: !r.noTime && (r.at.After(in.At) || r.answered.After(in.At)),
			rBefore: !r.noTime && answeredBy(r.at, r.answered, in.At),
		}
		if rst, rwhy := rawRequestState(in, r); !r.noTime && (rst != st || rwhy != why) {
			p.req = &reqPlace{state: rst, why: rwhy, after: r.at.After(in.At)}
		}
		ps = append(ps, p)
	}
	return ps, unread, byWords
}

// rawOutsideTest is evPlace.outsideTest of r: why F's serve of it says
// nothing of a value where F gave it the application only in an older test
// of the set (Raw.InAnOlderTest) or only before the call's test began
// (Raw.BeforeTheTest), as main's outsideTest reads it.
func rawOutsideTest(r Raw) string {
	switch {
	case r.InAnOlderTest():
		return olderTestWhy
	case r.BeforeTheTest():
		return beforeTestWhy
	}
	return ""
}

// earlierOutsideTest is evPlace.outsideTest of e: an exchange F served
// only before the call's test began (Earlier.beforeTest).
func earlierOutsideTest(e Earlier) string {
	if e.beforeTest {
		return beforeTestWhy
	}
	return ""
}

// earlierLateCall is evPlace.lateCall of e: the place an earlier test's
// call left the run, which the application sent in an older test.
func earlierLateCall(e Earlier) string {
	if e.ofEarlierTest() {
		return "the application sent " + e.Mock + "'s request only in an older test than the call's: the call may be a late call of that test, by an application that fell behind, so each value may be a copy of a word its run picked from what both runs gave"
	}
	return ""
}

// rawRequestState is a recording's state for a word its request holds:
// the application held what it sent when it sent it, so the request is
// placed by its own time, as earlierState places an exchange's request
// leaf, whenever (or if ever) it was answered; and given the application
// by F before the call reached keploy, as rawState asks.
func rawRequestState(in Input, r Raw) (placeState, string) {
	switch {
	case r.at.Equal(in.At):
		return stateUnknown, atCallWhy
	case r.at.After(in.At):
		return stateNotBefore, askedAfterWhy
	case !r.served || r.servedAfter:
		return stateNotBefore, ""
	}
	return stateBefore, ""
}

// placedFor is p as it stands for the recorded word r: a recording whose
// request holds r at one of its request's fields (requestPath) is placed
// by its request's time (p.req); one whose answer alone holds r, or whose
// fields keploy could not read, stays placed by its answer (rawState).
func placedFor(p evPlace, r string) evPlace {
	if p.req == nil {
		return p
	}
	at, _ := p.t.rec.fieldsWith(r)
	if slices.ContainsFunc(at, func(f fieldAt) bool { return requestPath(f.path) }) {
		p.state, p.why, p.recAfter = p.req.state, p.req.why, p.req.after
	}
	return p
}

// wordPlaces are a word's check's places (base, the call's): each copied
// and placed for R's word r (placedFor) at its first read (at), and kept
// for the check, so serve order and a swap mark the copy. A check reads
// the places the postings name (placeIndex), the places of a candidate's
// mock (orderUnknown) and those a late call's search names
// (lateCallTook): it copies and places those alone, not every place of the
// call for every word (placeCopies counts each copy). It keeps them by
// place (of, made at the first copy): a slot for each place of the call,
// made for every word, cost a call of thousands of places and hundreds of
// words more than the few places each word reads.
type wordPlaces struct {
	base []evPlace
	r    string
	of   map[int]*evPlace
}

func newWordPlaces(base []evPlace, r string) *wordPlaces {
	return &wordPlaces{base: base, r: r}
}

// at is the i-th place, placed for the word.
func (ps *wordPlaces) at(i int) *evPlace {
	if p := ps.of[i]; p != nil {
		return p
	}
	placeCopies.Add(1)
	p := new(evPlace)
	*p = placedFor(ps.base[i], ps.r)
	if ps.of == nil {
		ps.of = map[int]*evPlace{}
	}
	ps.of[i] = p
	return p
}

// placeCopies counts the places a word's check copies and places
// (wordPlaces), for the rows that bound a call's work.
var placeCopies atomic.Int64

// textHalf is t, a recording's text, as its request alone (request) or
// its answer alone: each field in or outside its request (requestPath),
// laid out as textFields lays a text out; t itself where it is no
// recording's (a leaf is one side already), with where each of those
// fields stands in it (laid). What a recording's request holds is what the
// application sent there, never what the recording gave it. Where keploy
// could not read the recording, t itself, which its read marked unread:
// every question of it is unread, and the call refused.
func textHalf(t *readerText, request bool) (half *readerText, laid []rawField) {
	if t.raw == nil {
		return t, nil
	}
	s, fields, ok := t.read()
	if !ok {
		return t, nil
	}
	var b strings.Builder
	for _, f := range fields {
		if requestPath(f.path) == request {
			text := s[f.start:f.end]
			f.start = b.Len()
			b.WriteString(text)
			f.end = b.Len()
			laid = append(laid, f)
			b.WriteByte('\n')
		}
	}
	return t.x.text(b.String()), laid
}

// halves is p, the i-th of the call's places, a recording's, as its two
// halves, each read in that half of each run's text (textHalf): its
// request, what the application sent there, a send placed by its request's
// time (p.req), and its answer, what the recording gave it, placed by its
// answer's (rawState). A place's halves are built once per call
// (dc.halfBuilds counts each build) and kept to its end where the index's
// budget holds their bytes (claimed there, with what the index keeps, and
// given back with it); past the budget they are the instant's that asks
// alone, built again for each instant and let go when it is decided
// (letGoHalves): what the call holds of them is bounded by the budget and
// one instant's halves, however many instants it decides (dc.halfPeak).
// Each half's fields are kept beside it (dc.laid, dc.laidNow), where
// atFinest reads them.
func (dc *decider) halves(i int, p evPlace) (req, ans evPlace) {
	if h, ok := dc.halved[i]; ok {
		return h[0], h[1]
	}
	var texts []*readerText
	var fields [][]rawField
	half := func(request bool) evPlace {
		h := p
		rec, rf := textHalf(p.t.rec, request)
		live, lf := textHalf(p.t.live, request)
		texts, fields = append(texts, rec, live), append(fields, rf, lf)
		h.t = &placeText{rec: rec, live: live, views: p.t.views}
		return h
	}
	req, ans = half(true), half(false)
	req.side = Request
	if p.req != nil {
		req.state, req.why, req.recAfter = p.req.state, p.req.why, p.req.after
	}
	dc.halfBuilds++
	var n int64
	for j, t := range texts {
		n += int64(len(t.text))
		for _, f := range fields[j] {
			n += rawFieldBytes + int64(len(f.path))
		}
	}
	laid := &dc.laidNow
	if dc.x.claim(n) {
		if dc.halved == nil {
			dc.halved = map[int][2]evPlace{}
		}
		dc.halved[i] = [2]evPlace{req, ans}
		laid = &dc.laid
		dc.halfKept += n
	} else {
		dc.halfNow += n
	}
	dc.halfPeak = max(dc.halfPeak, dc.halfKept+dc.halfNow)
	if *laid == nil {
		*laid = map[*readerText][]rawField{}
	}
	for j, t := range texts {
		(*laid)[t] = fields[j]
	}
	return req, ans
}

// letGoHalves lets go of the halves built past the budget for the instant
// just decided (halves).
func (dc *decider) letGoHalves() {
	dc.laidNow, dc.halfNow = nil, 0
}

// rawFieldBytes is about the bytes a half's field takes, but for its path.
const rawFieldBytes = int64(unsafe.Sizeof(rawField{}))

// notFromMock is why a recording's payload was not read, where its read
// gave none.
const notFromMock = "it was not read from its mock, learn.RawOf"

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
		return stateNotBefore, askedAfterWhy
	}
	return stateBefore, ""
}

// rawState is a recording's state: answered before the call in R
// (answerState), and given the application by F before the call reached
// keploy.
func rawState(in Input, r Raw) (placeState, string) {
	switch st, why := answerState(r.at, r.answered, in.At); {
	case r.noTime:
		return stateUnknown, noAnswerAt
	case st != stateBefore:
		return st, why
	case !r.served || r.servedAfter:
		return stateNotBefore, ""
	}
	return stateBefore, ""
}

// answerState places a recording's answer by its answer's time, when the
// application got its words: one rule for an exchange's answer, a start-up
// recording's and a set entry's (a word a request holds is placed by the
// request's: earlierState, rawRequestState). Answered before the call, it
// is before; answered after it (however its request was timed), it gave
// R's application nothing before the call, so R's word there decides
// nothing (recAfter). The call's own instant is neither before nor after:
// a recording answered at it is unknown, refused (fail closed). One with no
// answer time keploy can read is unknown.
func answerState(req, answered, at time.Time) (placeState, string) {
	switch {
	case untimedAnswer(req, answered):
		return stateUnknown, noAnswerAt
	case answered.After(at):
		return stateNotBefore, recordedAfterWhy
	case answered.Equal(at):
		return stateUnknown, atCallWhy
	}
	return stateBefore, ""
}

// recordedAfterWhy is why a word is refused at a recording answered after
// the call: R's word there decides nothing, so round F's side refuses it.
const recordedAfterWhy = "answered after the call, it shows nothing of the recorded value; round F gave, or may have given, the new value there"

// askedAfterWhy is recordedAfterWhy where a place's request time places
// it (rawRequestState, earlierState): it was asked after the call, which
// keploy knows where it can read no answer time too.
const askedAfterWhy = "asked after the call, it shows nothing of the recorded value; round F gave, or may have given, the new value there"

// answeredBy reports whether answerState places the answer before the call
// at at.
func answeredBy(req, answered, at time.Time) bool {
	st, _ := answerState(req, answered, at)
	return st == stateBefore
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
// another serve of it in F. Two places are tied alike here never: an
// order the recording cannot tell, or one that differs, marks them
// unknown even where both hold the value's words at the same fields. It
// reports whether it marked a place.
func orderUnknown(in Input, ps *wordPlaces, cand map[string]bool) (marked bool) {
	if len(cand) == 0 || len(cand) == 1 && !servedAgain(in, cand) {
		return false
	}
	source := func(mock string) bool { return cand[mock] }
	rec, _, untoldRec := recordedOrder(in.At, in.Earlier, in.Raw, source, func(Raw) string { return "" })
	f, untoldF := servedOrder(in.Earlier, in.Raw, source)
	why := cmp.Or(untoldRec, untoldF)
	if d := orderDiff(rec, f, func(a, b string) bool { return a == b }); why == "" && d != "" {
		why = "another order: " + d
	}
	if why == "" {
		return false
	}
	for i := range ps.base {
		if !cand[ps.base[i].mock] {
			continue
		}
		if p := ps.at(i); p.state == stateBefore {
			p.state, p.why = stateUnknown, why
			marked = true
		}
	}
	return marked
}

// servedAgain reports whether round F served a candidate of cand at two
// numbered serves (servedOrder's span).
func servedAgain(in Input, cand map[string]bool) bool {
	return slices.ContainsFunc(in.Earlier, func(e Earlier) bool {
		return cand[e.Mock] && (e.Payload.Read() || e.Payload.ordered) && e.Payload.last != e.Payload.first
	}) || slices.ContainsFunc(in.Raw, func(x Raw) bool { return cand[x.mock] && x.served && x.last != x.first })
}

// Why a place's state in round F is unknown, as a refusal's line says it
// in parentheses.
const (
	swapNote   = "a connection start-up keploy may swap"
	atCallWhy  = "recorded at the call's own instant"
	noTimeWhy  = "recorded at a time keploy cannot read"
	noAnswerAt = "no answer time keploy can read"
	// servedAfterWhy: round F served a recording of the set only after the
	// call reached keploy, or in a request still aloft then
	// (givenWords.fAfter).
	servedAfterWhy = "round F gave it to the application only after the call reached keploy"
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
// match it finds in no one field as a swap's. A recording that is no
// start-up is no swap's, and is not loaded to ask. at is the places the
// word's check reads (placeIndex.asked), held what each of their texts
// holds of w (heldAt, held[k] of at[k]), asked once for the word: a place
// it does not read holds neither word.
func swapUnknown(in Input, ps *wordPlaces, w readWord, at []int32, held []evHeld) {
	holds := func(own string) bool {
		return holdsToken(own, w.Rec) || slices.ContainsFunc(w.Tokens, func(t wholeToken) bool { return holdsToken(own, t.Rec) })
	}
	for k, i := range at {
		if !held[k].hr {
			continue
		}
		p := ps.at(int(i))
		if p.raw == 0 || p.state != stateBefore {
			continue
		}
		if r := in.Raw[p.raw-1]; r.startUp && r.swapHolds(r.payload().text, in.Raw, holds) {
			p.state, p.why = stateUnknown, swapNote
		}
	}
}

// evHeld is what a place's two texts hold of a changed word: R's text the
// recorded word (hr), round F's text F's (hf), and whether both were read.
// It is asked of a place once per word: serve order (orderUnknown,
// swapUnknown) changes only the place's state, which no text answers.
type evHeld struct{ hr, hf, read bool }

// heldAt asks p's texts for the changed word r (R) to f (F), once each.
func heldAt(p evPlace, r, f string) evHeld {
	placeAsks.Add(1)
	hr, okR := p.t.rec.holdsWord(r)
	hf, okF := p.t.live.holdsWord(f)
	return evHeld{hr: hr, hf: hf, read: okR && okF}
}

// evidenceOf is what p shows of the changed word r (R) to f (F), given
// what its texts hold of it (h, heldAt): the evidence table, read through x,
// at p's state as it stands now. read is false where p's text lay past the
// call's budget.
func evidenceOf(p evPlace, h evHeld, r, f string, piece bool) (ev wordEvidence, read bool) {
	if !h.read {
		return evNone, false
	}
	hr, hf := h.hr, h.hf
	// A recording answered after the call gave R's application nothing
	// before it: R's word there decides nothing (F's still does), as
	// setEvidence reads a set entry.
	hr = hr && !p.recAfter
	if p.fUnstated {
		if hr || hf || p.fMay || slices.ContainsFunc(p.t.views, func(v *readerText) bool { return evHolds(v, f) }) {
			return evUnknown, true
		}
		return evNone, true
	}
	switch same := p.same(); {
	case !hr && !hf:
		return evNone, true
	case p.state != stateBefore, same && hr && hf && !piece:
		// The second, unchanged: one text holds both, so neither run's shows the change.
		// A piece of a longer value (pieceOfAnId) is chance there: the
		// value's long words judge the place.
		return evUnknown, true
	case same:
		return evDisproven, true
	case hr && hf:
		return evShown, true
	case nothingNew(p):
		// F gave nothing new there: every word of F's text stood in R's.
		return evUnknown, true
	case hr && !strings.Contains(p.liveText(), f), !hr && unchangedFor(p, r, f):
		// The second, changed elsewhere, as recorded for the word: a start-up answer
		// served as recorded to a request of round F's own session.
		return evDisproven, true
	}
	return evUnknown, true
}

// unchangedFor reports whether p, changed, is unchanged for the word: one
// field holds f in both texts, and round F's holds no r (a text keploy could
// not read does not: fail closed).
func unchangedFor(p evPlace, r, f string) bool {
	if hl, ok := p.t.live.holdsWord(r); hl || !ok {
		return false
	}
	same, _ := inOneField(p.t.rec, f, p.t.live, f)
	return same
}

// movedField is why p does not show r and f in both runs though its texts
// hold them: no one field holds r in the recording and f in round F's view
// (inOneField's rule), so F gave the word at another field than the
// recorded one (main reads a recording field by field). "" where one field
// holds both. It reads each text's fields once: past the budget each read
// scans the text.
func movedField(p evPlace, r, f string) string {
	rf, okR := p.t.rec.fieldsWith(r)
	lf, okL := p.t.live.fieldsWith(f)
	switch {
	case !okR || !okL:
		return "past the call's budget"
	case slices.ContainsFunc(rf, func(a fieldAt) bool { return slices.Contains(lf, a) }):
		return ""
	case len(lf) > 0:
		return "its view gave " + strconv.Quote(f) + " at " + lf[0].path + ", not at a field whose recorded text holds " + strconv.Quote(r)
	}
	return "its view holds " + strconv.Quote(f) + " at no one field"
}

// countsByText reports whether a disproving place counts the word by its
// text: 32 bits or more (wordBits), alone or as a whole token around it
// the place also holds whole: an unchanged place that holds only one of the two words is chance.
func countsByText(p evPlace, w readWord) bool {
	return wordBits(w.Rec) >= 32 || wordBits(w.Live) >= 32 || slices.ContainsFunc(w.Tokens, func(t wholeToken) bool {
		return wordBits(t.Rec) >= 32 && tokenIn(p.t.rec, t.Kind, t.Rec) || wordBits(t.Live) >= 32 && tokenIn(p.t.live, t.Kind, t.Live)
	})
}

// tokenIn reports whether t holds the whole token tok, read or not.
func tokenIn(t *readerText, kind tokenKind, tok string) bool {
	h, _ := t.holdsToken(kind, tok)
	return h
}

// heldWhole reports whether p, a place F moved, held the recorded value's
// whole token around w in R (a conflict in the call's scope
// or the start-up).
func heldWhole(p evPlace, w readWord, rec string) bool {
	// No token around it: the word is a whole token of its own (no ID byte
	// beside it).
	return !p.same() && (tokenIn(p.t.rec, tokenID, rec) || len(w.Tokens) == 0 && tokenIn(p.t.rec, tokenID, w.Rec) ||
		slices.ContainsFunc(w.Tokens, func(t wholeToken) bool { return tokenIn(p.t.rec, t.Kind, t.Rec) }))
}

// sameTokenAround is the recorded value's whole token around w, longer than
// w, that p, a place both runs gave alike, held in R ("blue-17" for "17" of
// "blue-17"), "" where it held none. Such a token stood there whole in both
// runs while the value moved off it: no chance of a short word, so keploy
// cannot tell whether the value came from there (main refuses it as a word
// of an earlier place that did not drift with it). p's texts are set
// (withTexts), as heldWhole reads them.
func sameTokenAround(p evPlace, w readWord) string {
	if !p.same() {
		return ""
	}
	for _, t := range w.Tokens {
		if tokenIn(p.t.rec, t.Kind, t.Rec) {
			return t.Rec
		}
	}
	return ""
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
	return !slices.ContainsFunc(variables.Words(live), func(w [2]int) bool { return !had[live[w[0]:w[1]]] })
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
	if i := slices.IndexFunc(set, func(g GivenPlaces) bool { return !g.read }); i >= 0 {
		return nil, set[i].mock + ", a recording round F gave earlier in the test set"
	}
	return set, ""
}

// outside are the recordings outside the call's scope that round F gave
// before it, read once for the call, as main's word rule and echo read them
// (offsetPlacesOf: the set's and raw's, each recording once): their
// answers' words and instants; empty where the call has none.
func (dc *decider) outside() *offsetPlaces {
	if dc.op == nil {
		dc.op = &offsetPlaces{}
		if len(dc.set) > 0 || slices.ContainsFunc(dc.in.Raw, func(r Raw) bool { return r.elsewhere }) {
			dc.op = dc.offsetPlaces()
		}
	}
	return dc.op
}

// offsetPlaces is offsetPlacesOf's places for the call, with the words and
// instants of each recording of another test handed to it
// (Raw.OfAnotherTest) read as offsetPlacesOf reads them, in raw's order
// before the set's, but its words kept for the set (outsideWords), not read
// again in each call. offsetPlacesOf reads Raw.elsewhere only there, so it
// is handed raw with the mark cleared, and reads every other place as
// before.
func (dc *decider) offsetPlaces() *offsetPlaces {
	raw := dc.in.Raw
	var words []givenWords
	var answers []givenMoment
	if slices.ContainsFunc(raw, func(r Raw) bool { return r.elsewhere }) {
		raw = slices.Clone(raw)
		for i, x := range dc.in.Raw {
			if x.elsewhere {
				raw[i].elsewhere = false
				if x.isRead() {
					words = append(words, dc.outsideWords(x))
					answers = append(answers, dc.dr.answerMoments(x)...)
				}
			}
		}
	}
	op := dc.dr.offsetPlacesOf(dc.in.At, dc.in.Earlier, raw, dc.set)
	op.words = append(words, op.words...)
	op.answers = append(answers, op.answers...)
	return op
}

// outsideWords is wordsOf(x), x a recording of another test handed to the
// call, its words kept for the set by x's identity (Raw.id) and what else
// makes them (wordsKey): how round F served x and the payload of each view
// of it the call was handed, which a call reads again from the recording's
// statements (Order.ServedView). Counted on the set's budget (setClaim). A
// later call handed x as an earlier one was asks the kept words, not x's
// texts; one handed another view reads them again, and keeps them apart, as
// one past the budget, or of no set, reads them in each call.
func (dc *decider) outsideWords(x Raw) givenWords {
	s := dc.dr.setOf(x)
	if s == nil {
		return dc.dr.wordsOf(x)
	}
	fs, k := filtersOf(s), wordsKeyOf(x)
	fs.mu.Lock()
	i := slices.IndexFunc(fs.words[x.id], func(e *wordsEntry) bool { return e.key.equal(k) })
	if i >= 0 {
		e := fs.words[x.id][i]
		fs.mu.Unlock()
		return e.of(x)
	}
	fs.wordsBuilds++
	first := len(fs.words) == 0
	fs.mu.Unlock()
	g := dc.dr.wordsOf(x)
	// The entry counts its slot in the set's map (as a posting's, a key
	// and a list's headers) with the id's letters, and its pointer in the
	// id's list at twice its size.
	e := keptWordsOf(k, g)
	e.bytes += postSlotBytes + allocBytes(len(x.id), false) + allocBytes(16, true)
	if first {
		e.bytes += mapGroupBytes(slotOf[string, []*wordsEntry](), true)
	}
	if setClaim(s, e.bytes) {
		fs.mu.Lock()
		fs.words[strings.Clone(x.id)] = append(fs.words[x.id], e)
		fs.mu.Unlock()
		return e.of(x)
	}
	return g
}

// wordsEntry is what the set keeps of an outside recording's words
// (outsideWords): its words as wordsOf read them (lists), and what they
// were read from (key), each its own copy (keptWordsOf), and the bytes it
// counts on the set's budget: what both hold, and its slot in the set's
// map.
type wordsEntry struct {
	key   wordsKey
	lists givenWords
	bytes int64
}

// keptWordsOf is the entry the set keeps of g, the words wordsOf read of a
// recording with key k, counted as it holds them (allocBytes): the entry,
// a copy of each payload of k, and a copy of each list of g cut to its
// length (an empty one allocates none: no capacity answerListsOf left in a
// list stays alive with it), each word once and its own copy, and lists that
// share one array (a recording served as recorded gave the same words)
// sharing one copy. Each copy is the entry's own: no text the call read
// stays alive in it past what it counts. A later call is handed the
// entry's lists (of), the same each time: the set's index of word lists
// (listsKey) reads them once.
func keptWordsOf(k wordsKey, g givenWords) *wordsEntry {
	e := &wordsEntry{}
	n := allocBytes(unsafe.Sizeof(*e), true)
	e.key.served, e.key.asRecorded, e.key.unseen, e.key.servedAfter = k.served, k.asRecorded, k.unseen, k.servedAfter
	if k.live != nil {
		b, bn := keptPayload(*k.live)
		e.key.live, n = &b, n+bn+allocBytes(unsafe.Sizeof(b), true)
	}
	if k.views != nil {
		e.key.views = make([]rawBody, len(k.views))
		n += allocBytes(uintptr(len(k.views))*unsafe.Sizeof(rawBody{}), true)
		for i, v := range k.views {
			b, bn := keptPayload(v)
			e.key.views[i], n = b, n+bn
		}
	}
	words, copies := map[string]string{}, map[*[]string][][]string{}
	keep := func(ls [][]string) [][]string {
		if ls == nil {
			return nil
		}
		if c, ok := copies[unsafe.SliceData(ls)]; ok && len(c) == len(ls) {
			return c
		}
		c := make([][]string, len(ls))
		n += allocBytes(uintptr(len(ls))*unsafe.Sizeof(ls[0]), true)
		for i, l := range ls {
			if len(l) == 0 {
				if l != nil {
					c[i] = []string{}
				}
				continue
			}
			c[i] = make([]string, len(l))
			n += allocBytes(uintptr(len(l))*unsafe.Sizeof(l[0]), true)
			for j, w := range l {
				kw, ok := words[w]
				if !ok {
					kw = strings.Clone(w)
					words[kw] = kw
					n += allocBytes(len(kw), false)
				}
				c[i][j] = kw
			}
		}
		copies[unsafe.SliceData(ls)] = c
		return c
	}
	keepLists := func(l answerLists) answerLists { return answerLists{words: keep(l.words), stamped: keep(l.stamped)} }
	e.lists = g
	e.lists.answerLists, e.lists.view = keepLists(g.answerLists), keepLists(g.view)
	e.lists.same, e.lists.gave = keep(g.same), keep(g.gave)
	if g.views != nil {
		e.lists.views = make([]answerLists, len(g.views))
		n += allocBytes(uintptr(len(g.views))*unsafe.Sizeof(answerLists{}), true)
		for i, v := range g.views {
			e.lists.views[i] = keepLists(v)
		}
	}
	e.lists.mock, e.lists.conn = "", ""
	e.bytes = n
	return e
}

// keptPayload is a copy of b the set keeps (keptWordsOf), its text, its
// fields' and stamps' lists and paths and its clock reads each its own,
// and the bytes the copy holds (allocBytes).
func keptPayload(b rawBody) (rawBody, int64) {
	c := rawBody{text: strings.Clone(b.text), unread: strings.Clone(b.unread), read: b.read}
	n := allocBytes(len(c.text), false) + allocBytes(len(c.unread), false)
	if b.answerClock != nil {
		c.answerClock = make([]time.Time, len(b.answerClock))
		copy(c.answerClock, b.answerClock)
		n += allocBytes(uintptr(len(b.answerClock))*unsafe.Sizeof(time.Time{}), true)
	}
	fields := func(fs []rawField) []rawField {
		if fs == nil {
			return nil
		}
		out := make([]rawField, len(fs))
		n += allocBytes(uintptr(len(fs))*unsafe.Sizeof(rawField{}), true)
		for i, f := range fs {
			f.path = strings.Clone(f.path)
			n += allocBytes(len(f.path), false)
			out[i] = f
		}
		return out
	}
	c.fields, c.stamps = fields(b.fields), fields(b.stamps)
	return c, n
}

// wordsKey is what makes a recording's words (wordsOf) past its identity,
// which fixes its own payload: how round F served it (Raw.served,
// asRecorded, unseen), whether only after the call reached keploy
// (servedAfter: a later call it was served before reads it apart), and the
// payloads of F's view of it (live) and of the views F's serves of it gave
// (views).
type wordsKey struct {
	served, asRecorded, unseen, servedAfter bool
	live                                    *rawBody
	views                                   []rawBody
}

func wordsKeyOf(x Raw) wordsKey {
	k := wordsKey{served: x.served, asRecorded: x.asRecorded, unseen: x.unseen, servedAfter: x.servedAfter}
	if x.live != nil {
		b := x.live.payload()
		k.live = &b
	}
	for _, v := range x.views {
		k.views = append(k.views, v.payload())
	}
	return k
}

func (k wordsKey) equal(o wordsKey) bool {
	return k.served == o.served && k.asRecorded == o.asRecorded && k.unseen == o.unseen && k.servedAfter == o.servedAfter &&
		(k.live == nil) == (o.live == nil) && (k.live == nil || sameBody(*k.live, *o.live)) && slices.EqualFunc(k.views, o.views, sameBody)
}

// sameBody reports whether a and b, the payloads of two views (Raw.viewOf),
// are one: text, fields, stamps and clock reads alike. A view is one keploy
// read, so neither tells read from unread.
func sameBody(a, b rawBody) bool {
	return a.text == b.text && slices.Equal(a.fields, b.fields) && slices.Equal(a.stamps, b.stamps) &&
		slices.EqualFunc(a.answerClock, b.answerClock, time.Time.Equal)
}

// of is e's words as wordsOf reads them of x, the recording e keeps them
// of: x's own times, serves and test (as wordsOf copies them), and e's
// words.
func (e *wordsEntry) of(x Raw) givenWords {
	g := e.lists
	g.mock, g.at, g.late, g.noTime, g.answered, g.conn, g.pos = strings.Clone(x.mock), x.at, x.late, x.noTime, x.answered, strings.Clone(x.conn), x.pos
	g.test, g.prev, g.first, g.last = x.test, x.prev, x.first, x.last
	return g
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
			if (g.words.unstated || g.words.unread) && namesApart(g.answer, *c.rm, *c.lm) {
				return g.mock, "the recorded moment stands in " + g.mock + ", another test's answer given before the call, whose view in round F keploy cannot tell"
			}
		case namesApart(ms, *c.lm, *c.rm):
			return g.mock, "round F's moment stands in " + g.mock + ", which round F gave before the call but which was recorded " + when
		case atCall && namesApart(g.answer, *c.rm, *c.lm):
			return g.mock, "the recorded moment stands in " + g.mock + ", recorded " + when + ", which the recording may have given before the call"
		}
	}
	return "", ""
}

// namesApart reports whether one of ms names m by a cut that does not name
// other too.
func namesApart(ms []givenMoment, m, other moment) bool {
	return slices.ContainsFunc(ms, func(v givenMoment) bool { return namesMomentApart(v.m, m, other, nil) })
}

// echoedOutside names the recording outside the call's scope whose answer,
// as round F gave it before the call, holds the changed instant c: round F's
// moment, or the recorded one where it was recorded before the call, by a
// cut that does not name the other run's too. ok where main's echo of those
// places (echoedElsewhere) refuses c, or where round F's view of a recording
// of the set (GivenPlaces.fView: what F's serve of it was told to give)
// holds F's moment so; mock "" where no one recording names it alone. An
// instant there is a copy of what F gave the application, or a pick from
// it, never a read of the clock. served is the call's earlier places the
// echo reads: dc.in.Earlier, or those without a client test's requests where
// a read of the clock goes before them (readOverClients).
func (dc *decider) echoedOutside(c changedInstant, served []Earlier) (mock string, ok bool) {
	if c.rm == nil || c.lm == nil {
		return "", false
	}
	if i := slices.IndexFunc(dc.set, func(g GivenPlaces) bool { return namesApart(g.fView, *c.lm, *c.rm) }); i >= 0 {
		return dc.set[i].mock, true
	}
	op := dc.outside()
	pair := []driftedInstant{{rec: *c.rm, live: *c.lm}}
	if len(op.answers) == 0 || dc.dr.echoedElsewhere(pair, dc.in.At, served, dc.in.Raw, func() *offsetPlaces { return op }) == "" {
		return "", false
	}
	if i := slices.IndexFunc(op.answers, func(g givenMoment) bool {
		return namesMomentApart(g.m, *c.lm, *c.rm, nil) || !g.at.After(dc.in.At) && namesMomentApart(g.m, *c.rm, *c.lm, nil)
	}); i >= 0 {
		return op.answers[i].mock, true
	}
	return "", true
}

// setEvidence is what g shows of the changed word r (R) to f (F), and
// whether round F changed it: the evidence table, read by words, where inR
// and inF are the lists of g that hold r and f (outsideHeld.holding). Its
// recorded words count for R only where it was recorded before the call.
// It shows or disproves only where it is before the call in both runs, as a
// recording handed to the call is (rawState): one with no answer time, or
// recorded or answered at the call's own instant, is unknown there.
func setEvidence(g givenWords, inR, inF heldRoles, at time.Time, piece bool) (ev wordEvidence, changed bool) {
	// A recording answered after the call gave R's application nothing
	// before it: R's word there decides nothing (F's still does).
	hr := inR&inOwn != 0 && g.givenBefore(at)
	before := g.answeredBefore(at)
	switch {
	case g.fAfter:
		// Round F served it only after the call reached keploy, or in a
		// request still aloft then: it gave the application nothing there
		// before the call, whatever its serves stated. So round F's word in
		// what round F gave there shows nothing of where round F's
		// application took it. Round F's word in the recording's own text,
		// written in the recording run, is no word round F's application
		// made: it shows nothing either way, as a serve told as recorded
		// stays so for a Raw place (Raw.asOfArrival).
		if hr || inF&inOwn != 0 {
			return evUnknown, false
		}
		return evNone, false
	case g.unstated || g.unread:
		// Round F served it and keploy cannot tell what it gave there.
		return evUnknown, true
	case len(g.views) > 0:
		if hr || inF&inViews != 0 {
			return evUnknown, true
		}
		return evNone, true
	case len(g.view.words) > 0:
		hf := inF&inView != 0
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
		hf := inF&inOwn != 0
		switch {
		case !hr && !hf:
			return evNone, false
		case !before || hr && hf && !piece:
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

// givenBefore reports whether g's answer was given R's application before
// the call at at: recorded and answered at or before it, or with no answer
// time keploy can read (fail closed).
func (g *givenWords) givenBefore(at time.Time) bool {
	return g.noTime || !g.at.After(at) && !g.answered.After(at)
}

// answeredBefore reports whether g's answer was given R's application
// before the call at at, by times keploy reads: answered before it, after
// its own request (so that was before it too). Unlike givenBefore, one with
// no time keploy reads is not.
func (g *givenWords) answeredBefore(at time.Time) bool {
	return !g.noTime && !untimedAnswer(g.at, g.answered) && g.answered.Before(at)
}

// gluedAt reports whether p, a body that declares no media type, holds R's
// or F's word glued to the bytes around it, in any form it declares (main's
// gluedIn): keploy cannot tell its words from the bytes that frame them.
func (dc *decider) gluedAt(p evPlace, w readWord) bool {
	if !p.undeclared {
		return false
	}
	ed := dc.dr.of(p.rec, p.live)
	return gluedIn(ed, w.Rec, true) || gluedIn(ed, w.Live, false)
}

// setUnknownWhy is why a recording of the set shows nothing of a word:
// that round F gave it the application only after the call (fAfter); its
// time, where keploy cannot place it before the call (rawState); else that
// keploy keeps only its words.
func setUnknownWhy(g givenWords, at time.Time) string {
	switch st, why := answerState(g.at, g.answered, at); {
	case g.fAfter:
		return servedAfterWhy
	case g.noTime:
		return noAnswerAt
	case st != stateBefore:
		return why
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

// heldRoles are the lists of an outside recording (givenWords) that hold a
// word: its answer's (inOwn), F's view of it (inView), any view F's serves
// of it gave (inViews), as listsHold reads each.
type heldRoles uint8

const (
	inOwn heldRoles = 1 << iota
	inView
	inViews
)

// The set's index of its outside recordings' word lists (setLists): each
// answerLists a call's outside recordings hold (their answers', F's views
// of them, the views F's serves gave), its words read once for the set into
// postings, by word, of the lists that hold it. A call asks a word's
// postings, and decides only the recordings whose lists hold R's or F's
// word, and those whose state alone leaves them unknown (unstated, unread):
// any other shows nothing of the word (setEvidence: evNone). Asked per
// recording, a word cost each call a search of every recording's lists in
// each reading, however few held it. A list is the set's by its identity
// (listsKey): the set reads its outside recordings' lists once (setPlaces,
// outsideWords) and hands each call the same lists, which nothing writes
// after answerListsOf builds them. A list past the set's budget, or of a
// call in no set, is searched as before (listsHold), and decides the same.

// listsEntry is what the set keeps of one list: the id its postings name it
// by (-1 where it did not fit the set's budget), its words, to take it out
// of their postings once the list is let go, and the bytes it counts on the
// set's budget.
type listsEntry struct {
	once  sync.Once
	id    int32
	words []string
	bytes int64
	// post is what its words count in the postings (postLive).
	post int64
}

// Sizes a list's entry counts past its allocations (allocBytes: the entry
// and its words' list): its slot in the set's map of lists (mapSlotBytes),
// and per word what the postings' map holds for it (postBytes), counted
// fresh for every word, the side that counts more, as a word another list
// posted first stays in the map once that list is let go. The first list
// counts the two maps' first groups (firstGroups).
var listsSlotBytes = mapSlotBytes(slotOf[weak.Pointer[[]string], *listsEntry](), true)

// firstGroups is what the first insert into the set's map of lists and its
// postings of their words (setFilters.lists, post) counts once for the set
// and never gives back: each map's first group, which it holds from then
// on, a map built anew (dropLists) one in place of the one it lets go.
// fs.mu is held.
func (fs *setFilters) firstGroups() int64 {
	if fs.listsGrouped {
		return 0
	}
	return mapGroupBytes(slotOf[weak.Pointer[[]string], *listsEntry](), true) + mapGroupBytes(slotOf[string, []int32](), true)
}

// listsKey is l's identity: its words' backing array, weakly, so the set's
// index never keeps a list alive, and a list let go is never taken for a
// later one at its address. ok false where l holds no words list.
func listsKey(l answerLists) (k weak.Pointer[[]string], ok bool) {
	if len(l.words) == 0 {
		return k, false
	}
	return weak.Make(unsafe.SliceData(l.words)), true
}

// listsAsks counts the lists a call searched for a word, the set's index
// keeping none of them (outsideHeld.open), for the rows that bound a
// call's work; setFilters.listBuilds counts the lists a set's index read.
var listsAsks atomic.Int64

// buildLists reads l's words into the set's postings, once for the set,
// counted on its budget; e keeps id -1 past it.
func buildLists(fs *setFilters, s *setDerived, e *listsEntry, l answerLists) {
	e.id = -1
	seen := map[string]bool{}
	var words []string
	for _, ls := range [][][]string{l.words, l.stamped} {
		for _, list := range ls {
			for _, w := range list {
				if !seen[w] {
					seen[w] = true
					words = append(words, w)
				}
			}
		}
	}
	var post int64
	for _, w := range words {
		post += postBytes(w, true)
	}
	n := allocBytes(unsafe.Sizeof(*e), true) + listsSlotBytes + allocBytes(uintptr(cap(words))*unsafe.Sizeof(""), true) + post
	fs.mu.Lock()
	groups := fs.firstGroups()
	fs.mu.Unlock()
	if !setClaim(s, n+groups) {
		return
	}
	fs.mu.Lock()
	fs.listsGrouped = fs.listsGrouped || groups > 0
	id := fs.nextList
	fs.nextList++
	for _, w := range words {
		fs.post[w] = append(fs.post[w], id)
	}
	fs.listBuilds++
	fs.postLive += listsSlotBytes + post
	fs.mu.Unlock()
	e.id, e.words, e.bytes, e.post = id, words, n, post
}

// listsGone is what a list's cleanup needs: the set and its filters, and
// the list's key, all weakly, so the cleanup keeps none of them alive.
type listsGone struct {
	set weak.Pointer[setDerived]
	fs  weak.Pointer[setFilters]
	key weak.Pointer[[]string]
}

// dropLists takes a list the set let go out of its index: its entry, its id
// from its words' postings, and its bytes from the set's budget, but for
// what the index's maps still hold of it: its slot in the map of lists and
// the slot of each word it emptied in the postings (a Go map never
// shrinks), and the id slot of a word another list holds (a list of ids
// keeps its capacity). Those stay counted (postGone) until they pass what
// the live lists count in the maps (postLive), when both maps are built
// anew at their live size, the lists of ids at their length, and give them
// back: the index never holds more than it counts, and counts at most
// twice what its live lists do.
func dropLists(g listsGone) {
	fs := g.fs.Value()
	if fs == nil {
		return
	}
	fs.mu.Lock()
	e := fs.lists[g.key]
	delete(fs.lists, g.key)
	fs.mu.Unlock()
	if e == nil {
		return
	}
	// An entry past the budget (id -1) counts nothing: its drop gives
	// nothing back.
	if e.once.Do(func() { e.id = -1 }); e.id < 0 {
		return
	}
	fs.mu.Lock()
	gone := int64(listsSlotBytes)
	for _, w := range e.words {
		ids := fs.post[w]
		if i, ok := slices.BinarySearch(ids, e.id); ok {
			ids = slices.Delete(ids, i, i+1)
		}
		if len(ids) == 0 {
			delete(fs.post, w)
			gone += postSlotBytes
		} else {
			fs.post[w] = ids
			gone += postIDBytes
		}
	}
	back := e.bytes - gone
	fs.postLive -= listsSlotBytes + e.post
	fs.postGone += gone
	if fs.postGone > fs.postLive {
		lists := make(map[weak.Pointer[[]string]]*listsEntry, len(fs.lists))
		for k, e := range fs.lists {
			lists[k] = e
		}
		post := make(map[string][]int32, len(fs.post))
		for w, ids := range fs.post {
			post[w] = slices.Clone(ids)
		}
		fs.lists, fs.post = lists, post
		back += fs.postGone
		fs.postGone = 0
	}
	fs.mu.Unlock()
	if s := g.set.Value(); s != nil {
		s.mu.Lock()
		s.budget.used -= back
		s.mu.Unlock()
	}
}

// outsideHeld is a call's ask of the set's index: each list of each of its
// outside recordings (dc.outside().words: of, by recording and role), by
// list id the lists of the call it is (byID), those the index keeps none of
// (open: searched as before), and the recordings whose state alone leaves
// them unknown (state).
type outsideHeld struct {
	ls    []heldList
	byID  map[int32][]int32
	open  []int32
	state []int32
	fs    *setFilters
}

// heldList is one list of recording g (its index in the call's outside
// recordings) in its role.
type heldList struct {
	g    int32
	role heldRoles
	l    answerLists
}

// held is the call's outsideHeld, built at its first ask: its outside
// recordings' lists found in the set's index, each list new to it read
// once for the set (buildLists).
func (dc *decider) held() *outsideHeld {
	if dc.oh != nil {
		return dc.oh
	}
	// A call in no set asks empty postings: every list is open.
	oh := &outsideHeld{byID: map[int32][]int32{}, fs: &setFilters{}}
	dc.oh = oh
	for j, g := range dc.outside().words {
		if g.unstated || g.unread {
			oh.state = append(oh.state, int32(j))
		}
		add := func(role heldRoles, l answerLists) {
			if len(l.words) > 0 || len(l.stamped) > 0 {
				oh.ls = append(oh.ls, heldList{g: int32(j), role: role, l: l})
			}
		}
		add(inOwn, g.answerLists)
		add(inView, g.view)
		for _, v := range g.views {
			add(inViews, v)
		}
	}
	var s *setDerived
	if dc.x != nil && dc.x.set != nil {
		s, oh.fs = dc.x.set, dc.x.filters
	}
	es := make([]*listsEntry, len(oh.ls))
	if fs := oh.fs; s != nil {
		keys := make([]weak.Pointer[[]string], len(oh.ls))
		has := make([]bool, len(oh.ls))
		for i, h := range oh.ls {
			keys[i], has[i] = listsKey(h.l)
		}
		fs.mu.Lock()
		for i := range oh.ls {
			if !has[i] {
				continue
			}
			e := fs.lists[keys[i]]
			if e == nil {
				e = &listsEntry{}
				fs.lists[keys[i]] = e
				runtime.AddCleanup(unsafe.SliceData(oh.ls[i].l.words), dropLists, listsGone{set: weak.Make(s), fs: weak.Make(fs), key: keys[i]})
			}
			es[i] = e
		}
		fs.mu.Unlock()
	}
	for i, e := range es {
		if e != nil {
			e.once.Do(func() { buildLists(oh.fs, s, e, oh.ls[i].l) })
		}
		if e == nil || e.id < 0 {
			oh.open = append(oh.open, int32(i))
			continue
		}
		oh.byID[e.id] = append(oh.byID[e.id], int32(i))
	}
	return oh
}

// holding is, by outside recording (its index in the call's), the lists of
// it that hold t: those the set's postings of t name, and those the index
// keeps none of that hold it (listsHold).
func (oh *outsideHeld) holding(t string) map[int32]heldRoles {
	out := map[int32]heldRoles{}
	oh.fs.mu.Lock()
	ids := slices.Clone(oh.fs.post[t])
	oh.fs.mu.Unlock()
	for _, id := range ids {
		for _, i := range oh.byID[id] {
			out[oh.ls[i].g] |= oh.ls[i].role
		}
	}
	for _, i := range oh.open {
		listsAsks.Add(1)
		if listsHold(oh.ls[i].l, t) {
			out[oh.ls[i].g] |= oh.ls[i].role
		}
	}
	return out
}

// asked is, in order, the outside recordings a word's check reads: those
// whose lists hold one of the words (in), and those whose state alone
// leaves them unknown where state is asked.
func (oh *outsideHeld) asked(state bool, in ...map[int32]heldRoles) []int32 {
	var gs []int32
	if state {
		gs = slices.Clone(oh.state)
	}
	for _, m := range in {
		for g := range m {
			gs = append(gs, g)
		}
	}
	slices.Sort(gs)
	return slices.Compact(gs)
}

// placeIndex is a call's ask of the set's postings of its texts' words
// (postRuns) for its places (dc.ps): by a text's id there, the places
// whose recorded text (rec) or round F's (live) it is, and the places a
// word's check always reads (always). A word's check reads only the places
// whose recorded text may hold R's word, as one that holds a whole token
// around it does (asked), or whose round F's text may hold F's word, and
// the places always read: any other holds neither word, so it shows
// nothing of it (evidenceOf: evNone) and no token around it (tokenShown),
// as the filter of its texts would tell (ruledOut), with no lookup per
// place. The postings hold a text shorter than filterTextMin too, by its
// words alone (shortEntry): an exchange's leaves are mostly such texts,
// and read always, each was copied and asked for every word of every call,
// nearly all of a word's check on api-server's round F. Read always: a
// place whose texts the postings do not hold (one keploy could not read,
// one past the set's budget, a call in no set), and one a word may show at
// without its texts holding it (showsUntold): F's side unstated where F served it with no
// view told or read (fMay) or with views (its views decide), a body that
// declares no media type (gluedAt), a given (glued inside the word). A
// place F's side leaves unstated otherwise (a recording round F never
// served) shows a word only where one of its texts holds it (evidenceOf), so
// the postings name it as they name any place: read for every word, each of
// thousands of recordings round F had not served was copied and asked for
// every word of every call, most of the learn step's work on api-server's
// round F.
type placeIndex struct {
	rec, live map[int32][]int32
	always    []int32
	fs        *setFilters
}

// places is the call's placeIndex, built at its first ask: each place's
// texts' filters built, as their first ask builds them (setFilter).
func (dc *decider) places() *placeIndex {
	if dc.pi != nil {
		return dc.pi
	}
	// A call in no set asks empty postings: every place's texts are
	// unposted (postID), and every place is read.
	pi := &placeIndex{rec: map[int32][]int32{}, live: map[int32][]int32{}, fs: cmp.Or(dc.x.filters, &setFilters{})}
	dc.pi = pi
	placeBuilds.Add(1)
	for i, p := range dc.ps {
		r, l := int32(-1), int32(-1)
		if !p.showsUntold() {
			r, l = p.t.rec.postID(), p.t.live.postID()
		}
		if r < 0 || l < 0 {
			pi.always = append(pi.always, int32(i))
			continue
		}
		pi.rec[r] = append(pi.rec[r], int32(i))
		pi.live[l] = append(pi.live[l], int32(i))
	}
	return pi
}

// showsUntold reports whether a word may show at p without either of its
// texts holding it (placeIndex's read always): round F served it with no
// view told (fMay) or with views of its own (evidenceOf reads both as
// unknown whatever its texts hold; placesOf sets either only where F's side
// is unstated), a body that declares no media type (gluedAt), or a given
// (glued inside the word).
func (p evPlace) showsUntold() bool {
	return p.fMay || len(p.views) > 0 || p.undeclared || p.gave
}

// asked is the places a check of w reads (placeIndex), ascending; all
// where it reads every place: R's word or F's has no run of letters and
// digits the postings can name. A place that shows a whole token around
// R's word (tokenShown) is among those the postings name for R's word:
// each of the word's runs is a whole run of the token (recAsked), and a
// text that holds a token of either kind holds each of its runs whole
// (holdsToken; an instant never starts or ends glued to a letter or digit,
// so a base64 run cut at one's bound ends a run of letters and digits
// there too).
func (pi *placeIndex) asked(w readWord) (at []int32, all bool) {
	if !pi.mark(&at, pi.rec, runsOf(w.Rec)) || !pi.mark(&at, pi.live, runsOf(w.Live)) {
		return nil, true
	}
	return pi.ascending(at), false
}

// recAsked is the places whose recorded text may hold r as a word
// (holdsWord), as asked's does for R's word, and those read always,
// ascending; all where r has no run of letters and digits the postings can name. A text
// that holds r as a word in any reading holds each of r's runs whole: every
// reading reads a letter or digit as a word byte, so none stands right
// before or after r.
func (pi *placeIndex) recAsked(r string) (at []int32, all bool) {
	if !pi.mark(&at, pi.rec, runsOf(r)) {
		return nil, true
	}
	return pi.ascending(at), false
}

// ascending is at and the places read always, each once, in the call's
// order of places: a check reads them in that order, as it read every
// place before the postings named any. It is as long as the places the
// word's check reads, never the call's: a slot per place of the call for
// every word of a call of thousands of places cost more than the reads.
func (pi *placeIndex) ascending(at []int32) []int32 {
	at = append(at, pi.always...)
	slices.Sort(at)
	return slices.Compact(at)
}

// askedPlaces is at, or where all is set every place of n, in order.
func askedPlaces(at []int32, all bool, n int) []int32 {
	if !all {
		return at
	}
	at = make([]int32, n)
	for i := range at {
		at[i] = int32(i)
	}
	return at
}

// mark adds to ask the places whose text on side (rec or live) the set's
// postings say may hold every run of runs; false where runs is empty, which
// every text may hold.
func (pi *placeIndex) mark(ask *[]int32, side map[int32][]int32, runs []string) bool {
	ids, ok := pi.fs.holdingRuns(runs)
	for _, id := range ids {
		*ask = append(*ask, side[id]...)
	}
	return ok
}

// runsOf is s's runs of letters and digits, as eachWord cuts them.
func runsOf(s string) []string {
	var runs []string
	for i := 0; i < len(s); {
		if !isWord(s[i]) {
			i++
			continue
		}
		j := i + 1
		for j < len(s) && isWord(s[j]) {
			j++
		}
		runs = append(runs, s[i:j])
		i = j
	}
	return runs
}

// placeAsks counts the places a word's check reads (heldAt), and
// placeBuilds the calls' place indexes built (places), for the rows that
// bound a call's work.
var placeAsks, placeBuilds atomic.Int64

// viewNew reports whether round F's view of g holds a word its recording
// does not: F gave something new there.
func viewNew(g givenWords) bool {
	for i, list := range g.view.words {
		if slices.ContainsFunc(list, func(t string) bool { return !g.holds(i, t, true) }) {
			return true
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
// A leaf of a client test's request (Earlier.incoming: mock is one the
// running test took) is read at the place its test case binds a variable
// at (seenVar), as the learn step bound it there (Place.Var): its leaves are
// named by their paths alone (GivenRequest), so Seen.Place holds none.
func (t *Tracker) BoundWords(n uint64, mock string, s Seen) []BoundWord {
	if t == nil || s.Side != Request {
		return nil
	}
	rec, live := variables.Words(s.Leaf.Value), variables.Words(s.Live)
	if len(rec) != len(live) {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	place := s.Place
	if place == "" && t.tookRequestLocked(mock) {
		place = seenVar(Earlier{incoming: true}, s)
	}
	if place == "" {
		return nil
	}
	var out []BoundWord
	for k := range rec {
		key := mock + "\x00" + place + "#" + strconv.Itoa(k)
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

// tookRequestLocked reports whether mock is the place of a client test's
// request the running test took (Incoming): one of t.incoming. t.mu is held.
func (t *Tracker) tookRequestLocked(mock string) bool {
	for _, e := range t.incoming {
		if e.Mock == mock {
			return true
		}
	}
	return false
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
