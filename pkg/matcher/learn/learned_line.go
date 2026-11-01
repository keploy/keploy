package learn

import (
	"cmp"
	"slices"
	"strconv"
	"strings"

	"go.keploy.io/server/v3/pkg/secretname"
)

// A learned word's line says what keploy learned and where (test, mock,
// field, word), both values, the rule that gave it, at most three places
// of its evidence, and how to undo it:
//
//	<test> <mock> <field>[ word <n>]: recorded "<r>", round F "<f>": learned <what>: <rule>[, learned from round F's call in <window>, for learning only]. Places: <p>, <p>. Undo: <edit>.
//
// The undo is the edit that stops it: noise at the word (or the field) in
// keploy.yml, which keploy then never compares, or a re-record.

// learnedWord is what one learned word's line says.
type learnedWord struct {
	Test, Mock, Field string
	Word              int
	Recorded, Full    string
	Outcome           WordOutcome
	// Rule is the plain rule that gave it: for a Value its bind place, for
	// a Clock its offset and window.
	Rule string
	// Places are the evidence places, "mock-N <path>" each; three print.
	Places []string
	// From is, in the timer rule (ModeElsewhere), where round F's call
	// landed (Landed.String): "<TestB>'s window" or "between tests"; ""
	// otherwise.
	From string
}

// outcomeSays is what a learned line says of each outcome.
var outcomeSays = map[WordOutcome]string{
	OutcomeClock: "a clock read, checked in its call's span",
	OutcomeValue: "a value bound at its source, checked at every later use",
	OutcomeFresh: "a value made anew each run, bound at its first call and checked at every later use",
}

// learnedLine is l's one line (see above).
func learnedLine(l learnedWord) string {
	var b strings.Builder
	b.WriteString(wordWhere(l.Test, l.Mock, l.Field, l.Word))
	rec, full, rule := l.Recorded, l.Full, l.Rule
	if secretAt(l.Field, l.Places) {
		rec, full, rule = masked, masked, hidden(rule)
	}
	b.WriteString(recordedMark + strconv.Quote(rec) + roundFMark + strconv.Quote(full) + ": learned ")
	b.WriteString(cmp.Or(outcomeSays[l.Outcome], "nothing"))
	if rule != "" {
		b.WriteString(": " + rule)
	}
	if l.From != "" {
		b.WriteString(", learned from round F's call in " + l.From + ", for learning only")
	}
	b.WriteString(placesPart(l.Places))
	b.WriteString(". Undo: " + editWords(noiseEdit(l.Mock, l.Field, l.Word)) + editSep + editWords(Edit{Kind: EditReRecord}) + ".")
	return b.String()
}

// A secret's values never print. Where a line's field, or a place it names,
// names a secret by keploy's one rule for a name (secretname.Name, the rule
// a container's fingerprint uses), read at each segment of the path
// (body.user.password, header.Authorization; never createdAt), both values
// print as "***", and so does every value its rule or reason quotes: those
// quote every value they name (strconv.Quote), and nothing else. The
// Decision keeps both values, which the adapter binds. The rule is broad
// on purpose: a field named author or passenger prints "***" too.
const masked = "***"

// withoutMark begins a line's words for round F's text at a place that does
// not hold its value, where keploy does not print that text (gaveAt).
const withoutMark = "a value without "

// secretAt reports whether field, or one of places, names a secret.
func secretAt(field string, places []string) bool {
	return slices.ContainsFunc(append([]string{field}, places...), func(s string) bool {
		return slices.ContainsFunc(strings.FieldsFunc(s, func(r rune) bool { return r == '.' || r == ' ' }), secretname.Name)
	})
}

// hidden is s with each quoted value in it printed as "***"; from a quote
// that does not close on, all of it is (fail closed).
func hidden(s string) string {
	var b strings.Builder
	for {
		i := strings.IndexByte(s, '"')
		if i < 0 {
			return b.String() + s
		}
		b.WriteString(s[:i] + strconv.Quote(masked))
		q, err := strconv.QuotedPrefix(s[i:])
		if err != nil {
			return b.String()
		}
		s = s[i+len(q):]
	}
}

// hideSecrets prints rf's values as "***" where they are a secret's.
func hideSecrets(rf *Refusal) {
	if secretAt(rf.Field, rf.Places) {
		rf.Recorded, rf.Full, rf.Why = masked, masked, hidden(rf.Why)
	}
}
