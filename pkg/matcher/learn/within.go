package learn

import (
	"strconv"
	"strings"
)

// Within is how a leaf holds a variable's value as one part of a longer
// value: the constant text before and after it ("APP:" before an app id,
// "/settings" after a user id). The leaf is Before, the value, After. A
// replay compares a use place's text, and serves an answer place's, as
// recorded around the value the variable has in this run: only the
// variable's words move, every other word is still compared (word-level
// learning). The zero Within is none: the leaf holds the value whole, or at
// no part keploy can bind (Place.NotWithin says why).
//
// Only a value standing once in its leaf, whole, between characters that
// are not ASCII letters or digits or at an end, is such a part
// (PartWithin): a value standing twice, or as a part of a word, is not, and
// stays refused with that reason.
type Within struct {
	Before, After string
}

// Set reports whether w names a part: some constant text stands beside the
// value.
func (w Within) Set() bool { return w.Before != "" || w.After != "" }

// Render is the leaf w makes of value.
func (w Within) Render(value string) string { return w.Before + value + w.After }

// ValueIn is the value leaf holds at w's part, and whether leaf is one w
// makes: Before, then a value standing as PartWithin reads it, then After.
// A leaf whose constant text differs from the recorded one, or whose value
// is empty or not such a part, is none.
func (w Within) ValueIn(leaf string) (string, bool) {
	if !w.Set() || len(leaf) <= len(w.Before)+len(w.After) || !strings.HasPrefix(leaf, w.Before) || !strings.HasSuffix(leaf, w.After) {
		return "", false
	}
	v := leaf[len(w.Before) : len(leaf)-len(w.After)]
	if got, why := PartWithin(leaf, v); why != "" || got != w {
		return "", false
	}
	return v, true
}

// PartWithin is how longer holds value as one bindable part (Within), or,
// as why, why it holds it at none: "" with a set Within when it does. why
// names where the value stands by its characters' places, never by its
// text, and is "" with the zero Within too where longer does not hold value
// at all, or holds it whole.
func PartWithin(longer, value string) (Within, string) {
	if value == "" || longer == value {
		return Within{}, ""
	}
	n := occurrences(longer, value)
	switch {
	case n == 0:
		return Within{}, ""
	case n > 1:
		return Within{}, "the value stands in it at " + strconv.Itoa(n) + " places, and keploy binds a part of a longer value only where the value stands in it once"
	}
	i := strings.Index(longer, value)
	j := i + len(value)
	if i > 0 && isWord(longer[i-1]) || j < len(longer) && isWord(longer[j]) {
		return Within{}, "the value stands in it as a part of a word (" + partOf(longer, value) + ", beside a letter or digit), and keploy binds a part of a longer value only where the value stands whole between other characters or at an end"
	}
	return Within{Before: longer[:i], After: longer[j:]}, ""
}

// occurrences is how many places of s v begins at, overlapping ones
// counted: "aa" begins at two places of "aaa".
func occurrences(s, v string) int {
	n := 0
	for i := 0; i <= len(s)-len(v); {
		j := strings.Index(s[i:], v)
		if j < 0 {
			break
		}
		n++
		i += j + 1
	}
	return n
}

// partFrom is how a drifted leaf (recorded rec, sent live) holds the value
// an earlier leaf held whole (recorded fromRec, sent fromLive) as one
// bindable part: in both runs, with the same text beside it, so that only
// the value moved. why says why it holds it at none, "" where it holds it
// at one or does not hold the earlier value at all (a value found the
// other way round, the drifted leaf's inside the earlier one, or by its
// words alone).
func partFrom(rec, live, fromRec, fromLive string) (Within, string) {
	if occurrences(rec, fromRec) == 0 {
		return Within{}, ""
	}
	w, why := PartWithin(rec, fromRec)
	if why != "" {
		return Within{}, "as recorded, " + why
	}
	if occurrences(live, fromLive) == 0 {
		return Within{}, "the value it holds as recorded is not in what it held in record's full round, so the text beside it changed too, and keploy binds a part of a longer value only where the rest of the value stays as recorded"
	}
	l, why := PartWithin(live, fromLive)
	if why != "" {
		return Within{}, "in record's full round, " + why
	}
	if l != w {
		return Within{}, "the text beside the value changed in record's full round too, and keploy binds a part of a longer value only where the rest of the value stays as recorded"
	}
	return w, ""
}

// Carried is the variable value a request leaf learned as a variable
// (KindVariable) carries: the value it holds as recorded and as sent live,
// the leaf's whole values where its earlier place's value stands whole in it
// (From.Within is the zero Within), else the values read out of each at
// that part. ok is false where a leaf is not the part's text around a value.
func (l Learned) Carried() (rec, live string, ok bool) {
	w := l.From.Within
	if !w.Set() {
		return l.Recorded.Value, l.Live.Value, true
	}
	rec, okRec := w.ValueIn(l.Recorded.Value)
	live, okLive := w.ValueIn(l.Live.Value)
	return rec, live, okRec && okLive
}

// AnswerPart is how an answer leaf a holds the recorded value of r (a
// variable's recorded value, r's type): whole (ok, the zero Within), or as
// one part of a longer string, by PartWithin's bound (ok, a set Within). ok
// is false where a holds it at neither: not at all, at two places, or as a
// part of a word, where an answer keeps its recorded text.
func AnswerPart(r, a Leaf) (Within, bool) {
	if WholeEcho(r, a) {
		return Within{}, true
	}
	if r.Type != String || a.Type != String || a.Unnamed {
		return Within{}, false
	}
	w, why := PartWithin(a.Value, r.Value)
	return w, why == "" && w.Set()
}
