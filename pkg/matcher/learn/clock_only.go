package learn

import (
	"strings"

	"go.keploy.io/server/v3/pkg/models"
)

// ClockOnly reports whether a call that missed differs from the recording
// closest to it only in reads of the application's clock, by the parser's
// field diff of the two (models.UnmatchedCall.Clock): there is one diff at
// least; the list is whole (cut: the parser kept fewer diffs than it found,
// so what else differed is not known); and every diff is a value that
// changed (models.DiffKindValueChanged), whose recorded and live text are
// each whole (not cut at the diff's value bound, which ends a cut text with
// "…") and differ only in instants (clockDrift). Such a call is the
// recording with the application's clock moved: a timer's "older than now"
// filter, a delayed write's stamp. It reads no move by FullRoundLead and no
// call's window, as ClockOf does: the clock can have moved by any amount
// between the recording and this call (a timer fires at another moment of
// the test), so only the values' own text says it is the clock.
func ClockOnly(diffs []models.MockFieldDiff, cut bool) bool {
	if len(diffs) == 0 || cut {
		return false
	}
	for _, d := range diffs {
		if d.Kind != models.DiffKindValueChanged || cutText(d.Expected) || cutText(d.Actual) {
			return false
		}
		if !clockDrift(d.Expected, d.Actual, typedDate(d.Path)) {
			return false
		}
	}
	return true
}

// cutText reports whether a field diff's value may have been cut at its
// bound (matcher.JSONFieldDiffs ends a cut value with "…"): the rest of it,
// which may differ, is not known.
func cutText(s string) bool { return strings.HasSuffix(s, "…") }

// typedDate reports whether a field diff's path is a value typed as a date:
// extended JSON's $date (a Mongo call's canonical form writes BSON's datetime
// {"$date":{"$numberLong":"<ms>"}}), whose number is an instant by its type.
func typedDate(path string) bool {
	return strings.HasSuffix(path, ".$date.$numberLong") || strings.HasSuffix(path, ".$date") ||
		path == "$date" || path == "$date.$numberLong"
}

// clockDrift reports whether recorded and live, a field's text in a
// recording and in a call made now, differ only in instants, by however much
// the clock moved between them: every word that changed is a part of an
// instant in each, found in clockLayouts as clockAt finds one (the k-th of a
// layout in one read against the k-th in the other, each in range), and one
// changed. Only a layout whose text names a day counts (a date and time, a
// UUID's time): a time of day names none, and a bare number (an epoch) is an
// instant only where the field is typed a date (date), since a number that
// only looks like one ("2025551234", a phone number) is no read of the clock.
func clockDrift(recorded, live string, date bool) bool {
	rec, lv := map[string]bool{}, map[string]bool{}
	takenR, takenL := make([]bool, len(recorded)), make([]bool, len(live))
	for _, lay := range clockLayouts {
		if lay.wraps || lay.epoch && !date {
			continue
		}
		rm, lm := clockMatches(lay, recorded, takenR), clockMatches(lay, live, takenL)
		for j := 0; j < len(rm) && j < len(lm); j++ {
			rs, ls := recorded[rm[j][0]:rm[j][1]], live[lm[j][0]:lm[j][1]]
			if rs == ls {
				continue
			}
			for i := rm[j][0]; i < rm[j][1]; i++ {
				takenR[i] = true
			}
			for i := lm[j][0]; i < lm[j][1]; i++ {
				takenL[i] = true
			}
			if _, _, ok := lay.read(rs); !ok {
				continue
			}
			if _, _, ok := lay.read(ls); !ok {
				continue
			}
			for i, w := range wordsBy(rs, isWord) {
				if lay.timeWord == nil || lay.timeWord(rs, i) {
					rec[w] = true
				}
			}
			for i, w := range wordsBy(ls, isWord) {
				if lay.timeWord == nil || lay.timeWord(ls, i) {
					lv[w] = true
				}
			}
		}
	}
	r, l := tokensBy(recorded, isWord), tokensBy(live, isWord)
	changed := false
	for w := range r {
		if !l[w] {
			if !rec[w] {
				return false
			}
			changed = true
		}
	}
	for w := range l {
		if !r[w] {
			if !lv[w] {
				return false
			}
			changed = true
		}
	}
	return changed
}
