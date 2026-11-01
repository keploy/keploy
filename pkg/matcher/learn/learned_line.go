package learn

import (
	"strconv"
	"strings"
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
	// landed: "<TestB>'s window" or "between tests"; "" otherwise.
	From string
}

// learnedLine is l's one line (see above).
func learnedLine(l learnedWord) string {
	var b strings.Builder
	b.WriteString(wordWhere(l.Test, l.Mock, l.Field, l.Word))
	b.WriteString(recordedMark + strconv.Quote(l.Recorded) + roundFMark + strconv.Quote(l.Full) + ": learned ")
	switch l.Outcome {
	case OutcomeClock:
		b.WriteString("a clock read, checked in its call's span")
	case OutcomeValue:
		b.WriteString("a value bound at its source, checked at every later use")
	case OutcomeFresh:
		b.WriteString("a value made anew each run, bound at its first call and checked at every later use")
	default:
		b.WriteString("nothing")
	}
	if l.Rule != "" {
		b.WriteString(": " + l.Rule)
	}
	if l.From != "" {
		b.WriteString(", learned from round F's call in " + l.From + ", for learning only")
	}
	if len(l.Places) > 0 {
		b.WriteString(placesMark)
		for i, p := range l.Places {
			if i == maxLinePlaces {
				break
			}
			if i > 0 {
				b.WriteString(placeSep)
			}
			b.WriteString(p)
		}
	}
	undo := Edit{Kind: EditNoiseAtWord, Mock: l.Mock, Field: l.Field, Word: l.Word}
	if l.Word < 0 {
		undo = Edit{Kind: EditNoiseAtField, Mock: l.Mock, Field: l.Field, Word: -1}
	}
	b.WriteString(". Undo: " + editWords(undo) + editSep + editWords(Edit{Kind: EditReRecord}) + ".")
	return b.String()
}
