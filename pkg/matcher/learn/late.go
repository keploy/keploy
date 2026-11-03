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

// lateServes are the recordings F gave the application in an older test of
// the set (givenWords.test) that hold live, the value a call carries now,
// whole, as F gave it there (gaveWhole), in the order the word rule reads
// them (offsetPlaces.words); none where the places could not be read
// (offsetPlaces.unread).
func lateServes(live string, op *offsetPlaces) []givenWords {
	if op == nil || op.unread != "" {
		return nil
	}
	var out []givenWords
	for _, g := range op.words {
		if gave, known := g.gaveWhole(live); g.test > 0 && known && gave {
			out = append(out, g)
		}
	}
	return out
}

// gaveWhole reports whether F gave the application live whole, as one word
// of a reading it is all of, in g's answer (gave), and whether keploy can
// tell what F gave there (known): its recording's answer where F served it
// as recorded, the view F gave of it where it stated one; not where no
// adapter said, a view could not be read, or its serves gave views that
// differ.
func (g givenWords) gaveWhole(live string) (gave, known bool) {
	if g.unstated || g.unread || len(g.views) > 0 {
		return false, false
	}
	lists := g.answerLists
	if g.view.words != nil || g.view.stamped != nil {
		lists = g.view
	}
	for i, in := range readings {
		if allBytes(live, in) && lists.holds(i, live, false) {
			return true, true
		}
	}
	return false, true
}

// allBytes reports whether s is not empty and every byte of it is one in
// reads as a word's.
func allBytes(s string, in func(byte) bool) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if !in(s[i]) {
			return false
		}
	}
	return true
}

// notFromWhy is the reason of a drifted value v whose recorded value the
// search found at from (appearedIn), a recording's payload of the call's own
// test, where nothing F gave the application there holds the value the call
// carries now: F served from as the rule a copy follows its source sets
// aside (follows.refute: as recorded, byte for byte, or by a view the value
// did not move with), or had not served it before the call
// (Place.unserved). It names the serves of older tests that gave the
// application that value whole (lateServes, lateOf). Where none did and F
// served from, it says plainly that keploy cannot name where the value came
// from, never that it flowed from from. "" where F's view of from may hold
// the value, or keploy cannot tell what F gave there, or every word of the
// value that changed is a read of the clock (clockSet.whole: the clock's
// rules read it, and an answer's instant is followed whole whatever F gave
// there), which leaves the search's reason (evidence); and where F had not
// served from and no older test's serve gave the value, whose reason
// (correction) already says F never served it.
func (dc *DriftCache) notFromWhy(v *valueDrift, from Place, at time.Time, served []Earlier, raw []Raw, places func() *offsetPlaces) string {
	if v.clock.whole {
		return ""
	}
	aside := ""
	if !from.unserved {
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
			return ""
		}
	}
	if why := lateOf(v, from, lateServes(v.live, places())); why != "" || aside == "" {
		return why
	}
	return aside + "; " + notClock(v) + ", and keploy cannot name where the value the call carries now came from, so it cannot follow it. If the field is a time the application reads from its own clock, declare it noise in keploy.yml, and keploy stops comparing it"
}

// lateOf is the reason of a call that carries what F gave the application
// in an older test (late: lateServes), where its recorded value stands in
// from, a recording of the call's own test (appearedIn), at which F did not
// give it that value (notFromWhy): the call is late, and nothing of its own
// test answers it; "" where no older test's serve gave it.
func lateOf(v *valueDrift, from Place, late []givenWords) string {
	if len(late) == 0 {
		return ""
	}
	at := placeAt(from)
	found := "the value it was recorded with stands whole in " + at
	if from.Held != v.rec && from.Held != v.decRec {
		found = "a part of the value it was recorded with (" + partOf(v.rec, from.Held) + ") stands in " + at
	}
	if len(late) > 1 {
		names := make([]string, 0, 3)
		for _, g := range late[:min(3, len(late))] {
			names = append(names, g.mock+" (the set's test "+strconv.Itoa(g.test)+")")
		}
		more := ""
		if len(late) > 3 {
			more = " and " + strconv.Itoa(len(late)-3) + " more"
		}
		return "the value it carries now is, whole, what record's full round gave the application in each of " + strings.Join(names, ", ") + more + ", older tests than the call's, so keploy cannot tell which of them it carries; " + found + ", a recording of the call's own test, where record's full round did not give the application that value. The call is late: the application still works on what an older test gave it, and no recording of the call's own test answers it, since keploy follows no value of another test's. Look at why the application fell behind (a call of that test keploy did not answer, which the application retried)"
	}
	g := late[0]
	return "the value it carries now is, whole, what record's full round gave the application in " + g.mock + ", in the set's test " + strconv.Itoa(g.test) + ", an older test than the call's; " + found + ", a recording of the call's own test, where record's full round did not give the application that value. The call is a late call of test " + strconv.Itoa(g.test) + ": the application still works on what " + g.mock + " gave it there, and no recording of the call's own test answers it, since keploy follows no value of another test's. Look at why the application fell behind (a call of test " + strconv.Itoa(g.test) + " keploy did not answer, which the application retried)"
}
