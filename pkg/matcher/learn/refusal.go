package learn

import (
	"strconv"
	"strings"
	"unicode/utf8"
)

// A refused word's line is one line a person, an LLM or a tool
// reads:
//
//	<test> <mock> <field>[ word <n>]: recorded "<r>", round F "<f>": refused (<kind>): <why>. Places: <p>, <p>, <p>. Edits: <edit>; or <edit>.
//
// The values are always quoted (strconv.Quote), so a value with quotes,
// colons or newlines stays on one line and reads back as it was. A name
// (test, mock, field, a given's name) is printed bare where it holds no
// space, quote or separator, and quoted otherwise. The why is printed bare
// unless it holds a section's start (". Places: ", ". Edits: "), a control
// byte or a leading quote. At most three places are printed; the edits are
// likeliest first, each in the words of the edit to make (keploy never
// makes it). ParseRefusal reads every such line back to its Refusal.

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

// refusalKinds are the nine kinds, for reading a line back.
var refusalKinds = map[RefusalKind]bool{
	RefusalPick: true, RefusalNoClockRead: true, RefusalTwoSources: true, RefusalConflict: true,
	RefusalUnknown: true, RefusalAnotherScope: true, RefusalUnread: true, RefusalChangedShape: true,
	RefusalCannotCheck: true,
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
	if len(r.Places) > 0 {
		b.WriteString(placesMark)
		for i, p := range r.Places {
			if i == maxLinePlaces {
				break
			}
			if i > 0 {
				b.WriteString(placeSep)
			}
			if plainPlace(p) {
				b.WriteString(p)
			} else {
				b.WriteString(strconv.Quote(p))
			}
		}
	}
	if len(r.Edits) > 0 {
		b.WriteString(editsMark)
		for i, e := range r.Edits {
			if i > 0 {
				b.WriteString(editSep)
			}
			b.WriteString(editWords(e))
		}
	}
	b.WriteString(".")
	return b.String()
}

// wordWhere is "<test> <mock> <field>[ word <n>]", each name bare or quoted.
func wordWhere(test, mock, field string, word int) string {
	s := lineName(test) + " " + lineName(mock) + " " + lineName(field)
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
	var b strings.Builder
	for i := 0; i < len(form); i++ {
		if form[i] != '%' || i+1 == len(form) {
			b.WriteByte(form[i])
			continue
		}
		i++
		switch form[i] {
		case 'm':
			b.WriteString(lineName(e.Mock))
		case 'f':
			b.WriteString(lineName(e.Field))
		case 'w':
			b.WriteString(strconv.Itoa(e.Word))
		case 'n':
			b.WriteString(strconv.Quote(e.Name))
		}
	}
	return b.String()
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
	return utf8.ValidString(why) && !hasControl(why) && !strings.HasPrefix(why, `"`) &&
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
// A line printed with more than three places reads back with three.
func ParseRefusal(line string) (Refusal, bool) {
	var r Refusal
	p := lineReader{s: line}
	var ok bool
	if r.Test, ok = p.name(); !ok || !p.lit(" ") {
		return Refusal{}, false
	}
	if r.Mock, ok = p.name(); !ok || !p.lit(" ") {
		return Refusal{}, false
	}
	if r.Field, ok = p.name(); !ok {
		return Refusal{}, false
	}
	r.Word = -1
	if p.lit(" word ") {
		if r.Word, ok = p.number(); !ok {
			return Refusal{}, false
		}
	}
	if !p.lit(recordedMark) {
		return Refusal{}, false
	}
	if r.Recorded, ok = p.quoted(); !ok || !p.lit(roundFMark) {
		return Refusal{}, false
	}
	if r.Full, ok = p.quoted(); !ok || !p.lit(refusedMark) {
		return Refusal{}, false
	}
	end := strings.IndexAny(p.rest(), ",)")
	if end < 0 {
		return Refusal{}, false
	}
	r.Kind = RefusalKind(p.rest()[:end])
	p.i += end
	if !refusalKinds[r.Kind] || p.lit(limitMark) != (r.Kind == RefusalCannotCheck) || !p.lit("): ") {
		return Refusal{}, false
	}
	if strings.HasPrefix(p.rest(), `"`) {
		if r.Why, ok = p.quoted(); !ok {
			return Refusal{}, false
		}
	} else {
		rest := p.rest()
		end := len(rest) - 1
		for _, m := range []string{placesMark, editsMark} {
			if j := strings.Index(rest, m); j >= 0 && j < end {
				end = j
			}
		}
		if end < 0 {
			return Refusal{}, false
		}
		r.Why = rest[:end]
		p.i += end
	}
	if p.lit(placesMark) {
		for {
			pl, ok := p.place()
			if !ok {
				return Refusal{}, false
			}
			r.Places = append(r.Places, pl)
			if !p.lit(placeSep) {
				break
			}
		}
	}
	if p.lit(editsMark) {
		for {
			e, ok := p.edit()
			if !ok {
				return Refusal{}, false
			}
			r.Edits = append(r.Edits, e)
			if !p.lit(editSep) {
				break
			}
		}
	}
	if p.rest() != "." {
		return Refusal{}, false
	}
	return r, true
}

// lineReader reads a line from i on.
type lineReader struct {
	s string
	i int
}

func (p *lineReader) rest() string { return p.s[p.i:] }

// lit consumes lit where the rest starts with it.
func (p *lineReader) lit(lit string) bool {
	if !strings.HasPrefix(p.rest(), lit) {
		return false
	}
	p.i += len(lit)
	return true
}

// quoted consumes one Go-quoted string.
func (p *lineReader) quoted() (string, bool) {
	q, err := strconv.QuotedPrefix(p.rest())
	if err != nil {
		return "", false
	}
	s, err := strconv.Unquote(q)
	if err != nil {
		return "", false
	}
	p.i += len(q)
	return s, true
}

// name consumes a name as lineName prints it.
func (p *lineReader) name() (string, bool) {
	if strings.HasPrefix(p.rest(), `"`) {
		return p.quoted()
	}
	end := strings.IndexAny(p.rest(), " :;,\"")
	if end < 0 {
		end = len(p.rest())
	}
	if end == 0 {
		return "", false
	}
	s := p.rest()[:end]
	p.i += end
	return s, true
}

// number consumes a non-negative decimal.
func (p *lineReader) number() (int, bool) {
	end := 0
	for end < len(p.rest()) && p.rest()[end] >= '0' && p.rest()[end] <= '9' {
		end++
	}
	n, err := strconv.Atoi(p.rest()[:end])
	if err != nil {
		return 0, false
	}
	p.i += end
	return n, true
}

// place consumes a place as RefusalLine prints it: bare up to the next
// place, the edits or the line's end.
func (p *lineReader) place() (string, bool) {
	if strings.HasPrefix(p.rest(), `"`) {
		return p.quoted()
	}
	rest := p.rest()
	end := len(rest) - 1
	for _, m := range []string{placeSep, editsMark} {
		if j := strings.Index(rest, m); j >= 0 && j < end {
			end = j
		}
	}
	if end <= 0 {
		return "", false
	}
	p.i += end
	return rest[:end], true
}

// edit consumes one edit in its kind's words: the first form whose words
// and names read whole there.
func (p *lineReader) edit() (Edit, bool) {
	for kind, form := range editForms {
		q := *p
		e, ok := q.form(kind, form)
		if ok && (strings.HasPrefix(q.rest(), editSep) || q.rest() == ".") {
			*p = q
			return e, true
		}
	}
	return Edit{}, false
}

// form reads one edit of kind in form's words.
func (p *lineReader) form(kind EditKind, form string) (Edit, bool) {
	e := Edit{Kind: kind}
	if kind == EditNoiseAtField {
		e.Word = -1
	}
	for i := 0; i < len(form); i++ {
		if form[i] != '%' || i+1 == len(form) {
			if !p.lit(form[i : i+1]) {
				return Edit{}, false
			}
			continue
		}
		i++
		var ok bool
		switch form[i] {
		case 'm':
			e.Mock, ok = p.name()
		case 'f':
			e.Field, ok = p.name()
		case 'w':
			e.Word, ok = p.number()
		case 'n':
			e.Name, ok = p.quoted()
		}
		if !ok {
			return Edit{}, false
		}
	}
	return e, true
}
