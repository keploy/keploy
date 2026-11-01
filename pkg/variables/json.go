package variables

import (
	"encoding/json"
	"errors"
	"io"
	"math/big"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode/utf16"
	"unicode/utf8"

	"go.keploy.io/server/v3/pkg/templates"
)

// IsJSONDoc reports whether s is one JSON document.
func IsJSONDoc(s string) bool { return json.Valid([]byte(s)) }

// JSONLeafAt returns the scalar of doc at path: doc must be one JSON document,
// and the scalar the one a reader sees there (a repeated key's last value).
func JSONLeafAt(doc string, path []templates.PathSeg) (templates.Leaf, bool) {
	if !IsJSONDoc(doc) {
		return templates.Leaf{}, false
	}
	leaves, ok := templates.JSONLeaves(doc)
	if !ok {
		return templates.Leaf{}, false
	}
	for _, l := range leaves {
		if !l.Shadowed && samePath(l.Path, path) {
			return l, true
		}
	}
	return templates.Leaf{}, false
}

// IsNumberLeaf reports whether l is a JSON number.
func IsNumberLeaf(l templates.Leaf) bool { return l.Kind == templates.LeafNumber }

// SpliceAt writes raw, a JSON value, in place of the scalar of doc at path.
func SpliceAt(doc string, path []templates.PathSeg, raw string) (string, bool) {
	l, ok := JSONLeafAt(doc, path)
	if !ok {
		return doc, false
	}
	return doc[:l.Start] + raw + doc[l.End:], true
}

// scalarText is how a value is written at a leaf of kind k: a JSON string at a
// string leaf; the value itself at a number, boolean or null leaf, only when
// it is one of those. false when the value cannot stand there.
func scalarText(k templates.LeafKind, value string) (string, bool) {
	switch k {
	case templates.LeafString:
		b, err := json.Marshal(value)
		if err != nil {
			return "", false
		}
		return string(b), true
	case templates.LeafNumber:
		return value, templates.IsNumber(value)
	case templates.LeafBool:
		return value, value == "true" || value == "false"
	case templates.LeafNull:
		return value, value == "null"
	}
	return "", false
}

// SameValue reports whether a place holds a variable's value. Text is compared
// as written; where either side is a JSON number, two numbers are the same when
// their values are (1.50 is 1.5).
func SameValue(want, got string, number bool) bool {
	if want == got {
		return true
	}
	if !number || !templates.IsNumber(want) || !templates.IsNumber(got) {
		return false
	}
	x, okX := new(big.Rat).SetString(want)
	y, okY := new(big.Rat).SetString(got)
	return okX && okY && x.Cmp(y) == 0
}

// Edit replaces the bytes [Start, End) of a body with Raw.
type Edit struct {
	Start, End int
	Raw        string
}

// AnswerEdit is the edit that makes the scalar of body at path hold value,
// written as that scalar's JSON type is written. The scalar must hold recorded
// (the variable's recorded value); otherwise, or when value cannot stand
// there, it says why.
func AnswerEdit(body string, path []templates.PathSeg, recorded, value string) (Edit, string) {
	l, ok := JSONLeafAt(body, path)
	if !ok {
		return Edit{}, "the recorded response holds no scalar there"
	}
	if !SameValue(recorded, l.Value(body), IsNumberLeaf(l)) {
		return Edit{}, "the recorded response holds " + l.Raw(body) + " there, not the variable's recorded value " + quote(recorded)
	}
	raw, ok := scalarText(l.Kind, value)
	if !ok {
		return Edit{}, "the value " + quote(value) + " cannot stand at a place the recording holds " + l.Raw(body)
	}
	return Edit{Start: l.Start, End: l.End, Raw: raw}, ""
}

// ApplyEdits applies edits to body; false when two of them overlap.
func ApplyEdits(body string, edits []Edit) (string, bool) {
	if len(edits) == 0 {
		return body, true
	}
	es := append([]Edit(nil), edits...)
	sort.Slice(es, func(i, j int) bool { return es[i].Start > es[j].Start })
	for i := 1; i < len(es); i++ {
		if es[i].End > es[i-1].Start {
			return body, false
		}
	}
	for _, e := range es {
		body = body[:e.Start] + e.Raw + body[e.End:]
	}
	return body, true
}

func quote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// Noise is the known noise of a request body, as the variable pass reads it.
// A path covers its place and everything under it, read by exact path, never
// as a substring: "a.b" covers a.b, a.b.c and the elements of an array at a.b
// (array steps are not written, or written [] after the key that holds the
// array, as keploy's learned noise writes them: "a.b" and "a[].b" are one
// path; lower case; a "body." prefix is dropped). A path
// with patterns covers only a scalar whose recorded and live values each match
// one of them. Under noise a value may change, but not its JSON type.
type Noise struct {
	paths map[string][]*regexp.Regexp
	// RecordedValueIsNoise, when set, says a recorded scalar is a redacted
	// value (the obfuscator's): any live value of its type stands there.
	RecordedValueIsNoise func(string) bool
}

// Add adds a noise path and its patterns (none: the whole place). A pattern
// that does not compile makes the path cover nothing.
func (n *Noise) Add(path string, patterns []string) {
	p := noisePath(strings.TrimPrefix(strings.TrimSpace(path), "body."))
	if p == "" {
		return
	}
	if n.paths == nil {
		n.paths = map[string][]*regexp.Regexp{}
	}
	if _, have := n.paths[p]; have && len(n.paths[p]) == 0 {
		return // the whole place is already noise
	}
	if len(patterns) == 0 {
		n.paths[p] = nil
		return
	}
	res := n.paths[p]
	for _, s := range patterns {
		re, err := regexp.Compile(s)
		if err != nil {
			continue
		}
		res = append(res, re)
	}
	if len(res) == 0 {
		res = []*regexp.Regexp{regexp.MustCompile(`$^`)} // matches nothing
	}
	n.paths[p] = res
}

// noisePath is a path as noise is looked up: lower case, array steps dropped.
func noisePath(p string) string {
	return strings.ToLower(strings.ReplaceAll(p, "[]", ""))
}

// whole reports whether a path, or one holding it, is noise as a whole.
func (n Noise) whole(path string) bool {
	if len(n.paths) == 0 {
		return false
	}
	p := noisePath(path)
	for p != "" {
		if res, ok := n.paths[p]; ok && len(res) == 0 {
			return true
		}
		cut := strings.LastIndexByte(p, '.')
		if cut <= 0 {
			return false
		}
		p = p[:cut]
	}
	return false
}

// patterned reports whether a scalar at path is noise by a pattern its two
// values both match.
func (n Noise) patterned(path, rec, live string) bool {
	res := n.paths[noisePath(path)]
	if len(res) == 0 {
		return false
	}
	match := func(v string) bool {
		for _, re := range res {
			if re.MatchString(v) {
				return true
			}
		}
		return false
	}
	return match(rec) && match(live)
}

// SameJSONUnder reports whether two JSON documents are the same document
// outside noise: every key, every array element by position, every scalar by
// value (numbers by exact value).
func SameJSONUnder(recorded, live string, n Noise) bool {
	var r, l any
	if !decodeNumbers(recorded, &r) || !decodeNumbers(live, &l) {
		return false
	}
	return sameUnder(r, l, "", n)
}

// SameJSON is SameJSONUnder with whole-place noise paths.
func SameJSON(recorded, live string, noise []string) bool {
	var n Noise
	for _, p := range noise {
		n.Add(p, nil)
	}
	return SameJSONUnder(recorded, live, n)
}

func decodeNumbers(doc string, out *any) bool {
	dec := json.NewDecoder(strings.NewReader(doc))
	dec.UseNumber()
	if err := dec.Decode(out); err != nil {
		return false
	}
	// One document and nothing after it.
	_, err := dec.Token()
	return errors.Is(err, io.EOF)
}

func jsonType(v any) string {
	switch v.(type) {
	case map[string]any:
		return "object"
	case []any:
		return "array"
	case string:
		return "string"
	case json.Number:
		return "number"
	case bool:
		return "bool"
	case nil:
		return "null"
	}
	return "?"
}

func scalarString(v any) (string, bool) {
	switch t := v.(type) {
	case string:
		return t, true
	case json.Number:
		return t.String(), true
	case bool:
		if t {
			return "true", true
		}
		return "false", true
	case nil:
		return "null", true
	}
	return "", false
}

func sameUnder(r, l any, path string, n Noise) bool {
	if jsonType(r) != jsonType(l) {
		return false
	}
	if n.whole(path) {
		return true
	}
	switch rv := r.(type) {
	case map[string]any:
		lv := l.(map[string]any)
		if len(rv) != len(lv) {
			return false
		}
		for k, rc := range rv {
			lc, ok := lv[k]
			if !ok {
				return false
			}
			p := k
			if path != "" {
				p = path + "." + k
			}
			if !sameUnder(rc, lc, p, n) {
				return false
			}
		}
		return true
	case []any:
		lv := l.([]any)
		if len(rv) != len(lv) {
			return false
		}
		p := path + "[]"
		for i := range rv {
			if !sameUnder(rv[i], lv[i], p, n) {
				return false
			}
		}
		return true
	}
	rs, _ := scalarString(r)
	ls, _ := scalarString(l)
	if n.RecordedValueIsNoise != nil && n.RecordedValueIsNoise(rs) {
		return true
	}
	if n.patterned(path, rs, ls) {
		return true
	}
	if num, ok := r.(json.Number); ok {
		return SameValue(num.String(), ls, true)
	}
	return r == l
}

// SpliceJSONString is raw, a JSON string literal, rewritten to hold next:
// every byte of raw before and after the text that changes is kept as
// recorded (its escapes, its '<' or "\u003c", its non-ASCII letters), and
// only the changed text is encoded, in raw's style (HTML escapes where raw
// shows one). Where raw is no string literal, or keploy cannot keep its
// bytes (an invalid UTF-8 byte in it), next is encoded whole. ok is false
// where next cannot be written as JSON.
func SpliceJSONString(raw, next string) (string, bool) {
	html := strings.Contains(raw, `\u003c`) || strings.Contains(raw, `\u003e`) || strings.Contains(raw, `\u0026`)
	var inner string
	if json.Unmarshal([]byte(raw), &inner) != nil || len(raw) < 2 || raw[0] != '"' {
		return jsonString(next, html)
	}
	// units are where each unit of raw (one character as written, an
	// escape, or a surrogate pair) starts, in raw and in what it decodes
	// to; the last is raw's closing quote and len(inner).
	type unit struct{ raw, dec int }
	units := make([]unit, 0, len(raw))
	dec := 0
	for i := 1; i < len(raw)-1; {
		units = append(units, unit{i, dec})
		n, d := jsonUnit(raw[i : len(raw)-1])
		if n == 0 {
			return jsonString(next, html)
		}
		i, dec = i+n, dec+d
	}
	if dec != len(inner) {
		return jsonString(next, html)
	}
	units = append(units, unit{len(raw) - 1, dec})
	p := 0
	for p < len(inner) && p < len(next) && inner[p] == next[p] {
		p++
	}
	q := 0
	for q < len(inner)-p && q < len(next)-p && inner[len(inner)-1-q] == next[len(next)-1-q] {
		q++
	}
	// The kept bytes end and begin at units' bounds.
	lo := 0
	for lo+1 < len(units) && units[lo+1].dec <= p {
		lo++
	}
	hi := len(units) - 1
	for hi > 0 && units[hi-1].dec >= len(inner)-q {
		hi--
	}
	if hi < lo {
		hi = lo
	}
	mid, ok := jsonString(next[units[lo].dec:len(next)-(len(inner)-units[hi].dec)], html)
	if !ok {
		return "", false
	}
	out := raw[:units[lo].raw] + mid[1:len(mid)-1] + raw[units[hi].raw:]
	var got string
	if json.Unmarshal([]byte(out), &got) != nil || got != next {
		return jsonString(next, html)
	}
	return out, true
}

// jsonUnit is the length of the unit s begins with in a JSON string
// literal's text (a character as written, an escape, or a surrogate pair's
// two escapes), and the length of what it decodes to; 0 where s begins with
// no unit encoding/json decodes byte for byte.
func jsonUnit(s string) (n, dec int) {
	switch c := s[0]; {
	case c == '\\' && len(s) >= 6 && s[1] == 'u':
		r, err := strconv.ParseUint(s[2:6], 16, 32)
		if err != nil {
			return 0, 0
		}
		if utf16.IsSurrogate(rune(r)) {
			if len(s) >= 12 && s[6] == '\\' && s[7] == 'u' {
				if r2, err := strconv.ParseUint(s[8:12], 16, 32); err == nil {
					if p := utf16.DecodeRune(rune(r), rune(r2)); p != utf8.RuneError {
						return 12, utf8.RuneLen(p)
					}
				}
			}
			return 6, utf8.RuneLen(utf8.RuneError)
		}
		return 6, utf8.RuneLen(rune(r))
	case c == '\\' && len(s) >= 2:
		return 2, 1
	case c == '\\' || c == '"':
		return 0, 0
	case c < utf8.RuneSelf:
		return 1, 1
	}
	r, size := utf8.DecodeRuneInString(s)
	if r == utf8.RuneError && size == 1 {
		return 0, 0 // an invalid byte decodes to U+FFFD: no byte kept as written
	}
	return size, size
}
