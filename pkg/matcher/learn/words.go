package learn

import "go.keploy.io/server/v3/pkg/models"

// Word-level learn: of a drifted leaf, record's full round learns only the
// words that changed (models.WordsEntry), and a later replay keeps checking
// the rest.
//
// A value is read as segments, split at each run of structural characters
// (whitespace and " ' ` { } [ ] ( ) : , ; & < > | \ = / ? # +: a closed
// lexical class, the delimiters of the grammars keploy declares, applied byte
// by byte), and each segment as runs, split at every other byte that is
// neither a letter nor a digit and wherever the character class changes. An
// accepted clock read collapses first, on both sides, into one segment and
// one run. No value's shape is read: what may change is what F saw change.
//
// This build declares the word-level contract only: nothing learns words yet,
// and each function below refuses.

// notJudged is why WholeDrift refuses while this build judges no leaf whole.
const notJudged = "keploy does not judge yet whether a leaf drifted whole"

// notRead is why WordsAdmit refuses while this build reads no learned words.
const notRead = "keploy does not read learned words yet, so the leaf is compared whole"

// WordsAdmit reports whether live may stand where rec was recorded, given the
// words e learned of the leaf: both values are split again, every byte outside
// e's slots must be equal, and each slot is checked as its kind says
// (models.WordSlot). An entry that does not hold is void and admits nothing:
// its digest or layout no longer matches rec, a guard is missing or
// malformed, a slot's seg or run is out of range, a kind or a layout is one
// this build does not know, or an instant's skeleton is not rec's match at
// its segment. why says, for a person to read, what did not hold; "" when ok.
//
// This build reads no words: it admits nothing.
func WordsAdmit(rec, live string, e models.WordsEntry) (ok bool, why string) {
	return false, notRead
}

// WholeDrift reports whether a drifted leaf (rec, recorded; live, record's
// full round's) changed whole: after its accepted clock reads collapse (read
// by the full round's lead, or within w, the call's window; nil for none),
// every run of rec changed and no changed run repeats. It is judged by runs,
// never by a value's shape. A protocol learner that learns a leaf whole (as
// noise, or an echo) asks it in each reading it compares, and learns the
// leaf only when it is whole in all of them. why says, for a person to read,
// why it is not whole; "" when it is.
//
// This build judges no leaf whole: it refuses each.
func WholeDrift(rec, live string, w *Window) (whole bool, why string) {
	return false, notJudged
}
