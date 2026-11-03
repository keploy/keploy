package learn

import (
	"strconv"
	"strings"
	"time"
)

// A late call is one the application makes in a later test's window while
// it still works on what an older test gave it (a consumer that retried a
// message F did not answer, and fell behind): the value it carries now is
// what F gave the application in that older test, and the value the
// call's recording holds stands in a recording of the window's own test. The
// search for the recorded value's source (appearedIn) finds that recording,
// but the value the call carries never came from it: a reason that named it
// as the value's source sent the user to the wrong recording.

// lateServes are the recordings F gave the application in one older test of
// the set (givenWords.test, Raw.test) that hold live, the value a call
// carries now, whole, as F gave it there (gaveWhole): those among raw, the
// recordings handed to the call (the test F ran before the call's is read
// as an earlier place of the call's own test: Tracker.Before), then those F
// gave earlier in the set (offsetPlaces.words), in the order the word rule
// reads them. None where live is a value an answer may hold by chance
// (lateID), or a recording F gave earlier in the set could not be read
// (offsetPlaces.unread), as it may hold the value too (one among raw that
// could not be read is refused before, with its own reason).
func (dc *DriftCache) lateServes(live string, raw []Raw, op *offsetPlaces) []givenWords {
	if !lateID(live) || op.unread != "" {
		return nil
	}
	var out []givenWords
	for _, x := range raw {
		if x.test == 0 {
			continue
		}
		g := dc.wordsOf(x)
		g.test, g.prev = x.test, x.prev
		if g.gaveWhole(live) {
			out = append(out, g)
		}
	}
	for _, g := range op.words {
		if g.test > 0 && g.gaveWhole(live) {
			out = append(out, g)
		}
	}
	return out
}

// lateID reports whether a late call may be named by live, the value it
// carries now: an id, a run of id bytes alone (idByte) of at least
// minEncodedRun, that holds a letter and a digit (a UUID, a nanoid,
// "ord-7K29Q4AB"), which no answer holds by chance. A count, a flag, a
// status or a code ("4", "true", "pending", "201") stands in most answers,
// so an older test's serve that holds one shows nothing of where the call's
// value came from, and keploy says it cannot name it.
func lateID(live string) bool {
	if len(live) < minEncodedRun {
		return false
	}
	letter, digit := false, false
	for i := 0; i < len(live); i++ {
		c := live[i]
		switch {
		case !idByte(c):
			return false
		case c >= '0' && c <= '9':
			digit = true
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z':
			letter = true
		}
	}
	return letter && digit
}

// gaveWhole reports whether keploy can tell that F gave the application
// live whole, as one word in a reading of g's answer: its recording's answer
// where F served it as recorded, the view F gave of it where it stated one;
// never where no adapter said, a view could not be read, or its serves gave
// views that differ. A word of a reading is a run of its bytes alone, so one
// equal to live is all of it.
func (g givenWords) gaveWhole(live string) bool {
	if g.unstated || g.unread || len(g.views) > 0 {
		return false
	}
	lists := g.answerLists
	if g.view.words != nil {
		lists = g.view
	}
	for i := range readings {
		if lists.holds(i, live, false) {
			return true
		}
	}
	return false
}

// notFromWhy is the reason of a drifted value v whose recorded value the
// search found at from (appearedIn), a recording's payload of the call's own
// test, where F served from as the rule a copy follows its source sets
// aside (follows.refute: as recorded, byte for byte, or by a view the value
// did not move with): nothing F gave the application there holds the value
// the call carries now. It names the serves of older tests that gave the
// application that value whole (lateServes, lateOf), and late is true: the
// leaf is no variable of from then. Where none did, it says plainly that
// keploy cannot name where the value came from, never that it flowed from
// from. "" where F's view of from may hold the value, keploy cannot tell what
// F gave there, or F had not served it before the call (Place.unserved,
// whose reason says so: correction), and where every word of the value that
// changed is a read of the clock (clockSet.whole: the clock's rules read it,
// and an answer's instant is followed whole whatever F gave there), which
// leaves the search's reason (evidence). Only past those does it read the
// recordings F gave the application earlier in the set (places).
func (dc *DriftCache) notFromWhy(v *valueDrift, from Place, at time.Time, served []Earlier, raw []Raw, places func() *offsetPlaces) (why string, late bool) {
	if v.clock.whole {
		return "", false
	}
	aside := ""
	for _, x := range raw {
		if x.mock != from.Mock {
			continue
		}
		keep := dc.stampsEchoed(v, served, raw)
		if f := (&follows{dc: dc, rec: v.rec, live: v.live, at: at}); f.refute(x, dc.searched(x, keep), keep, v, from) {
			aside = f.refuted
		}
		break
	}
	if aside == "" {
		return "", false
	}
	if why := lateOf(v, from, dc.lateServes(v.live, raw, places())); why != "" {
		return why, true
	}
	return aside + "; " + notClock(v) + ", and keploy cannot name where the value the call carries now came from, so it cannot follow it. If the field is a time the application reads from its own clock, declare it noise in keploy.yml, and keploy stops comparing it", false
}

// lateOf is the reason of a call that carries what F gave the application
// in an older test (late: lateServes), where its recorded value stands in
// from, a recording of the call's own test (appearedIn), at which F did not
// give it that value (notFromWhy): the call is late, and nothing of its own
// test answers it; "" where no older test's serve gave it. It names the
// serves by their recordings, never by the order F ran its tests in, which
// a selection or a deferred test makes no test case's number; only a serve
// of the test F ran just before the call's says so (givenWords.prev), whose
// values keploy follows where the call was recorded with them.
func lateOf(v *valueDrift, from Place, late []givenWords) string {
	if len(late) == 0 {
		return ""
	}
	at := placeAt(from)
	found := "the value it was recorded with stands whole in " + at
	if from.Held != v.rec && from.Held != v.decRec {
		found = "a part of the value it was recorded with (" + partOf(v.rec, from.Held) + ") stands in " + at
	}
	const tail = ", a recording of the call's own test, where record's full round did not give the application that value. "
	// A value of the test F ran before the call's is one keploy follows
	// where the call was recorded with it: this call was not.
	notFollowed := "no recording of the call's own test answers it, since keploy does not follow a value another test's recording gave the application. "
	for _, g := range late {
		if g.prev {
			notFollowed = "no recording of the call's own test answers it, and the call was recorded with another value than the application got there, so keploy cannot follow it from there either. "
		}
	}
	notFollowed += "Look at why the application fell behind (a call of that test keploy did not answer, which the application retried)"
	if len(late) > 1 {
		names := make([]string, 0, 3)
		for _, g := range late[:min(3, len(late))] {
			names = append(names, g.mock)
		}
		more := ""
		if len(late) > 3 {
			more = " and " + strconv.Itoa(len(late)-3) + " more"
		}
		return "the value it carries now is, whole, what record's full round gave the application in each of " + strings.Join(names, ", ") + more + ", each in an older test than the call's, so keploy cannot tell which of them it carries; " + found + tail + "The call is late: the application still works on what an older test gave it, and " + notFollowed
	}
	g := late[0]
	test := "an older test than the call's"
	if g.prev {
		test = "the test record's full round ran just before the call's"
	}
	return "the value it carries now is, whole, what record's full round gave the application in " + g.mock + ", in " + test + "; " + found + tail + "The call is a late call of " + g.mock + "'s test: the application still works on what " + g.mock + " gave it there, and " + notFollowed
}
