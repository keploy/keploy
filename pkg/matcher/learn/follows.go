package learn

import (
	"fmt"
	"sort"
	"time"
)

// A copy follows its source (founder decision, 2026-10-24; the recording is
// the second observation, 20:05). The word rule (unaccounted) refuses a
// drifted value one of whose changed words stands at an earlier place: keploy
// cannot tell whether the value came from there. A small number the
// application computes (Flyway's execution_time, the milliseconds a migration
// took) is refused so whenever its digits happen to be a word of an earlier
// payload ("1" of "SELECT 1", "16" of "PostgreSQL 16.4"), and the set it
// starts goes unjudged.
//
// A copy moves with its source. Record's full round (F) serves the recorded
// answers, so the recording and F gave the application the same earlier
// words, and each made the value once: the recording made the recorded
// value, F the value it saw. Where both gave the application a word at a
// place before the call and one of the two values lacks it, the value did
// not come from that word: that value was made beside the word without
// taking it, and an application that copied it would have copied it there
// too. So F decides alone (ClassifyFollowing), the recording being the other
// observation: a drift the word rule refuses only for such words, and that
// would otherwise be noise, is learned as varying (noise) where
//   - the recording and F both gave the application each word the rule
//     found, at the place it found it, before the call: the recording by a
//     place whose answer it recorded strictly before the call (one requested
//     at the call's own instant, or still in flight when the application
//     made the call, may have been given after it; one whose recorded
//     request or answer time keploy cannot read shows nothing), F by a place
//     it served (a recording F did not serve, F did not give);
//   - the recorded value and the value F saw do not both hold it (one holds
//     it: that is why the rule found it; the other must lack it). The two
//     values differ wherever the rule finds a changed word at all;
//   - the recording and F gave the application the places before the call
//     in the same order (recordedOrder, servedOrder), so an application that
//     copies an answer by its position (the last one it got) took the same
//     place's word in both, and a value that lacks it shows no copy of it.
//     The recording shows the order of its answers only where each was
//     recorded before the next request, or where requests in flight together
//     were carried on one connection at known positions (Raw.SentOn).
//
// Otherwise the value is refused, the reason naming which of these failed.
// Nothing is read from a value's shape. Everything else the classifier
// refuses (an offset, an instant, a glued word of an undeclared body, a word
// of another test's or of a recording made after the call, an echo in the
// answer) stays refused: the rule reads the word rule's plain matches of the
// call's own earlier places alone.
//
// No later round decides anything: the confirming round (C), a child keploy
// test, runs no learn step, and nothing carries F's reading to it. So F never
// answers a call provisionally; what it cannot decide, it refuses.

// follows is the word rule's reading of one drifted value in record's full
// round under the rule a copy follows its source: nil reads as today,
// refusing every match. A match is let go (excused) only where the recording
// and F both gave the application the word at its place before the call and
// the two values do not both hold it; otherwise it is refused, saying why
// (refused).
type follows struct {
	dc *DriftCache
	// rec is the recorded value, live the value F saw.
	rec, live string
	// first is the word rule's reason at the first match let go: what the
	// reason of a refusal for the order names.
	first   string
	refused string
}

// excused reports whether the word rule may pass over word t of the value,
// found at a place whose reason there is why: recGave, whether the
// recording gave the application t there before the call; fGave, whether
// record's full round did; untimed, of a place the recording gave, says why
// keploy cannot tell when it did ("" where it can).
func (f *follows) excused(t string, recGave, fGave bool, untimed, why string) bool {
	if f == nil {
		return false
	}
	switch {
	case untimed != "":
		f.refused = untimed + ", so the recording cannot show the value does not follow that word"
	case !recGave:
		f.refused = "the recording did not give the application that word there before the call, so it cannot show the value does not follow it"
	case !fGave:
		f.refused = "record's full round did not give the application that word there before the call, so it cannot show the value does not follow it"
	case f.dc.holdsWord(f.rec, t) && f.dc.holdsWord(f.live, t):
		f.refused = "the recorded value and the value record's full round saw both hold that word, and both gave it there, so neither shows the value does not follow it"
	default:
		if f.first == "" {
			f.first = why
		}
		return true
	}
	return false
}

// excusedSeen is excused of word t, found in reading i at a leaf of an
// exchange record's full round served before the call (e), whose recorded
// and served text ed reads: the recording gave the application the leaf's
// recorded text there where it recorded e's answer strictly before the call
// (recordedGave; the test case's own request: before every call of the
// test), and F gave it what it served there (Seen.Live).
func (f *follows) excusedSeen(e Earlier, ed *valueDrift, i int, t string, at time.Time, why string) bool {
	if f == nil {
		return false
	}
	before, untimed := e.Given, ""
	if !e.Given {
		before, untimed = recordedGave(e.At, e.Payload.answered, at, e.Mock)
	}
	return f.excused(t, before && ed.recTok[i][t], ed.liveTok[i][t], untimed, why)
}

// excusedRaw is excused of word t, found in reading i in x's payload: the
// recording gave the application the payload where it recorded x's answer
// strictly before the call (recordedGave), and F gave it where it served x. A payload's clock stamps
// are read blanked: F may have stamped its own there, so a word that stands
// only in one is no word both gave.
func (f *follows) excusedRaw(x Raw, i int, t string, at time.Time, why string) bool {
	if f == nil {
		return false
	}
	before, untimed := recordedGave(x.at, x.answered, at, x.mock)
	held := f.dc.searchedTokens(x, false)[i][t]
	return f.excused(t, before && held, x.served && held, untimed, why)
}

// recordedGave reports whether the recording gave the application a place
// whose request was recorded at req and its answer at answered before a call
// recorded at at: answered strictly before the call's request was recorded.
// A request in flight when the application made the call (recorded before
// it, answered at or after it) gave the application nothing before the call,
// so the recorded value shows nothing of its answer's words. untimed says
// why it cannot be told: no recorded time, or no answer time keploy can read
// (none, or one before its request).
func recordedGave(req, answered, at time.Time, mock string) (before bool, untimed string) {
	switch {
	case req.IsZero():
		return false, "keploy cannot read when the recording gave the application " + mock + " (its mock holds no recorded time)"
	case answered.IsZero() || answered.Before(req):
		return false, "keploy cannot read when the recording answered " + mock + " (its mock holds no answer time it can read)"
	}
	return answered.Before(at), ""
}

// holdsWord reports whether value holds t as a word, in any reading and
// any declared form.
func (dc *DriftCache) holdsWord(value, t string) bool {
	d := dc.of(value, value)
	for i := range d.liveTok {
		if d.liveTok[i][t] {
			return true
		}
	}
	return false
}

// followed is the verdict on a drift record's full round would learn as
// noise once the word rule let go the words fol found (fol.first non-empty):
// noise where the recording and F gave the application the places before
// the call in the same order; refused otherwise, saying why. earlier and raw
// are every place the classifier was handed (the call's own and those F gave
// elsewhere in the set).
func (fol *follows) followed(ln Learned, at time.Time, earlier []Earlier, raw []Raw) Learned {
	rec, untoldRec := recordedOrder(at, earlier, raw)
	f, untoldF := servedOrder(earlier, raw)
	why := ""
	switch {
	case untoldRec != "":
		why = untoldRec
	case untoldF != "":
		why = untoldF
	default:
		if d := orderDiff(rec, f); d != "" {
			why = "the recording and record's full round gave the application the earlier answers in a different order (" + d + ")"
		}
	}
	if why != "" {
		ln.Kind, ln.Why, ln.Echo = KindUnaccounted, fol.first+"; "+why+", so keploy cannot tell a copy from a varying value", nil
	}
	return ln
}

// ClassifyFollowing is ClassifyGiven of record's full round under the rule a
// copy follows its source: a drift ClassifyGiven refuses only for words the
// word rule found at earlier places of the call's own test, and would
// otherwise learn as noise, is noise where the recording and F show the
// value no copy of any of them (follows.go), and refused otherwise, saying
// why. Every other reading is ClassifyGiven's. It decides in F alone: nothing
// is left for a later round.
func ClassifyFollowing(dc *DriftCache, drift []string, live, rec Shape, at time.Time, earlier []Earlier, raw []Raw, resp []Leaf, w *Window, given GivenEarlier) []Learned {
	return classifyGiven(dc, drift, live, rec, at, earlier, raw, resp, w, given, true)
}

// recordedOrder is the places the recording gave the application before a
// call recorded at at, in the order it gave them their answers: the test
// case's request first; then every place whose answer was recorded strictly
// before the call (recordedGave), served by F or not. The recording shows the
// order of two places where one was answered strictly before the other's
// request was recorded (keploy stamped its answer first), or, for two
// requests in flight together, where both were carried on one connection at
// known, distinct positions there (Raw.SentOn) that agree with when their
// requests were recorded: its protocol answers them in the order they were
// sent, and a client that sends a block of statements in one write gives
// them one recorded instant. A position orders only requests in flight
// together; it is no second clock. untold, where the order of any two places
// of different mocks cannot be told, says why: two requests in flight
// together on two connections, or at an unknown or shared position (the
// order of their answers is then the dependency's and the network's, which
// the recording does not hold); a place whose recorded request or answer
// time keploy cannot read.
func recordedOrder(at time.Time, earlier []Earlier, raw []Raw) (order []string, untold string) {
	var places []recPlace
	add := func(p recPlace) string {
		before, untimed := recordedGave(p.at, p.answered, at, p.mock)
		if untimed != "" {
			return untimed
		}
		if before {
			places = append(places, p)
		}
		return ""
	}
	for _, e := range earlier {
		if e.Given {
			order = append(order, "the test case's request")
			continue
		}
		if why := add(recPlace{e.Mock, e.At, e.Payload.answered, e.Payload.conn, e.Payload.pos}); why != "" {
			return nil, why
		}
	}
	for _, x := range raw {
		if why := add(recPlace{x.mock, x.at, x.answered, x.conn, x.pos}); why != "" {
			return nil, why
		}
	}
	// By when each request was recorded, and among those recorded at one
	// instant on one connection by position; the name only makes the sort
	// stable, never an order: two places it alone orders are refused below.
	sort.SliceStable(places, func(i, j int) bool {
		a, b := places[i], places[j]
		switch {
		case !a.at.Equal(b.at):
			return a.at.Before(b.at)
		case a.conn != b.conn:
			return a.conn < b.conn
		case a.pos != b.pos:
			return a.pos < b.pos
		}
		return a.mock < b.mock
	})
	// Every place must be told after every one sorted before it. Where each
	// earlier one was answered before its request (the usual sequential
	// application), one comparison with the latest answer says so.
	var latest time.Time
	for j, p := range places {
		if !latest.Before(p.at) {
			for _, q := range places[:j] {
				if why := answeredBefore(q, p); why != "" {
					return nil, why
				}
			}
		}
		if p.answered.After(latest) {
			latest = p.answered
		}
		order = append(order, p.mock)
	}
	return order, ""
}

// recPlace is a place the recording gave the application before the call
// (recordedOrder): its mock, when its request and its answer were recorded
// (both read: recordedGave), and where its request was carried (Raw.SentOn).
type recPlace struct {
	mock         string
	at, answered time.Time
	conn         string
	pos          uint64
}

// answeredBefore says why keploy cannot tell that the recording gave the
// application q's answer before p's, q sorted before p (recordedOrder); ""
// where it can: one recording; q answered strictly before p's request was
// recorded; or both carried on one connection, at known and distinct
// positions, q's first.
func answeredBefore(q, p recPlace) string {
	switch {
	case q.mock == p.mock:
		return ""
	case q.answered.Before(p.at):
		return ""
	case q.conn != "" && q.conn == p.conn && q.pos != 0 && p.pos != 0 && q.pos != p.pos:
		if q.pos < p.pos {
			return ""
		}
		return "the recording carried " + q.mock + " and " + p.mock + " on one connection at positions that disagree with when their requests were recorded, so keploy cannot tell the order it gave them in"
	case q.at.Equal(p.at):
		return "the recording gave the application " + q.mock + " and " + p.mock + " at one instant, so keploy cannot tell the order it gave them in"
	}
	return "the recording's requests for " + q.mock + " and " + p.mock + " were in flight together (" + p.mock + "'s was recorded before " + q.mock + " was answered), so keploy cannot tell which answer it gave the application first"
}

// servedOrder is the places record's full round gave the application before
// the call, in the order it gave them: the test case's request first; then
// what it served (every Earlier, and each Raw it served, whenever recorded),
// in the one order it served the set's mocks in (Raw.ServedIn, the first and
// the last of each mock's serves; an exchange's by its payload's,
// InServeOrder). A recording it did not serve it did not give. untold, where
// that order cannot be told, says why: a served place the order does not
// number, beside another it served (two serves it gave in an order keploy
// cannot establish are unordered, never ordered by when keploy noted them),
// or a mock served more than once with a serve between its first and its
// last that is no place's (it may have been that mock's own, unnumbered).
func servedOrder(earlier []Earlier, raw []Raw) (order []string, untold string) {
	type serve struct {
		n    int
		mock string
	}
	type span struct {
		first, last int
		mock        string
	}
	var serves []serve
	var spans []span
	unordered := ""
	placed := 0
	add := func(mock string, first, last int) {
		placed++
		if first == 0 {
			unordered = mock
			return
		}
		serves = append(serves, serve{first, mock})
		if last != first {
			serves = append(serves, serve{last, mock})
			spans = append(spans, span{first, last, mock})
		}
	}
	for _, e := range earlier {
		switch {
		case e.Given:
			order = append(order, "the test case's request")
		case e.Payload.Read():
			add(e.Mock, e.Payload.first, e.Payload.last)
		default:
			add(e.Mock, 0, 0)
		}
	}
	for _, x := range raw {
		if x.served {
			add(x.mock, x.first, x.last)
		}
	}
	if unordered != "" && placed > 1 {
		return nil, "keploy does not know where record's full round served " + unordered + " among the other earlier answers it served"
	}
	sort.SliceStable(serves, func(i, j int) bool {
		if serves[i].n != serves[j].n {
			return serves[i].n < serves[j].n
		}
		return serves[i].mock < serves[j].mock
	})
	var nums []int
	for _, v := range serves {
		if len(nums) == 0 || nums[len(nums)-1] != v.n {
			nums = append(nums, v.n)
		}
	}
	for _, s := range spans {
		// The serves strictly between s's first and last that some place's
		// first or last numbers: fewer than the numbers between leaves a
		// serve keploy cannot place, which may be s's mock's own.
		between := sort.SearchInts(nums, s.last) - sort.SearchInts(nums, s.first+1)
		if between < s.last-s.first-1 {
			return nil, "record's full round served " + s.mock + " more than once, with a serve between that is no earlier answer's, which keploy cannot place"
		}
	}
	for _, v := range serves {
		order = append(order, v.mock)
	}
	return order, ""
}

// orderDiff says where the recording's order of the places it gave the
// application (rec, recordedOrder) and record's full round's (f,
// servedOrder) first differ; "" where they do not.
func orderDiff(rec, f []string) string {
	for i := 0; i < len(rec) || i < len(f); i++ {
		switch {
		case i >= len(rec):
			return fmt.Sprintf("the recording gave %d earlier answers, record's full round more", len(rec))
		case i >= len(f):
			return fmt.Sprintf("record's full round gave %d earlier answers, the recording more", len(f))
		case rec[i] != f[i]:
			return fmt.Sprintf("the %s earlier answer was %s in the recording and %s in record's full round", ordinal(i+1), rec[i], f[i])
		}
	}
	return ""
}

// ordinal is n as an English ordinal: 1st, 2nd, 3rd, 4th, 11th, 21st.
func ordinal(n int) string {
	suffix := "th"
	switch {
	case n%100 >= 11 && n%100 <= 13:
	case n%10 == 1:
		suffix = "st"
	case n%10 == 2:
		suffix = "nd"
	case n%10 == 3:
		suffix = "rd"
	}
	return fmt.Sprintf("%d%s", n, suffix)
}
