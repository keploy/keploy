package learn

import (
	"encoding/json"
	"strconv"
	"strings"
	"unicode/utf8"
)

// A refused word's line is one line a person, an LLM or a tool
// reads:
//
//	[<test> ]<mock> <field>[ word <n>]: recorded "<r>", round F "<f>": refused (<kind>): <why>. Places: <p>, <p>, <p>. Edits: <edit>; or <edit>. refusal={"kind":...}
//
// The values are always quoted (strconv.Quote), so a value with quotes,
// colons or newlines stays on one line and reads back as it was. A name
// (test, mock, field, a given's name) is printed bare where it holds no
// space, quote or separator, and quoted otherwise. A why or a place is
// printed bare unless that would break the line. At most three places are
// printed; the edits are likeliest first, each in the words of the edit to
// make (keploy never makes it). The line ends with the refusal but for its
// two values and its why in one fixed form, a JSON object after "refusal=",
// for a tool to read without parsing the words; each value stands once, in
// the sentence, where that tail fixes it. ParseRefusal reads every such line
// back to its Refusal.

// maxLinePlaces is the most places a line names.
const maxLinePlaces = 3

const (
	recordedMark = ": recorded "
	roundFMark   = ", round F "
	refusalTail  = " refusal="
	editSep      = "; or "
)

// editForms is each edit's words. %m is the mock, %f the field, %w the
// word, %n a given's name; each kind carries only the fields its words
// name (Edit's other fields are zero, Word -1 for a whole field).
var editForms = map[EditKind]string{
	EditMoveToMocks:   "move %m to this test's mocks: in mappings.yaml (it then binds)",
	EditMoveToStartup: "move %m to startup: in mappings.yaml (it then binds)",
	EditMoveToWorker:  "move %m to worker: in mappings.yaml (served, never required)",
	EditNoiseAtWord:   "noise at word %w of %f in %m, in keploy.yml",
	EditNoiseAtField:  "noise at %f in %m, in keploy.yml",
	EditGive:          "give it in the test: keploytest.Given(t, %n, v)",
	EditReRecord:      "re-record",
	EditFixApp:        "the app changed: fix it",
	EditNone:          "none, this is the guard",
}

// refusalKinds are the ten kinds, for reading a line back.
var refusalKinds = map[RefusalKind]bool{
	RefusalPick: true, RefusalNoClockRead: true, RefusalTwoSources: true, RefusalConflict: true,
	RefusalUnknown: true, RefusalAnotherScope: true, RefusalUnread: true, RefusalChangedShape: true,
	RefusalCannotCheck: true, RefusalLandedElsewhere: true,
}

// RefusalLine is r's one line (see above).
func RefusalLine(r Refusal) string {
	return wordWhere(r.Test, r.Mock, r.Field, r.Word) + recordedMark + strconv.Quote(r.Recorded) + roundFMark + strconv.Quote(r.Full) +
		refusedClause(r.Kind) + printed(r.Why) + refusalFoot(r)
}

// refusedClause is what stands between a line's values and its why.
func refusedClause(k RefusalKind) string {
	if k == RefusalCannotCheck {
		return ": refused (" + string(k) + ", keploy's limit): "
	}
	return ": refused (" + string(k) + "): "
}

// refusalFoot is what follows r's why: its places, its edits in words and
// the line's fixed tail.
func refusalFoot(r Refusal) string {
	r.Places = r.Places[:min(len(r.Places), maxLinePlaces)]
	s := placesPart(r.Places)
	sep := ". Edits: "
	for _, e := range r.Edits {
		s, sep = s+sep+editWords(e), editSep
	}
	r.Recorded, r.Full, r.Why = "", "", ""
	js, _ := json.Marshal(r) // a struct of strings and ints always marshals
	return s + "." + refusalTail + string(js)
}

// placesPart is a line's places, at most three.
func placesPart(ps []string) string {
	s, sep := "", ". Places: "
	for _, p := range ps[:min(len(ps), maxLinePlaces)] {
		s, sep = s+sep+printed(p), ", "
	}
	return s
}

// wordWhere is "[<test> ]<mock> <field>[ word <n>]", each name bare or quoted.
func wordWhere(test, mock, field string, word int) string {
	s := lineName(mock) + " " + lineName(field)
	if test != "" {
		s = lineName(test) + " " + s
	}
	if word >= 0 {
		s += " word " + strconv.Itoa(word)
	}
	return s
}

// editWords is e in its kind's words; an edit of no known kind prints its
// kind alone (it then reads back as no refusal: fail closed).
func editWords(e Edit) string {
	form, ok := editForms[e.Kind]
	if !ok {
		return lineName(string(e.Kind))
	}
	return strings.NewReplacer("%m", lineName(e.Mock), "%f", lineName(e.Field), "%w", strconv.Itoa(e.Word), "%n", strconv.Quote(e.Name)).Replace(form)
}

// printed is a why or a place as a line prints it: bare, or quoted where it
// is empty or bare would break the line.
func printed(s string) string {
	if s == "" || !utf8.ValidString(s) || strings.ContainsFunc(s, func(r rune) bool { return r < 0x20 || r == 0x7f }) {
		return strconv.Quote(s)
	}
	return s
}

// lineName is s bare where it is a plain name: printed bare, with no space,
// separator or quote, and not ending in '.'; else s quoted.
func lineName(s string) string {
	if printed(s) != s || strings.HasSuffix(s, ".") || strings.ContainsAny(s, " :;,\"") {
		return strconv.Quote(s)
	}
	return s
}

// ParseRefusal reads a refused word's line back to its Refusal; ok false
// where line is not one (a learned line, a kind or an edit of no known kind,
// text). A line printed with more than three places reads back with three.
// It takes all but the values and the why from the line's fixed tail, reads
// those where the tail fixes them, and keeps the result only where printing
// it gives the line again, so a line it misreads is no refusal (fail closed).
func ParseRefusal(line string) (Refusal, bool) {
	// The tail is the last "refusal=" whose rest is a JSON object; an earlier
	// one inside a quoted value never parses to the line's end.
	for end := len(line); end > 0; {
		if end = strings.LastIndex(line[:end], refusalTail); end < 0 {
			break
		}
		var r Refusal
		if json.Unmarshal([]byte(line[end+len(refusalTail):]), &r) != nil {
			continue
		}
		ok := refusalKinds[r.Kind] && readTexts(&r, line)
		for _, e := range r.Edits {
			_, known := editForms[e.Kind]
			ok = ok && known
		}
		if ok && RefusalLine(r) == line {
			return r, true
		}
	}
	return Refusal{}, false
}

// readTexts reads r's two values and its why from line, between the parts
// r's tail fixes; false where line does not hold them there.
func readTexts(r *Refusal, line string) bool {
	s, atRec := strings.CutPrefix(line, wordWhere(r.Test, r.Mock, r.Field, r.Word)+recordedMark)
	rec, s, okR := cutQuoted(s)
	s, atF := strings.CutPrefix(s, roundFMark)
	full, s, okF := cutQuoted(s)
	s, atWhy := strings.CutPrefix(s, refusedClause(r.Kind))
	why, ended := strings.CutSuffix(s, refusalFoot(*r))
	if u, err := strconv.Unquote(why); err == nil && printed(u) == why {
		why = u
	}
	r.Recorded, r.Full, r.Why = rec, full, why
	return atRec && okR && atF && okF && atWhy && ended
}

// cutQuoted reads one Go-quoted string from the front of s, and the rest.
func cutQuoted(s string) (v, rest string, ok bool) {
	q, err := strconv.QuotedPrefix(s)
	if err != nil {
		return "", s, false
	}
	v, err = strconv.Unquote(q)
	return v, s[len(q):], err == nil
}
