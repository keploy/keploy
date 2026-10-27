package learn

import (
	"fmt"
	"slices"
	"sort"
	"strings"
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
//   - the recording and F gave the application the value's candidate
//     sources before the call in the same order (recordedOrder,
//     servedOrder), so an application that copies an answer by its position
//     (the last one it got) took the same place's word in both, and a value
//     that lacks it shows no copy of it. The candidate sources are the
//     places where the rule found a word of the value (follows.sources): the
//     order exists only to tell which of them the application had last. A
//     place that holds none of those words is no source of either value, so
//     its order is not read, nor whether its request was in flight with a
//     source's (a connection pool's handshake on its own thread beside a
//     migration's statement). The recording shows the order of two sources
//     only where one was recorded answered before the other's request, or
//     where requests in flight together were carried on one connection at
//     known positions (Raw.SentOn); two sources in flight together on two
//     connections are refused.
//
// Otherwise the value is refused, the reason naming which of these failed.
// Nothing is read from a value's shape. The same holds of a word the rule
// finds in the answer of a recording F gave the application before the call
// that no earlier place of the call's own test is (another recorded test's,
// or one F gave earlier in the set: givenWhy, excusedGiven): a group of a
// random id that another test's random id holds by chance is let go where
// the recording gave that answer strictly before the call, F gave the word
// at the same fields, the two values do not both hold it, and the order
// holds; keploy still follows no value of another test's, and a value that
// moved with such a word stays refused. And it holds the other way about: a
// word of the value F saw that F gave the application at fields of an
// earlier payload it served changed, where the recording gave another value
// that the recorded value took none of the words of (excusedLive), is let
// go on the same terms. Everything else the classifier
// refuses (an offset, an instant, a glued word of an undeclared body, a word
// of a recording made after the call, an echo in the answer) stays refused:
// the rule reads the word rule's plain matches alone.
//
// A connection's recorded start-up (isStartUp) is no place the rule reads
// where the start-ups differ (swapHolds: its backend pid and secret key, a
// server parameter set per connection). F gives each connection the
// application opens a recorded start-up by the order the connections open
// in, not by what each sent, so it may give the application's connection
// another connection's start-up: both runs then gave the application the
// same start-ups in the same order, yet not at the same place. A recorded
// value one of whose words stands there is refused (startUpWhy), never
// learned as varying, whatever the order: such a word is never a source
// that can vary. A word every start-up carries alike (the server's version
// and parameters) is read as any recording's: whichever start-up a
// connection got, it carried that word. A value F saw that holds a
// start-up's word the recorded value lacks reads as any other word both
// runs gave.
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
	// at is when the call's request was recorded. refuted, where the search
	// for the value's source went past a place F served unchanged that the
	// value moved away from (refute), names the first such place: what a
	// value learned as noise says of it (followed).
	at      time.Time
	refuted string
	// aside, where the search for the value's source went past a leaf that
	// held its recorded value whole and that F saw hold another value
	// (refuteSeen), names the first such leaf: what a value learned as noise
	// or refused as an echo says of it.
	aside string
	// moved, where the word rule let a word of the value go at a place F
	// gave the application another value at that the value took none of the
	// words of (movedAway), names the first such place: what a value learned
	// as noise says of it, where no place was refuted.
	moved string
	// sources are the mocks of the places the word rule let a word of the
	// value go at (excused), and of the place the search for its source went
	// past (refute): the value's candidate sources, the only places whose
	// order the rule reads (followed).
	sources map[string]bool
	// given are the recordings F gave the application earlier in the set
	// (GivenEarlier) that the word rule let a word of the value go at
	// (excusedGiven): candidate sources no Raw handed to the call stands
	// for, ordered with the rest (followed).
	given []Raw
}

// source notes mock as a candidate source of the value (follows.sources).
func (f *follows) source(mock string) {
	if f.sources == nil {
		f.sources = map[string]bool{}
	}
	f.sources[mock] = true
}

// refute reports whether p, a place in x's payload (texts, as appearedIn
// searched it) holding a word of v's recorded value, is no place the value
// came from: F served x unchanged (Raw.AsRecorded), the recording gave the
// application x before the call (recordedGave), and x does not hold the
// value F saw, in any form. Both runs gave the application x's words there,
// and the value moved while they did not: a copy moves with its source. The
// first such place is named (refuted). Everywhere else (fol nil, a place F
// did not serve or served changed, one the recording did not give before
// the call or whose times keploy cannot read, one holding F's value too)
// the place stays where the value came from. A start-up's word a swap can
// change is never handed here (appearedIn refuses a value found there:
// swapHolds).
//
// A place of a payload F served changed and gave its view of (ServedLive)
// is set aside so too where, at every field of it holding the word, F gave
// the application that word, or another value that the value took none of
// the words of (liveAt, movedAway), and no text F gave there holds the
// value F saw: the value did not move with what F gave in its place.
func (f *follows) refute(x Raw, texts [2]string, keep bool, v *valueDrift, p Place) bool {
	if f == nil {
		return false
	}
	gave := ", which record's full round sent and gave the application as recorded, byte for byte, while the value changed"
	switch {
	case x.AsRecorded():
	case x.Live():
		// Searched unblanked: a stamp or clock read F gave there that holds
		// the value F saw keeps the place too.
		for _, text := range [2]string{x.live.text, f.dc.rawExtra(x.live.text)} {
			if ContainsToken(text, v.live) || v.decoded && ContainsToken(text, v.decLive) {
				return false
			}
		}
		switch fv, _ := f.dc.liveAt(x, texts[0], keep, v, func(own string) bool { return holdsToken(own, p.Held) }); fv {
		case fSame:
			gave = ", where record's full round gave the application that word as recorded, while the value changed"
		case fMoved:
			gave = ", where record's full round gave the application another value, which the value took none of the words of"
		default:
			return false
		}
	default:
		return false
	}
	if before, untimed := recordedGave(x.recordedAt(), x.answered, f.at, x.mock); !before || untimed != "" {
		return false
	}
	for _, text := range texts {
		if ContainsToken(text, v.live) || v.decoded && ContainsToken(text, v.decLive) {
			return false
		}
	}
	if f.refuted == "" {
		found := "its recorded value stands whole in " + placeAt(p)
		if p.Held != v.rec && p.Held != v.decRec {
			found = "a part of its recorded value (" + partOf(v.rec, p.Held) + ") stands in " + placeAt(p)
		}
		f.refuted = found + gave + ": a copy moves with its source, so the value is no copy of it"
	}
	// The word rule reads the same word at the same place on the same terms
	// and notes it there too (excusedRaw); a source set aside is noted here
	// as well, so it is ordered should the two searches ever part.
	f.source(x.mock)
	return true
}

// refuteSeen reports whether s, a leaf of e, an exchange F served before the
// call, that held v's recorded value whole (whole), is no place the value came
// from: an earlier place is a later value's source only where it held that
// value in both runs, and F saw it hold another value than the one F saw at
// v's place (Seen.Live). A copy moves with its source, so the search goes on
// past it; the first such place is named (aside), and the place is a
// candidate source (sources). A value then learned as noise, which no replay
// checks, is noise only where the recording and F gave the application its
// candidate sources in the same order (followed), and an echo of the call's
// answer is refused, both naming the place; a value learned as a read of the
// clock is checked against its call's clock at every replay, which no order
// of its sources changes (classifyRead).
//
// Everywhere else the place stays where the value came from, as before: fol
// nil (the rule's absence), a part of the value (a part that drifted with it
// moved with it in both runs: sources), a leaf F saw hold the value F saw,
// and an instant its protocol types, whose text and the value's can write
// one moment in two ways (dc.instantSource reads its moments). A place set
// aside whose recorded times keploy cannot read, or that the recording
// answered at the call's own instant, is ordered all the same: a value
// learned as noise is then refused, naming it (followed).
func (f *follows) refuteSeen(e Earlier, s Seen, whole bool, v *valueDrift) bool {
	if f == nil || !whole || s.Leaf.Type == Instant || s.Live == v.live {
		return false
	}
	if f.aside == "" {
		saw := ", where record's full round gave the application another value than the value's"
		if s.Side == Request && !e.Given {
			saw = ", where record's full round saw the application send another value than the value's"
		}
		f.aside = "its recorded value stands whole in " + placeAt(seenPlace(e, s, true)) + saw + ": a copy moves with its source, so the value is no copy of it"
	}
	f.source(e.Mock)
	return true
}

// fView is what record's full round shows it gave the application at a
// place holding a word of a drifted value, before the call.
type fView int

const (
	// fNone: F gave the application nothing there that keploy sees (a
	// recording F did not serve, or served changed with no view of it, or a
	// view that lacks the place).
	fNone fView = iota
	// fSame: F gave the application the word there.
	fSame
	// fMoved: F gave the application another value there, which the value
	// took none of the words of (movedAway).
	fMoved
	// fFollowed: F gave the application another value there, which the
	// value took a word of, or drifted with, or which gained no word the
	// recorded one lacked (movedAway says which).
	fFollowed
	// fElsewhere: F gave the application the word at a field of the
	// payload whose recorded text lacks it, or that the recording lacks.
	fElsewhere
	// fSent: the word stands in what F's application sent there (its
	// request), which it made itself, or in a payload whose request and
	// answer keploy cannot tell apart (recordedThere).
	fSent
)

// excused reports whether the word rule may pass over word t of the value,
// found at a place (stands: the word and where it stands, as a reason names
// them) whose reason there is why: recGave, whether the recording gave the
// application t there before the call; fv, what record's full round gave
// there (followed: of fFollowed, why the value follows it; of fElsewhere,
// where); untimed, of a place the recording gave, says why keploy cannot
// tell when it did ("" where it can).
func (f *follows) excused(t string, recGave bool, fv fView, followed, untimed, why, stands string) bool {
	if f == nil {
		return false
	}
	switch {
	case untimed != "":
		f.refused = untimed + ", so the recording cannot show the value does not follow that word"
	case !recGave:
		f.refused = "the recording did not give the application that word there before the call, so it cannot show the value does not follow it"
	case fv == fNone:
		f.refused = "record's full round did not give the application that word there before the call, so it cannot show the value does not follow it"
	case fv == fFollowed:
		f.refused = "record's full round gave the application another value there, " + followed + ", so it cannot show the value does not follow it"
	case fv == fElsewhere:
		f.refused = "record's full round gave the application that word at " + followed + ", where the recording did not, so it cannot show the value does not follow it"
	case f.dc.holdsWord(f.rec, t) && f.dc.holdsWord(f.live, t):
		f.refused = "the recorded value and the value record's full round saw both hold that word, and both gave it there, so neither shows the value does not follow it"
	default:
		if f.first == "" {
			f.first = why
		}
		if fv == fMoved && f.moved == "" {
			f.moved = stands + ", where record's full round gave the application another value, which the value took none of the words of: a copy moves with its source, so the value is no copy of it"
		}
		return true
	}
	return false
}

// movedAway says why a place F gave the application another value at than
// recorded (ed: the recorded value there and the one F gave) does not show
// the drifted value v no copy of it, "" where it does: the place gained a
// word in F (a value to copy), v as F saw it holds none of the words the
// place gained, in any reading or form, not even inside a longer word, nor
// does the place hold, inside a longer word, a word v gained, and the two
// did not drift together (sources). A copy moves with its source. A place
// that gained no word (one dropped, nothing new) shows nothing, and neither
// does one whose words v took: there it stays where the value may have come
// from (fail closed). Only finding more stops it: no word's shape is read.
func movedAway(ed, v *valueDrift) string {
	if ed.rec == ed.live {
		return "and it is the value recorded there"
	}
	gained := false
	for i := range ed.gained {
		for w := range ed.gained[i] {
			gained = true
			if strings.Contains(v.live, w) || v.decoded && strings.Contains(v.decLive, w) || inForms(v.forms, w) {
				return "and the value took a word of it"
			}
		}
	}
	if !gained {
		return "which holds no word the recorded one lacked"
	}
	for i := range v.gained {
		for w := range v.gained[i] {
			if inForms(ed.forms, w) {
				return "and it holds a word the value took"
			}
		}
	}
	if found, _ := sources(ed, v); found {
		return "and the value drifted with it"
	}
	return ""
}

// inForms reports whether w stands anywhere in the live side of forms (a
// value's declared forms, valueDrift.forms), inside a longer word too.
func inForms(forms [][2]string, w string) bool {
	for _, f := range forms {
		if strings.Contains(f[1], w) {
			return true
		}
	}
	return false
}

// liveAt is what record's full round gave the application, by its view of x
// (ServedLive), at the fields of x's payload that hold what in finds (text:
// x's recorded text as searched; keep: its stamps searched as any text, as
// the view's are then): fSame where every such field of the view, by its
// path, holds it too; fMoved where F gave another value at one or more of
// them, which v took none of the words of (movedAway), and the same at the
// rest; fFollowed where v took a word of one, or drifted with it (followed
// says which); fElsewhere where a field of the view holds it whose recorded
// field lacks it, or that the recording lacks (followed names the field);
// fNone where x has no view, where no one recorded field holds it (it
// stands across two), or where the view lacks a field holding it. Fields
// are read one by one, the view's as well as the recording's, so what F
// gave at another field than the recording did is never read as moved.
func (dc *DriftCache) liveAt(x Raw, text string, keep bool, v *valueDrift, in func(own string) bool) (view fView, followed string) {
	if !x.Live() {
		return fNone, ""
	}
	liveText := dc.searched(*x.live, keep)[0]
	at := func(fields []rawField, text string, f rawField) (string, bool) {
		for _, g := range fields {
			if g.path == f.path && g.key == f.key {
				if g.end <= len(text) {
					return text[g.start:g.end], true
				}
				return "", false
			}
		}
		return "", false
	}
	for _, g := range x.live.fields {
		if g.end > len(liveText) || !in(liveText[g.start:g.end]) {
			continue
		}
		if own, ok := at(x.fields, text, g); !ok || !in(own) {
			return fElsewhere, g.describe()
		}
	}
	view = fSame
	held := false
	for _, f := range x.fields {
		if f.end > len(text) || !in(text[f.start:f.end]) {
			continue
		}
		held = true
		lv, ok := at(x.live.fields, liveText, f)
		if !ok {
			return fNone, ""
		}
		if in(lv) {
			continue
		}
		if why := movedAway(dc.of(text[f.start:f.end], lv), v); why != "" {
			view, followed = fFollowed, why
		} else if view == fSame {
			view = fMoved
		}
	}
	if !held {
		return fNone, ""
	}
	return view, followed
}

// excusedSeen is excused of word t of the drifted value v, found in reading
// i at a leaf (path) of an exchange record's full round served before the
// call (e), whose recorded and served text ed reads: the recording gave the
// application the leaf's recorded text there where it recorded e's answer
// strictly before the call (recordedGave; the test case's own request:
// before every call of the test), and F gave it what it served there
// (Seen.Live): t where that holds it, else another value, which v took none
// of the words of or did (movedAway).
func (f *follows) excusedSeen(e Earlier, ed, v *valueDrift, i int, t string, at time.Time, why, path string) bool {
	if f == nil {
		return false
	}
	before, untimed := e.Given, ""
	if !e.Given {
		before, untimed = recordedGave(e.recordedAt(), e.Payload.answered, at, e.Mock)
	}
	fv, followed := fNone, ""
	switch {
	case ed.liveTok[i][t]:
		fv = fSame
	case ed.rec != ed.live:
		fv = fMoved
		if followed = movedAway(ed, v); followed != "" {
			fv = fFollowed
		}
	}
	if !f.excused(t, before && ed.recTok[i][t], fv, followed, untimed, why, v.wordOf(t, v.recTok[i][t])+" stands in "+e.Mock+" at "+path) {
		return false
	}
	f.source(e.Mock)
	return true
}

// excusedRaw is excused of word t of the drifted value v, found in reading i
// in x's payload (at place, as a reason names it): the recording gave the
// application the payload where it recorded x's answer strictly before the
// call (recordedGave), and F gave it where it served x unchanged
// (Raw.AsRecorded), or, of a payload F served changed and gave its view of
// (ServedLive), field by field as the view shows (liveAt): t, or another
// value v took none of the words of. A payload F served changed with no
// view, F may have given another word there, which keploy does not see. A
// payload's clock stamps are read blanked: F may have stamped its own
// there, so a word that stands only in one is no word both gave.
func (f *follows) excusedRaw(x Raw, v *valueDrift, i int, t string, at time.Time, why, place string) bool {
	if f == nil {
		return false
	}
	before, untimed := recordedGave(x.recordedAt(), x.answered, at, x.mock)
	held := f.dc.searchedTokens(x, false)[i][t]
	fv, followed := fNone, ""
	switch {
	case !held:
	case x.AsRecorded():
		fv = fSame
	case x.Live():
		fv, followed = f.dc.liveAt(x, f.dc.searched(x, false)[0], false, v, func(own string) bool { return f.dc.rawTokens(own)[i][t] })
	}
	if !f.excused(t, before && held, fv, followed, untimed, why, v.wordOf(t, v.recTok[i][t])+" stands in "+place) {
		return false
	}
	f.source(x.mock)
	return true
}

// excusedLive is excusedRaw the other way about, of word t the drifted
// value v holds as F saw it (in reading i), found in what F gave the
// application in x's payload, served changed (its view, ServedLive), and in
// no text the recording gave there (at place, as a reason names it): the
// recording gave the application x's payload where it recorded x's answer
// strictly before the call (recordedGave), and at every field of the view
// holding t (by path) it gave another value, which the recorded value took
// none of the words of (recordedThere, movedFrom); the recorded value lacks t.
// That is fMoved's evidence with the two runs' roles swapped: a copy moves
// with its source, so a value that took nothing of what the recording gave
// there, while F gave t there, is no copy of it; and the recorded value
// took no word of the payload anywhere (recordedTook): a value that copies
// a word of a payload by where it stands there follows it to another field
// where F gave its words elsewhere. Only an answer the
// application was given is read: a word of the request it sent there, which
// it made itself, shows nothing. A view that gives t at a field of the
// request, or at a field the recording lacks, a field the recorded value took a word of or
// drifted with, one that lost no word in F, a payload the recording gave at
// or after the call, or whose times keploy cannot read, shows nothing, and
// the word stays refused, naming why.
func (f *follows) excusedLive(x Raw, v *valueDrift, i int, t string, at time.Time, why, place string) bool {
	if f == nil {
		return false
	}
	before, untimed := recordedGave(x.recordedAt(), x.answered, at, x.mock)
	view, followed := f.dc.recordedThere(x, v, func(own string) bool { return f.dc.rawTokens(own)[i][t] })
	took := f.dc.recordedTook(x, v)
	switch {
	case untimed != "":
		f.refused = untimed + ", so the recording cannot show the value does not follow that word"
		return false
	case !before:
		f.refused = "the recording did not give the application that payload before the call, so it cannot show the value does not follow that word"
		return false
	case view == fNone:
		f.refused = "no one field of what record's full round gave there holds that word, so the recording cannot show the value does not follow it"
		return false
	case view == fElsewhere:
		f.refused = "record's full round gave the application that word at " + followed + ", which the recording lacks, so it cannot show the value does not follow it"
		return false
	case view == fSent:
		f.refused = "the word stands at " + followed + ", in what the application sent there and not in an answer it was given, so the recording cannot show the value does not follow it"
		return false
	case view == fFollowed:
		f.refused = "the recording gave the application another value there, " + followed + ", so it cannot show the value does not follow it"
		return false
	case took != "":
		f.refused = took + ", so the value may have followed that payload's words to wherever record's full round gave them, and the recording cannot show it does not follow that word"
		return false
	case f.dc.holdsWord(f.rec, t):
		f.refused = "the recorded value holds that word too, so it cannot show the value does not follow it"
		return false
	}
	if f.first == "" {
		f.first = why
	}
	if f.moved == "" {
		f.moved = v.wordOf(t, false) + " stands in " + place + ", where the recording gave the application another value, which the recorded value took none of the words of: a copy moves with its source, so the value is no copy of it"
	}
	f.source(x.mock)
	return true
}

// recordedThere is liveAt the other way about: what the recording gave the
// application at the fields of x's view (ServedLive) that hold what in
// finds, where the recording's text holds it nowhere (excusedLive). fSent
// where such a field is no field of its answer (the request the
// application sent, which it made itself: no word the recording gave it;
// or of a kind whose spec holds its request and answer in one field, which
// has no answer apart: answerApart), followed naming the field; fMoved
// where the recording gave another value at every such field (by path),
// which the recorded value took none of the words of (movedFrom);
// fFollowed where it took a word of one, or drifted with it, or one lost no
// word in F (followed says which); fElsewhere where a field holding it is
// one the recording lacks (followed names the field); fNone where x has no
// view or no one field of the view holds it (it stands across two). Texts
// are read as excusedRaw reads them: stamps and protocol-phase clock reads
// blanked.
func (dc *DriftCache) recordedThere(x Raw, v *valueDrift, in func(own string) bool) (view fView, followed string) {
	if !x.Live() {
		return fNone, ""
	}
	text := dc.searched(x, false)[0]
	liveText := dc.searched(*x.live, false)[0]
	view = fNone
	for _, g := range x.live.fields {
		if g.end > len(liveText) || !in(liveText[g.start:g.end]) {
			continue
		}
		if !answerPath(g.path) {
			return fSent, g.describe()
		}
		own, ok := fieldText(x.fields, text, g)
		if !ok {
			return fElsewhere, g.describe()
		}
		if why := movedFrom(dc.of(own, liveText[g.start:g.end]), v); why != "" {
			return fFollowed, why
		}
		view = fMoved
	}
	return view, ""
}

// recordedTook says where a word of what the drifted value v changed, as
// recorded (eachChanged), stands in x's payload as the recording gave it,
// stamps searched as any text, "" where none does: the recording gave the
// application that payload and the recorded value took a word of it (at
// whatever field). A value that copies a word of a payload by where it
// stands there (the id of a row F gave in another order) took a word of it
// in both runs, at another field in each, so a word of what F gave there
// the recording lacks shows nothing (excusedLive).
func (dc *DriftCache) recordedTook(x Raw, v *valueDrift) string {
	tokens := dc.searchedTokens(x, true)
	return v.eachChanged(func(i int, w string, recorded bool) string {
		if !recorded || i >= len(tokens) || !tokens[i][w] {
			return ""
		}
		if f := dc.fieldOf(x, dc.searched(x, true), w); f != "" {
			return v.wordOf(w, true) + " stands in " + x.mock + " at " + f + ", as the recording gave it"
		}
		return v.wordOf(w, true) + " stands in " + x.mock + "'s payload, as the recording gave it"
	})
}

// fieldText is the text of the field of fields (in text) at f's path,
// false where there is none or it runs past text.
func fieldText(fields []rawField, text string, f rawField) (string, bool) {
	for _, g := range fields {
		if g.path == f.path && g.key == f.key {
			if g.end <= len(text) {
				return text[g.start:g.end], true
			}
			return "", false
		}
	}
	return "", false
}

// movedFrom is movedAway the other way about: why a place the recording
// gave the application another value at than F gave (ed: the recorded value
// there and the one F gave, which holds a word of the drifted value v as F
// saw it) does not show v no copy of it, "" where it does: the place lost a
// word in F (a value the recording could have copied), v as recorded holds
// none of the words the place lost, in any reading or form, not even inside
// a longer word, nor does the place as recorded hold, inside a longer word,
// a word v lost, and the two did not drift together (sources). A place that
// lost no word shows nothing, and neither does one whose words the
// recorded value took: there it stays where the value may have come from
// (fail closed). Only finding more stops it: no word's shape is read.
func movedFrom(ed, v *valueDrift) string {
	lost := false
	for i := range ed.lost {
		for w := range ed.lost[i] {
			lost = true
			if strings.Contains(v.rec, w) || v.decoded && strings.Contains(v.decRec, w) || gluedIn(v, w, true) {
				return "and the recorded value took a word of it"
			}
		}
	}
	if !lost {
		return "which holds no word record's full round's lacks"
	}
	for i := range v.lost {
		for w := range v.lost[i] {
			if gluedIn(ed, w, true) {
				return "and it holds a word the recorded value lost"
			}
		}
	}
	if found, _ := sources(ed, v); found {
		return "and the value drifted with it"
	}
	return ""
}

// excusedGiven is excused of word t of the drifted value v, found in reading
// i in the answer of a recording F gave the application before the call
// that no earlier place of the call's own test is (g: another recorded
// test's, or one F gave earlier in the running set), whose reason there is
// why (givenWhy): the recording gave the application g's answer strictly
// before the call (recordedGave), and F gave it t at the fields the
// recording gave it at (givenWords.same: g served unchanged, or its view
// showing t there). Both runs then gave the application that word there, and
// one of the two values lacks it (excused): the value was made beside the
// word without taking it, and an application that copied it would have
// copied it in both runs. That is what tells a word a value holds by chance
// (a group of a random id that another test's random id holds too) from a
// copy, never the word's shape. A recording F served changed with no view, a
// view that gives the word at another field or not at all, one the recording
// answered at or after the call, or whose times keploy cannot read, shows
// nothing, and the word stays refused, naming why.
func (f *follows) excusedGiven(g givenWords, v *valueDrift, i int, t string, at time.Time, why string) bool {
	if f == nil {
		return false
	}
	gat := g.at
	if g.noTime {
		gat = time.Time{}
	}
	before, untimed := recordedGave(gat, g.answered, at, g.mock)
	fv := fNone
	if g.sameHolds(i, t) {
		fv = fSame
	}
	if !f.excused(t, before, fv, "", untimed, why, v.wordOf(t, v.recTok[i][t])+" stands in "+g.mock) {
		return false
	}
	f.source(g.mock)
	if g.set {
		for _, x := range f.given {
			if x.mock == g.mock && x.at.Equal(g.at) {
				return true
			}
		}
		f.given = append(f.given, Raw{mock: g.mock, at: g.at, noTime: g.noTime, answered: g.answered, conn: g.conn, pos: g.pos, served: true, read: true, first: g.first, last: g.last})
	}
	return true
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
// noise once the word rule let go the words fol found (fol.first non-empty),
// or once the search for its source went past a place (fol.refuted):
// noise where the recording and F gave the application the value's
// candidate sources (fol.sources) before the call in the same order;
// refused otherwise, saying why. earlier and raw are every place the
// classifier was handed (the call's own and those F gave elsewhere in the
// set); only the sources among them are ordered.
func (fol *follows) followed(ln Learned, at time.Time, earlier []Earlier, raw []Raw) Learned {
	source := func(mock string) bool { return fol.sources[mock] }
	raw = append(raw[:len(raw):len(raw)], fol.given...)
	rec, untoldRec := recordedOrder(at, earlier, raw, source)
	f, untoldF := servedOrder(earlier, raw, source)
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
		lead := fol.first
		// refute sets a source aside only where F served it as recorded, the
		// recording gave it before the call and F's value is not in it, and
		// the word rule then reads the same word at the same place on the
		// same terms (excused), so it lets the word go there and sets first
		// before a value is learned as noise. A leaf refuteSeen sets aside
		// leaves first unset where the value's words there are a clock
		// read's, which the word rule never reads (aside). Either way the
		// refusal names the source set aside (fail closed), and the echo's
		// refusal in classifyRead does the same.
		if lead == "" {
			lead = fol.refuted
		}
		if lead == "" {
			lead = fol.aside
		}
		ln.Kind, ln.Why, ln.Echo = KindUnaccounted, lead+"; "+why+", so keploy cannot tell a copy from a varying value", nil
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
// case's request first; then every candidate source (source: a place
// holding a word the value could have copied, follows.sources) whose answer
// was recorded strictly before the call (recordedGave), served by F or not.
// Any other place is read for nothing: neither its times nor its order. The recording shows the
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
func recordedOrder(at time.Time, earlier []Earlier, raw []Raw, source func(mock string) bool) (order []string, untold string) {
	var places []recPlace
	add := func(p recPlace) string {
		if !source(p.mock) {
			return ""
		}
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
		if why := add(recPlace{e.Mock, e.recordedAt(), e.Payload.answered, e.Payload.conn, e.Payload.pos}); why != "" {
			return nil, why
		}
	}
	for _, x := range raw {
		if why := add(recPlace{x.mock, x.recordedAt(), x.answered, x.conn, x.pos}); why != "" {
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
// the candidate sources (source) it served (every Earlier, and each Raw it
// served, whenever recorded), in the one order it served the set's mocks in
// (Raw.ServedIn, the first and the last of each mock's serves; an
// exchange's by its payload's, InServeOrder). A recording it did not serve
// it did not give. untold, where that order cannot be told, says why: a
// served source the order does not number, beside another source it served
// (two serves it gave in an order keploy cannot establish are unordered,
// never ordered by when keploy noted them), or a source served more than
// once with a serve between its first and its last that is no place's (it
// may have been that mock's own, unnumbered). Any other place's serves only
// number the set's.
func servedOrder(earlier []Earlier, raw []Raw, source func(mock string) bool) (order []string, untold string) {
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
	var others []int
	unordered := ""
	placed := 0
	add := func(mock string, first, last int) {
		if !source(mock) {
			// In no order the rule reads, its serves still number the
			// set's: a serve between a source's first and last is placed.
			others = append(others, first, last)
			return
		}
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
		nums = append(nums, v.n)
	}
	for _, n := range others {
		if n != 0 {
			nums = append(nums, n)
		}
	}
	sort.Ints(nums)
	nums = slices.Compact(nums)
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
