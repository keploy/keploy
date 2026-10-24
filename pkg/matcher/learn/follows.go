package learn

import (
	"fmt"
	"sort"
	"time"
)

// A copy follows its source (founder decision, 2026-10-24). The word rule
// (unaccounted) refuses a drifted value one of whose changed words stands at
// an earlier place: keploy cannot tell whether the value came from there. A
// small number the application computes (Flyway's execution_time, the
// milliseconds a migration took) is refused so whenever its digits happen to
// be a word of an earlier payload ("1" of "SELECT 1", "16" of "PostgreSQL
// 16.4"), and the set it starts goes unjudged.
//
// A copy moves with its source. So where record's full round (F) and the
// confirming round (C) both give the application the same word at a place,
// and the value lacks that word in one of them, the value did not come from
// that word: the match is no evidence. Where every word the rule found is so
// shown no copy, and the value F saw differs from the one C saw (two live
// observations that differ), the value is learned as varying (noise).
// Nothing is read from a value's shape.
//
// F alone never decides: a word F's value holds can only be shown no copy
// by a round whose value lacks it. So F answers such a call provisionally
// (KindPending, ClassifyFollowing), carrying what it saw (Pending), and C
// decides (Pending.Decide). A pending reading is never a verdict: where C
// does not decide it, the call stays refused with Pending's reason.
// Everything else the classifier refuses (an offset, an instant, a glued
// word of an undeclared body, a word of another test's, an echo in the
// answer) stays refused in both rounds: the rule reads the word rule's
// plain matches alone.

// KindPending is a drift record's full round could learn as noise but for
// words the word rule found at earlier places: answered provisionally,
// varying or a copy undecided until the confirming round decides
// (Pending.Decide). It is never a verdict.
const KindPending Kind = "pending"

// Pending is what record's full round saw of a drifted leaf it answered
// provisionally (KindPending): what the confirming round needs to decide
// whether the value follows the words the word rule found. It holds values
// and words, which no reason names: it is kept beside the mock until the
// confirming round reads it, never written into a reason.
type Pending struct {
	// Path is the leaf, Recorded its recorded value, Live the value record's
	// full round saw there.
	Path     string `json:"path" yaml:"path"`
	Recorded string `json:"recorded" yaml:"recorded"`
	Live     string `json:"live" yaml:"live"`
	// Words are the words the word rule found at earlier places.
	Words []PendingWord `json:"words" yaml:"words"`
	// Raw are the recordings record's full round read before the call as
	// payloads (Raw), by mock; Seen the leaves of the exchanges it served
	// before it (Earlier.Seen), each with the value it saw there.
	Raw  []string       `json:"raw,omitempty" yaml:"raw,omitempty"`
	Seen []PendingPlace `json:"seen,omitempty" yaml:"seen,omitempty"`
	// Order is the places record's full round gave the application before
	// the call, in the order it gave them (givenOrder); Untold, where that
	// order cannot be told, says why. The confirming round learns noise only
	// where it gave the same places in the same order.
	Order  []string `json:"order,omitempty" yaml:"order,omitempty"`
	Untold string   `json:"untold,omitempty" yaml:"untold,omitempty"`
}

// PendingWord is one word the word rule found at an earlier place in a
// round: in the value's reading Reading, on its recorded side (Recorded) or
// as the round saw it, at Mock (and Path, a leaf of an exchange served
// before the call; "" for a recording's payload). Why is the word rule's
// reason there. ByF: record's full round gave the application the word
// there while its value lacked it, so it already shows the value is no copy
// of it.
type PendingWord struct {
	Word     string `json:"word" yaml:"word"`
	Reading  int    `json:"reading" yaml:"reading"`
	Recorded bool   `json:"recorded,omitempty" yaml:"recorded,omitempty"`
	Mock     string `json:"mock" yaml:"mock"`
	Path     string `json:"path,omitempty" yaml:"path,omitempty"`
	Why      string `json:"why" yaml:"why"`
	ByF      bool   `json:"byF,omitempty" yaml:"byF,omitempty"`
}

// PendingPlace is a leaf of an exchange served before the call, with the
// value the round saw there.
type PendingPlace struct {
	Mock string `json:"mock" yaml:"mock"`
	Path string `json:"path" yaml:"path"`
	Live string `json:"live" yaml:"live"`
}

// followMode is how classifyGiven reads the word rule's matches: as today
// (followNone), as record's full round under the rule a copy follows its
// source (followF), or as the confirming round deciding a pending reading
// (followC).
type followMode int

const (
	followNone followMode = iota
	followF
	followC
)

// follows is the word rule's reading of one drifted value under the rule a
// copy follows its source (unaccounted): nil reads as today, refusing every
// match. In record's full round (pending nil) every plain match is kept as a
// word to decide (words); in the confirming round a match is let go only
// where a round shows the value is no copy of it (excused), and refused
// otherwise, saying why (refused).
type follows struct {
	dc      *DriftCache
	at      time.Time
	served  []Earlier
	raw     []Raw
	live    string
	pending *Pending
	words   []PendingWord
	refused string
}

// excused reports whether the word rule may pass over word t of the value,
// found in reading i (on its recorded side: recorded) at mock (at path, a
// leaf F served; "" for a payload), whose reason there is why. In record's
// full round it always may, and keeps the word for the confirming round. In
// the confirming round it may only where record's full round or this round
// gave the application t there while its value lacked it.
func (f *follows) excused(i int, t string, recorded bool, mock, path, why string) bool {
	if f == nil {
		return false
	}
	gaveHere := f.gave(i, t, mock, path)
	if f.pending == nil {
		f.words = append(f.words, PendingWord{Word: t, Reading: i, Recorded: recorded, Mock: mock, Path: path, Why: why,
			ByF: gaveHere && !f.dc.holdsWord(f.live, t)})
		return true
	}
	if gaveHere && !f.dc.holdsWord(f.live, t) {
		return true
	}
	if f.pending.gave(f.dc, i, t, mock, path, f.raw) && !f.dc.holdsWord(f.pending.Live, t) {
		return true
	}
	f.refused = "record's full round and the confirming round both saw a value holding that word wherever the place gave it, so neither shows the value does not follow it"
	return false
}

// gave reports whether this round gave the application t, in reading i, at
// mock (at path; "" for a recording's payload) before the call. A payload's
// clock stamps are read blanked: a word that stands only in one is no word
// it gave.
func (f *follows) gave(i int, t, mock, path string) bool {
	if path == "" {
		for _, x := range f.raw {
			if x.mock == mock && x.earlierThan(f.at) {
				return f.dc.searchedTokens(x, false)[i][t]
			}
		}
		return false
	}
	for _, e := range f.served {
		if e.Mock != mock {
			continue
		}
		for _, s := range e.Seen {
			if s.Leaf.Path == path {
				return f.dc.of(s.Live, s.Live).liveTok[i][t]
			}
		}
	}
	return false
}

// gave reports whether record's full round gave the application t, in
// reading i, at mock (at path; "" for a recording's payload) before the
// call. A payload is the recording's own text in every round: raw is this
// round's, read for its words.
func (p *Pending) gave(dc *DriftCache, i int, t, mock, path string, raw []Raw) bool {
	if path == "" {
		in := false
		for _, m := range p.Raw {
			if m == mock {
				in = true
				break
			}
		}
		if !in {
			return false
		}
		for _, x := range raw {
			if x.mock == mock {
				return dc.searchedTokens(x, false)[i][t]
			}
		}
		return false
	}
	for _, s := range p.Seen {
		if s.Mock == mock && s.Path == path {
			return dc.of(s.Live, s.Live).liveTok[i][t]
		}
	}
	return false
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

// pendingOf is what record's full round keeps of a drifted leaf it answers
// provisionally (Pending): the words f found, and the places it read.
func pendingOf(r, l Leaf, at time.Time, served []Earlier, raw []Raw, words []PendingWord) *Pending {
	p := &Pending{Path: r.Path, Recorded: r.Value, Live: l.Value, Words: words}
	for _, x := range raw {
		if x.earlierThan(at) {
			p.Raw = append(p.Raw, x.mock)
		}
	}
	for _, e := range served {
		for _, s := range e.Seen {
			p.Seen = append(p.Seen, PendingPlace{Mock: e.Mock, Path: s.Leaf.Path, Live: s.Live})
		}
	}
	return p
}

// pendingWhy is the reason of a provisional answer (KindPending): what the
// word rule found, and that the confirming round decides.
func pendingWhy(w PendingWord) string {
	return "pending, varying or a copy undecided: " + w.Why + ". record's full round answered the call provisionally; the confirming round decides whether the value follows that word (a copy moves with its source), and until it does, nothing of it is learned and the call counts as refused"
}

// ClassifyFollowing is ClassifyGiven of record's full round under the rule a
// copy follows its source: a drift ClassifyGiven refuses only for words the
// word rule found at earlier places, and would otherwise learn as noise, is
// KindPending instead, with Learned.Pending for the confirming round to
// decide (Pending.Decide). Every other reading is ClassifyGiven's. A caller
// that does not decide pending readings in the confirming round must not
// call it: a pending reading answered and never decided is a pass nothing
// judged.
func ClassifyFollowing(dc *DriftCache, drift []string, live, rec Shape, at time.Time, earlier []Earlier, raw []Raw, resp []Leaf, w *Window, given GivenEarlier) []Learned {
	out := classifyGiven(dc, drift, live, rec, at, earlier, raw, resp, w, given, followF, nil)
	for _, ln := range out {
		if ln.Pending != nil {
			ln.Pending.Order, ln.Pending.Untold = givenOrder(at, earlier, raw, "record's full round")
		}
	}
	return out
}

// givenOrder is the places a round gave the application before a call
// recorded at at, in the order it gave them: the test case's request first;
// then the recordings no round served (read as places the recording shows
// the application was given: the same in every round), in the order they
// were recorded; then what the round served, in the one order it served the
// set's mocks in (Raw.ServedIn, the first and the last of each mock's
// serves; an exchange's by its payload's, InServeOrder). untold, where that
// order cannot be told, says why, naming the round: a served place the
// order does not number, beside another the round served (two serves the
// round gave in an order keploy cannot establish are unordered, never
// ordered by when keploy noted them), or a mock served more than once with
// a serve between its first and its last that is no place's (it may have
// been that mock's own, unnumbered).
//
// An application that copies an answer by its position (the last one it
// got) takes a different word in each round where the rounds gave the same
// answers in another order, and each round shows the value no copy of the
// word the other took: the order is what tells that copy from a varying
// value (Pending.Decide).
func givenOrder(at time.Time, earlier []Earlier, raw []Raw, round string) (order []string, untold string) {
	type serve struct {
		n    int
		mock string
	}
	type span struct {
		first, last int
		mock        string
	}
	var fixed []Raw
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
		switch {
		case !x.earlierThan(at):
		case !x.served:
			fixed = append(fixed, x)
		default:
			add(x.mock, x.first, x.last)
		}
	}
	if unordered != "" && placed > 1 {
		return nil, "keploy does not know where " + round + " served " + unordered + " among the other earlier answers it served"
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
			return nil, round + " served " + s.mock + " more than once, with a serve between that is no earlier answer's, which keploy cannot place"
		}
	}
	sort.SliceStable(fixed, func(i, j int) bool {
		if !fixed[i].at.Equal(fixed[j].at) {
			return fixed[i].at.Before(fixed[j].at)
		}
		return fixed[i].mock < fixed[j].mock
	})
	for _, x := range fixed {
		order = append(order, x.mock)
	}
	for _, v := range serves {
		order = append(order, v.mock)
	}
	return order, ""
}

// orderDiff says where two rounds' orders of the places they gave the
// application (givenOrder) first differ; "" where they do not.
func orderDiff(f, c []string) string {
	for i := 0; i < len(f) || i < len(c); i++ {
		switch {
		case i >= len(f):
			return fmt.Sprintf("record's full round gave %d earlier answers, the confirming round more", len(f))
		case i >= len(c):
			return fmt.Sprintf("the confirming round gave %d earlier answers, record's full round more", len(c))
		case f[i] != c[i]:
			return fmt.Sprintf("the %s earlier answer was %s in record's full round and %s in the confirming round", ordinal(i+1), f[i], c[i])
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

// Decide is the confirming round's verdict on a drift record's full round
// answered provisionally (KindPending): noise where the value this round saw
// differs from the one F saw, and every word the word rule found, in either
// round, is one a round gave the application while its value lacked it;
// refused (KindUnaccounted) otherwise, saying why. live and rec are the
// leaf this round sent and its recording (a live value equal to the recorded
// one drifted nowhere this round, and is read as this round's value); the
// rest are ClassifyGiven's, of this round.
func (p *Pending) Decide(dc *DriftCache, live, rec Leaf, at time.Time, earlier []Earlier, raw []Raw, resp []Leaf, w *Window, given GivenEarlier) Learned {
	if dc == nil {
		dc = NewDriftCache()
	}
	first := PendingWord{Why: "record's full round answered it provisionally"}
	if len(p.Words) > 0 {
		first = p.Words[0]
	}
	refuse := func(why string) Learned {
		return Learned{Kind: KindUnaccounted, Recorded: rec, Live: live, Why: why}
	}
	switch {
	case rec.Path != p.Path || rec.Value != p.Recorded:
		return refuse(first.Why + "; the confirming round paired the call with another recorded value than record's full round, so it cannot decide whether the value follows that word")
	case live.Value == p.Live:
		return refuse(first.Why + "; the confirming round saw the same value as record's full round, so neither shows the value does not follow that word")
	}
	if live.Value != rec.Value {
		got := classifyGiven(dc, []string{rec.Path}, Shape{Leaves: map[string]Leaf{rec.Path: live}}, Shape{Leaves: map[string]Leaf{rec.Path: rec}}, at, earlier, raw, resp, w, given, followC, p)
		if got[0].Kind != KindNoise {
			why := got[0].Why
			if got[0].Kind != KindUnaccounted {
				why = "the confirming round reads it as " + string(got[0].Kind) + ", while record's full round read it as varying but for words found earlier, so keploy cannot tell which it is"
			}
			return refuse(why)
		}
	}
	served := append([]Earlier(nil), earlier...)
	own, _ := ownPlaces(raw)
	here := &follows{dc: dc, at: at, served: served, raw: own, live: live.Value}
	for _, w := range p.Words {
		if w.ByF || here.gave(w.Reading, w.Word, w.Mock, w.Path) && !dc.holdsWord(live.Value, w.Word) {
			continue
		}
		if !here.gave(w.Reading, w.Word, w.Mock, w.Path) {
			return refuse(w.Why + "; the confirming round did not give the application that word there before the call, so it cannot show the value does not follow it")
		}
		return refuse(w.Why + "; the confirming round's value held that word too, where the place still gave it, so keploy cannot tell the value does not follow it")
	}
	// A copy by position (the last answer the application got) takes
	// another word in a round that gave the answers in another order, and
	// each round shows it no copy of the other's: noise only where both
	// rounds gave the same places in the same order (givenOrder), so the
	// word's source stands at the same position in each.
	order, untold := givenOrder(at, earlier, raw, "the confirming round")
	switch {
	case p.Untold != "":
		return refuse(first.Why + "; " + p.Untold + ", so keploy cannot tell a copy from a varying value")
	case untold != "":
		return refuse(first.Why + "; " + untold + ", so keploy cannot tell a copy from a varying value")
	}
	if d := orderDiff(p.Order, order); d != "" {
		return refuse(first.Why + "; the rounds served the earlier answers in a different order, so keploy cannot tell a copy from a varying value (" + d + ")")
	}
	return Learned{Kind: KindNoise, Recorded: rec, Live: live}
}
