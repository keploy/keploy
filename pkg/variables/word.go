package variables

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"
)

// A word address names one word of a value: the place that holds the value,
// the decode steps that open it, and the word's index there:
//
//	<place>[|<step>]...[#<n>]
//
//	body.resourceKey[0]#1           the second word of a JSON scalar
//	header.Authorization|jwt.sub    the sub claim of a JWT, whole
//	header.Cookie|cookie.sid#0      the first word of the cookie sid
//	url.query.cursor|b64url|pct#2   the third word of a base64url cursor's
//	                                percent-decoded text
//
// A word is a run of ASCII letters and digits (Words), so "-", "_", "+",
// "/", "=" and "." always split. #n counts them from 0 in the text the last
// step gives; with no #n the address names that text whole. A plain place
// (no step, no word) is written as Place.String writes it, so a place
// written before word addresses prints the same (and reads the same through
// Parse and ReadPlace; ParseAddress states its limit).
//
// The steps:
//
//	pct            percent-decoded (%XX; '+' stays '+')
//	json           one JSON string literal or number, read as its value
//	b64, b64url    base64 in the standard or the URL alphabet, padded or not
//	jwt.<claim>    the claim of a JWT's payload (a string, or a number's text)
//	cookie.<name>  the value of the one pair named <name> in a Cookie or
//	               Set-Cookie value
//
// A claim or cookie name escapes '|', '#' and '\' as a query or header name
// does (Parse).

// Step is one decode step of a word address: "pct", "json", "b64", "b64url",
// "jwt.<claim>" or "cookie.<name>", its name unescaped.
type Step string

// The steps with no argument, and the prefixes of the two with one.
const (
	StepPct      Step = "pct"
	StepJSON     Step = "json"
	StepB64      Step = "b64"
	StepB64URL   Step = "b64url"
	jwtPrefix         = "jwt."
	cookiePrefix      = "cookie."
)

// JWTClaim is the step to a JWT's claim.
func JWTClaim(claim string) Step { return Step(jwtPrefix + claim) }

// Cookie is the step to the value of the cookie name.
func Cookie(name string) Step { return Step(cookiePrefix + name) }

// Address is a word address: Word -1 names the text the steps give whole.
type Address struct {
	Place Place
	Steps []Step
	Word  int
}

// ParseAddress reads a word address. A string with no step and no word reads
// as Parse reads it, with Word -1; so does one Parse reads whole whose '|'
// or '#' does not start a step or a word (a name written before the escape).
// So a mistyped step reads as part of a name no call holds: it fails at
// replay (nothing there to bind or check), not here. On an error, Word -1.
//
// Its limit: a name written before word addresses that ends in #<digits>
// ("url.query.page#2") reads here as a word of the shorter name, and one
// that holds '|' and a step's name ("url.query.a|json", "body.a|b64") reads
// here as the shorter name and that step, where Parse reads the whole name
// in both. A place of a mock's variables block is read with ReadPlace,
// which reads such a name whole where the recording holds it.
func ParseAddress(s string) (Address, error) {
	a, err := parseAddress(s)
	if err != nil {
		if p, perr := Parse(s); perr == nil {
			return Address{Place: p, Word: -1}, nil
		}
		return Address{Word: -1}, err
	}
	return a, nil
}

// ReadPlace reads s, a place of a mock's variables block (a bind, a use or
// an answer of models.MockVariable), as each protocol's replay reads one: as
// Parse reads it where the recording holds a value there (holds), so a place
// written before word addresses reads as it did, a name that ends in
// #<digits> or holds '|' included; else as ParseAddress reads it, a word or
// a decode step of a shorter place. Another protocol's learn step writes a
// word address there: a Mongo call that copies one word of a field an HTTP
// call sent binds its variable at that word of the HTTP recording. A place
// the recording holds neither way reads as ParseAddress reads it, and its
// reader finds no value there.
func ReadPlace(s string, holds func(Place) bool) (Address, error) {
	if p, err := Parse(s); err == nil && holds(p) {
		return Address{Place: p, Word: -1}, nil
	}
	return ParseAddress(s)
}

// Whole reports whether a names its place's value whole: no step, no word.
func (a Address) Whole() bool { return a.Word < 0 && len(a.Steps) == 0 }

func parseAddress(s string) (Address, error) {
	end := placeEnd(s)
	p, err := Parse(s[:end])
	if err != nil {
		return Address{}, err
	}
	a := Address{Place: p, Word: -1}
	rest := s[end:]
	for strings.HasPrefix(rest, "|") {
		n := nameEnd(rest, 1)
		st := Step(unescapeName(rest[1:n]))
		if !st.valid() {
			return Address{}, fmt.Errorf("not a word address: %q: step %q (want pct, json, b64, b64url, jwt.<claim> or cookie.<name>)", s, rest[1:n])
		}
		a.Steps = append(a.Steps, st)
		rest = rest[n:]
	}
	if strings.HasPrefix(rest, "#") {
		n, err := strconv.Atoi(rest[1:])
		if err != nil || n < 0 || strconv.Itoa(n) != rest[1:] {
			return Address{}, fmt.Errorf("not a word address: %q: the word is a number from 0", s)
		}
		a.Word, rest = n, ""
	}
	if rest != "" {
		return Address{}, fmt.Errorf("not a word address: %q: %q after the place", s, rest)
	}
	return a, nil
}

// placeEnd is where s's place ends: at its first '|' or '#' outside a
// quoted JSON key or an escape, after the ']' of url.path[n].
func placeEnd(s string) int {
	switch {
	case strings.HasPrefix(s, pathPrefix):
		if i := strings.IndexByte(s, ']'); i >= 0 {
			return i + 1
		}
	case strings.HasPrefix(s, queryPrefix):
		return nameEnd(s, len(queryPrefix))
	case strings.HasPrefix(s, headerPrefix):
		return nameEnd(s, len(headerPrefix))
	case strings.HasPrefix(s, bodyPrefix):
		for i := len(bodyPrefix); i < len(s); i++ {
			switch {
			case s[i] == '|' || s[i] == '#':
				return i
			case s[i] == '[' && i+1 < len(s) && s[i+1] == '"':
				for i += 2; i < len(s) && s[i] != '"'; i++ {
					if s[i] == '\\' {
						i++
					}
				}
			}
		}
	}
	return len(s)
}

// nameEnd is the first '|' or '#' of s from i on that no '\' escapes.
func nameEnd(s string, i int) int {
	for ; i < len(s) && s[i] != '|' && s[i] != '#'; i++ {
		if s[i] == '\\' {
			i++
		}
	}
	return min(i, len(s))
}

func (st Step) valid() bool {
	switch {
	case st == StepPct || st == StepJSON || st == StepB64 || st == StepB64URL:
		return true
	case strings.HasPrefix(string(st), jwtPrefix):
		return len(st) > len(jwtPrefix)
	case strings.HasPrefix(string(st), cookiePrefix):
		return len(st) > len(cookiePrefix)
	}
	return false
}

// String is the address as ParseAddress reads it; a plain place's is
// Place.String's. An address of no place (Place.String "") names nothing,
// whatever its steps and word, and prints "".
func (a Address) String() string {
	place := a.Place.String()
	if place == "" {
		return ""
	}
	var b strings.Builder
	b.WriteString(place)
	for _, st := range a.Steps {
		b.WriteString("|" + escapeName(string(st)))
	}
	if a.Word >= 0 {
		b.WriteString("#" + strconv.Itoa(a.Word))
	}
	return b.String()
}

// Words is where s's words are: its runs of ASCII letters and digits, each
// [start, end) in bytes, in order. It is the learn step's word rule (isWord)
// and replay's.
func Words(s string) [][2]int {
	var out [][2]int
	for i := 0; i < len(s); {
		if !isWordByte(s[i]) {
			i++
			continue
		}
		j := i + 1
		for j < len(s) && isWordByte(s[j]) {
			j++
		}
		out = append(out, [2]int{i, j})
		i = j
	}
	return out
}

func isWordByte(c byte) bool {
	return c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z'
}

// WordOf is the word a names in leaf, the value at a's place: leaf decoded
// by each step, then its Word-th word (the text whole for Word -1). ok is
// false where a step does not decode or there is no such word.
func (a Address) WordOf(leaf string) (string, bool) {
	text := leaf
	for _, st := range a.Steps {
		var ok bool
		if text, ok = Decode(st, text); !ok {
			return "", false
		}
	}
	return wordAt(text, a.Word)
}

func wordAt(text string, n int) (string, bool) {
	if n < 0 {
		return text, true
	}
	ws := Words(text)
	if n >= len(ws) {
		return "", false
	}
	return text[ws[n][0]:ws[n][1]], true
}

// ReplaceWord is leaf with the word a names set to v, every step encoded
// back the way leaf was, every other byte kept where a step allows (pct
// and cookie keep them; b64 and json re-encode the text, where leaf is
// their own encoding of it). base64 keeps leaf's padding, padded or raw;
// where leaf does not show which (its text needs no '=') and the padded
// and raw forms of the new text differ, it refuses. ok is false where it
// cannot: a step leaf does not decode by, a JWT claim (its signature would
// break), a word a does not find, a re-encoding that is not leaf's own, a
// padding leaf does not show, a cookie value v cannot be, or a v that a
// reads back otherwise (one that joins the words beside it).
func (a Address) ReplaceWord(leaf, v string) (string, bool) {
	out, ok := replaceIn(leaf, a.Steps, a.Word, v)
	if !ok {
		return "", false
	}
	if got, ok := a.WordOf(out); !ok || got != v {
		return "", false
	}
	return out, true
}

func replaceIn(text string, steps []Step, n int, v string) (string, bool) {
	if len(steps) == 0 {
		if n < 0 {
			return v, true
		}
		ws := Words(text)
		if n >= len(ws) {
			return "", false
		}
		return text[:ws[n][0]] + v + text[ws[n][1]:], true
	}
	inner, ok := Decode(steps[0], text)
	if !ok {
		return "", false
	}
	next, ok := replaceIn(inner, steps[1:], n, v)
	if !ok {
		return "", false
	}
	return splice(steps[0], text, inner, next)
}

// splice is text, which step decodes to inner, encoded back holding next.
func splice(st Step, text, inner, next string) (string, bool) {
	switch {
	case st == StepPct:
		// Keep the bytes before and after the change as written.
		p := 0
		for p < len(inner) && p < len(next) && inner[p] == next[p] {
			p++
		}
		q := 0
		for q < len(inner)-p && q < len(next)-p && inner[len(inner)-1-q] == next[len(next)-1-q] {
			q++
		}
		at := pctOffsets(text)
		return text[:at[p]] + pctEncode(next[p:len(next)-q]) + text[at[len(inner)-q]:], true
	case st == StepB64 || st == StepB64URL:
		// In the recording's own form, padded or raw. Where text shows
		// neither (inner needs no '='), next must need none either.
		e := b64Encodings(st)
		for i, form := range e {
			if form.EncodeToString([]byte(inner)) == text {
				out := form.EncodeToString([]byte(next))
				return out, e[1-i].EncodeToString([]byte(inner)) != text || out == e[1-i].EncodeToString([]byte(next))
			}
		}
	case st == StepJSON && text == inner:
		// A number: its text is its value.
		var n json.Number
		return next, json.Unmarshal([]byte(next), &n) == nil && n.String() == next
	case st == StepJSON:
		for _, html := range []bool{false, true} {
			if e, ok := jsonString(inner, html); ok && e == text {
				return jsonString(next, html)
			}
		}
	case strings.HasPrefix(string(st), cookiePrefix):
		lo, hi, ok := cookieValue(text, string(st[len(cookiePrefix):]))
		if ok && cookieSafe(next) {
			return text[:lo] + next + text[hi:], true
		}
	}
	return "", false
}

// Decode is text opened by the step st. ok is false where text does not
// decode by it.
func Decode(st Step, text string) (string, bool) {
	switch {
	case st == StepPct:
		at := pctOffsets(text)
		if at == nil {
			return "", false
		}
		b := make([]byte, 0, len(at)-1)
		for i := 0; i+1 < len(at); i++ {
			if text[at[i]] == '%' {
				c, _ := strconv.ParseUint(text[at[i]+1:at[i]+3], 16, 8)
				b = append(b, byte(c))
			} else {
				b = append(b, text[at[i]])
			}
		}
		return string(b), true
	case st == StepJSON:
		d := json.NewDecoder(strings.NewReader(text))
		d.UseNumber()
		var x, more interface{}
		if d.Decode(&x) != nil || d.Decode(&more) != io.EOF {
			return "", false // not one JSON value alone
		}
		switch x := x.(type) {
		case string:
			return x, true
		case json.Number:
			return x.String(), true
		}
	case st == StepB64 || st == StepB64URL:
		for _, e := range b64Encodings(st) {
			if b, err := e.DecodeString(text); err == nil && text != "" {
				return string(b), true
			}
		}
	case strings.HasPrefix(string(st), jwtPrefix):
		parts := strings.Split(text, ".")
		if len(parts) != 3 {
			return "", false
		}
		payload, ok := Decode(StepB64URL, parts[1])
		var claims map[string]json.RawMessage
		if !ok || json.Unmarshal([]byte(payload), &claims) != nil {
			return "", false
		}
		if raw, ok := claims[string(st[len(jwtPrefix):])]; ok {
			return Decode(StepJSON, string(raw))
		}
	case strings.HasPrefix(string(st), cookiePrefix):
		if lo, hi, ok := cookieValue(text, string(st[len(cookiePrefix):])); ok {
			return text[lo:hi], true
		}
	}
	return "", false
}

// b64Encodings are the encodings a b64 or b64url text may be written in:
// padded, then not.
func b64Encodings(st Step) []*base64.Encoding {
	if st == StepB64URL {
		return []*base64.Encoding{base64.URLEncoding, base64.RawURLEncoding}
	}
	return []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding}
}

func jsonString(s string, escapeHTML bool) (string, bool) {
	var b bytes.Buffer
	e := json.NewEncoder(&b)
	e.SetEscapeHTML(escapeHTML)
	if e.Encode(s) != nil {
		return "", false
	}
	return strings.TrimSuffix(b.String(), "\n"), true
}

// pctOffsets is, for each byte text percent-decodes to, where it starts in
// text, and len(text) last; nil where a '%' is not followed by two hex
// digits.
func pctOffsets(text string) []int {
	at := make([]int, 0, len(text)+1)
	for i := 0; i < len(text); i++ {
		at = append(at, i)
		if text[i] == '%' {
			if i+2 >= len(text) || !isHex(text[i+1]) || !isHex(text[i+2]) {
				return nil
			}
			i += 2
		}
	}
	return append(at, len(text))
}

func isHex(c byte) bool {
	return c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F'
}

func pctEncode(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if c := s[i]; isWordByte(c) || strings.IndexByte("-._~", c) >= 0 {
			b.WriteByte(c)
		} else {
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}

// cookieValue is where the value of the one pair named name stands in a
// Cookie or Set-Cookie value ("a=1; sid=x"); ok is false where no pair or
// more than one is named so.
func cookieValue(text, name string) (lo, hi int, ok bool) {
	for start := 0; start <= len(text); {
		end := strings.IndexByte(text[start:], ';')
		if end < 0 {
			end = len(text)
		} else {
			end += start
		}
		pair := text[start:end]
		eq := strings.IndexByte(pair, '=')
		if eq >= 0 && strings.TrimSpace(pair[:eq]) == name {
			if ok {
				return 0, 0, false
			}
			v := strings.TrimRight(pair[eq+1:], " \t")
			lo, hi, ok = start+eq+1, start+eq+1+len(v), true
		}
		start = end + 1
	}
	return lo, hi, ok
}

// cookieSafe reports whether s can stand as a cookie value: none of ';',
// ',', a space, '"', '\' or a control byte.
func cookieSafe(s string) bool {
	for i := 0; i < len(s); i++ {
		if c := s[i]; c <= ' ' || c == 0x7f || strings.IndexByte(`;,"\`, c) >= 0 {
			return false
		}
	}
	return true
}
