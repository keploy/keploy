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
//     place recorded strictly before the call (one made at the call's own
//     instant may have been given after it; one whose recorded time keploy
//     cannot read shows nothing), F by a place it served (a recording F did
//     not serve, F did not give);
//   - the recorded value and the value F saw do not both hold it (one holds
//     it: that is why the rule found it; the other must lack it). The two
//     values differ wherever the rule finds a changed word at all;
//   - the recording and F gave the application the places before the call
//     in the same order (recordedOrder, servedOrder), so an application that
//     copies an answer by its position (the last one it got) took the same
//     place's word in both, and a value that lacks it shows no copy of it.
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
// recorded text there where it recorded e strictly before the call (the test
// case's own request: before every call of the test), and F gave it what it
// served there (Seen.Live).
func (f *follows) excusedSeen(e Earlier, ed *valueDrift, i int, t string, at time.Time, why string) bool {
	if f == nil {
		return false
	}
	before, untimed := e.Given, ""
	if !e.Given {
		before, untimed = recordedBefore(e.At, at, e.Mock)
	}
	return f.excused(t, before && ed.recTok[i][t], ed.liveTok[i][t], untimed, why)
}

// excusedRaw is excused of word t, found in reading i in x's payload: the
// recording gave the application the payload where it recorded x strictly
// before the call, and F gave it where it served x. A payload's clock stamps
// are read blanked: F may have stamped its own there, so a word that stands
// only in one is no word both gave.
func (f *follows) excusedRaw(x Raw, i int, t string, at time.Time, why string) bool {
	if f == nil {
		return false
	}
	before, untimed := recordedBefore(x.at, at, x.mock)
	held := f.dc.searchedTokens(x, false)[i][t]
	return f.excused(t, before && held, x.served && held, untimed, why)
}

// recordedBefore reports whether a place recorded at rat was given to the
// recorded application before a call recorded at at: recorded strictly
// before it. untimed says why it cannot be told: no recorded time.
func recordedBefore(rat, at time.Time, mock string) (before bool, untimed string) {
	if rat.IsZero() {
		return false, "keploy cannot read when the recording gave the application " + mock + " (its mock holds no recorded time)"
	}
	return rat.Before(at), ""
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
// call recorded at at, in the order it gave them: the test case's request
// first; then every place recorded strictly before the call, served by F or
// not, by when it was recorded. untold, where that order cannot be told,
// says why: two places of different mocks recorded at one instant, or a
// place whose recorded time keploy cannot read.
func recordedOrder(at time.Time, earlier []Earlier, raw []Raw) (order []string, untold string) {
	type place struct {
		at   time.Time
		mock string
	}
	var places []place
	add := func(mock string, rat time.Time) string {
		before, untimed := recordedBefore(rat, at, mock)
		if untimed != "" {
			return untimed
		}
		if before {
			places = append(places, place{rat, mock})
		}
		return ""
	}
	for _, e := range earlier {
		if e.Given {
			order = append(order, "the test case's request")
			continue
		}
		if why := add(e.Mock, e.At); why != "" {
			return nil, why
		}
	}
	for _, x := range raw {
		if why := add(x.mock, x.at); why != "" {
			return nil, why
		}
	}
	sort.SliceStable(places, func(i, j int) bool {
		if !places[i].at.Equal(places[j].at) {
			return places[i].at.Before(places[j].at)
		}
		return places[i].mock < places[j].mock
	})
	for i, p := range places {
		if i > 0 && p.at.Equal(places[i-1].at) && p.mock != places[i-1].mock {
			return nil, "the recording gave the application " + places[i-1].mock + " and " + p.mock + " at one instant, so keploy cannot tell the order it gave them in"
		}
		order = append(order, p.mock)
	}
	return order, ""
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
