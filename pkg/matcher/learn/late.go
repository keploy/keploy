package learn

import (
	"strconv"
	"strings"
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

// lateOf is the reason of a call that carries what F gave the application
// in an older test (late: lateServes), where its recorded value stands in
// from, a recording of the call's own test (appearedIn), and F did not give
// it the value it carries now there (fromGave false: as recorded, by its
// view, or not served before the call): the call is late, and nothing of
// its own test answers it. "" where keploy cannot tell what F gave at from,
// which leaves the reason the search gives (evidence).
func (dc *DriftCache) lateOf(v *valueDrift, from Place, raw []Raw, late []givenWords) string {
	if len(late) == 0 {
		return ""
	}
	for _, x := range raw {
		if x.mock != from.Mock || !x.served {
			continue
		}
		if gave, known := dc.wordsOf(x).gaveWhole(v.live); !known || gave {
			return ""
		}
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
