package learn

import (
	"encoding/json"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"
)

// A refused word's line is one line a person, an LLM or a tool
// reads:
//
//	[<test> ]<mock> <field>[ word <n>]: recorded "<r>", round F "<f>": refused (<kind>): <why>. Places: <p>, <p>, <p>. Edits: <edit>; or <edit>. edits=[{"kind":...}]
//
// The values are always quoted (strconv.Quote), so a value with quotes,
// colons or newlines stays on one line and reads back as it was. A name
// (test, mock, field, a given's name) is printed bare where it holds no
// space, quote or separator, and quoted otherwise. The why is printed bare
// unless it holds a section's start (". Places: ", ". Edits: "), a control
// byte or a leading quote. At most three places are printed; the edits are
// likeliest first, each in the words of the edit to make (keploy never
// makes it). The line ends with the same edits in one fixed form, a JSON
// array of {kind, mock, field, word, name} (lineEdit) after "edits=", for a
// tool to read without parsing the words. ParseRefusal reads every such
// line back to its Refusal.

// maxLinePlaces is the most places a line names.
const maxLinePlaces = 3

const (
	recordedMark = ": recorded "
	roundFMark   = ", round F "
	refusedMark  = ": refused ("
	limitMark    = ", keploy's limit"
	placesMark   = ". Places: "
	editsMark    = ". Edits: "
	editSep      = "; or "
	placeSep     = ", "
	editsTail    = " edits="
)

// lineEdit is an Edit in the line's fixed form.
type lineEdit struct {
	Kind  EditKind `json:"kind"`
	Mock  string   `json:"mock,omitempty"`
	Field string   `json:"field,omitempty"`
	Name  string   `json:"name,omitempty"`
	Word  int      `json:"word,omitempty"`
}

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
	var b strings.Builder
	b.WriteString(wordWhere(r.Test, r.Mock, r.Field, r.Word))
	b.WriteString(recordedMark + strconv.Quote(r.Recorded) + roundFMark + strconv.Quote(r.Full))
	b.WriteString(refusedMark + string(r.Kind))
	if r.Kind == RefusalCannotCheck {
		b.WriteString(limitMark)
	}
	b.WriteString("): ")
	if plainWhy(r.Why) {
		b.WriteString(r.Why)
	} else {
		b.WriteString(strconv.Quote(r.Why))
	}
	b.WriteString(placesPart(r.Places))
	tail, words := make([]lineEdit, len(r.Edits)), make([]string, len(r.Edits))
	for i, e := range r.Edits {
		tail[i], words[i] = lineEdit(e), editWords(e)
	}
	if len(words) > 0 {
		b.WriteString(editsMark + strings.Join(words, editSep))
	}
	js, _ := json.Marshal(tail) // a slice of strings and ints always marshals
	b.WriteString("." + editsTail + string(js))
	return b.String()
}

// placesPart is a line's places, each bare or quoted, at most three.
func placesPart(ps []string) string {
	if len(ps) == 0 {
		return ""
	}
	out := make([]string, 0, maxLinePlaces)
	for _, p := range ps[:min(len(ps), maxLinePlaces)] {
		if !plainPlace(p) {
			p = strconv.Quote(p)
		}
		out = append(out, p)
	}
	return placesMark + strings.Join(out, placeSep)
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

// lineName is s bare where it is a plain name: not empty, valid UTF-8, no
// space, separator, quote or control byte, and not ending in '.'; else s
// quoted.
func lineName(s string) string {
	if s == "" || !utf8.ValidString(s) || strings.HasSuffix(s, ".") || strings.ContainsAny(s, " :;,\"") || hasControl(s) {
		return strconv.Quote(s)
	}
	return s
}

// plainWhy reports whether why prints bare: it reads back whole.
func plainWhy(why string) bool {
	return why != "" && utf8.ValidString(why) && !hasControl(why) && !strings.HasPrefix(why, `"`) &&
		!strings.Contains(why, placesMark) && !strings.Contains(why, editsMark)
}

// plainPlace reports whether a place prints bare: it reads back whole.
func plainPlace(p string) bool {
	return p != "" && utf8.ValidString(p) && !hasControl(p) && !strings.HasPrefix(p, `"`) &&
		!strings.HasSuffix(p, ".") && !strings.Contains(p, placeSep) && !strings.Contains(p, ". ")
}

func hasControl(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < 0x20 || s[i] == 0x7f {
			return true
		}
	}
	return false
}

// ParseRefusal reads a refused word's line back to its Refusal; ok false
// where line is not one (a learned line, an edit of no known kind, text).
// A line printed with more than three places reads back with three. It
// takes the edits from the line's fixed tail, reads the head, and keeps
// the result only where printing it gives the line again, so a line it
// misreads is no refusal (fail closed).
func ParseRefusal(line string) (Refusal, bool) {
	// The tail is the last "edits=" whose rest is a JSON array; an earlier
	// one inside a quoted value never parses to the line's end.
	for end := len(line); end > 0; {
		if end = strings.LastIndex(line[:end], editsTail); end < 0 {
			break
		}
		var tail []lineEdit
		if json.Unmarshal([]byte(line[end+len(editsTail):]), &tail) != nil {
			continue
		}
		r, ok := parseHead(line[:end])
		for _, e := range tail {
			_, known := editForms[e.Kind]
			ok = ok && known
			r.Edits = append(r.Edits, Edit(e))
		}
		if ok && RefusalLine(r) == line {
			return r, true
		}
	}
	return Refusal{}, false
}

// whereEnds matches what follows a line's second name where that name is the
// field, in a line of no test.
var whereEnds = regexp.MustCompile("^( word [0-9]+)?" + recordedMark)

// parseHead reads the line before its tail: the word, both values, the
// kind, the why and the places. The words of the edits are skipped: the
// tail holds them.
func parseHead(s string) (Refusal, bool) {
	r := Refusal{Word: -1}
	p := lineReader{s: strings.TrimSuffix(s, "."), ok: strings.HasSuffix(s, ".")}
	r.Mock, r.Field = p.text(" "), p.lit(" ").text(" ", ":")
	if !whereEnds.MatchString(p.s) {
		r.Test, r.Mock, r.Field = r.Mock, r.Field, p.lit(" ").text(" ", ":")
	}
	if p.ok && strings.HasPrefix(p.s, " word ") {
		w, err := strconv.Atoi(p.lit(" word ").text(":"))
		r.Word, p.ok = w, p.ok && err == nil
	}
	r.Recorded = p.lit(recordedMark).text(",")
	r.Full = p.lit(roundFMark).text(":")
	r.Kind = RefusalKind(p.lit(refusedMark).text(",", ")"))
	if r.Kind == RefusalCannotCheck {
		p.lit(limitMark)
	}
	r.Why = p.lit("): ").text(placesMark, editsMark)
	if p.ok && strings.HasPrefix(p.s, placesMark) {
		for r.Places = []string{p.lit(placesMark).text(placeSep, editsMark)}; p.ok && strings.HasPrefix(p.s, placeSep); {
			r.Places = append(r.Places, p.lit(placeSep).text(placeSep, editsMark))
		}
	}
	if p.ok && strings.HasPrefix(p.s, editsMark) {
		p.s = ""
	}
	return r, p.ok && p.s == "" && refusalKinds[r.Kind]
}

// lineReader reads s from the front; ok turns false at the first piece
// that is not there, and every later read then gives nothing.
type lineReader struct {
	s  string
	ok bool
}

// lit consumes lit where s starts with it.
func (p *lineReader) lit(lit string) *lineReader {
	p.ok = p.ok && strings.HasPrefix(p.s, lit)
	if p.ok {
		p.s = p.s[len(lit):]
	}
	return p
}

// text consumes one Go-quoted string, or bare text up to the first stop
// (or s's end), as lineName, plainWhy and plainPlace print it.
func (p *lineReader) text(stops ...string) string {
	if !p.ok {
		return ""
	}
	if q, err := strconv.QuotedPrefix(p.s); err == nil && strings.HasPrefix(p.s, `"`) {
		v, _ := strconv.Unquote(q)
		p.s = p.s[len(q):]
		return v
	}
	end := len(p.s)
	for _, st := range stops {
		if j := strings.Index(p.s, st); j >= 0 && j < end {
			end = j
		}
	}
	v := p.s[:end]
	p.s, p.ok = p.s[end:], end > 0
	return v
}
