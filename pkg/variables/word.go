package variables

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"slices"
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
//	header.Authorization|jwtsig     the signature of a JWT, whole
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
//	jwtsig         the signature of a JWT: its third part, base64url text,
//	               read whole (its '-' and '_' are letters of the
//	               alphabet there, not separators); a write there must
//	               keep its form, base64url of the length it replaces
//	cookie.<name>  the value of the one pair named <name> in a Cookie or
//	               Set-Cookie value
//
// A claim or cookie name escapes '|', '#' and '\' as a query or header name
// does (Parse).

// Step is one decode step of a word address: "pct", "json", "b64", "b64url",
// "jwtsig", "jwt.<claim>" or "cookie.<name>", its name unescaped.
type Step string

// The steps with no argument, and the prefixes of the two with one.
const (
	StepPct    Step = "pct"
	StepJSON   Step = "json"
	StepB64    Step = "b64"
	StepB64URL Step = "b64url"
	// StepJWTSig opens a JWT's signature. A claim's name follows "jwt.",
	// so no claim step is ever written so.
	StepJWTSig   Step = "jwtsig"
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
// reader finds no value there. A place with no '#' and no '|' reads one way
// only, and holds is not asked.
func ReadPlace(s string, holds func(Place) bool) (Address, error) {
	if !strings.ContainsAny(s, "#|") {
		p, err := Parse(s)
		if err != nil {
			return Address{}, err
		}
		return Address{Place: p, Word: -1}, nil
	}
	if p, err := Parse(s); err == nil && holds(p) {
		return Address{Place: p, Word: -1}, nil
	}
	return ParseAddress(s)
}

// Whole reports whether a names its place's value whole: no step, no word.
func (a Address) Whole() bool { return a.Word < 0 && len(a.Steps) == 0 }

// Within is the address of the text that places a's word: the value of the
// cookie a's first step reads by name, which no byte of the place outside
// that value moves; else a's place whole. A replay holds that text to the
// recording, a's word put back, whatever the noise over the place: noise
// is not applied there, and applies to the rest of the place only. A JWT
// claim is read by name too, but its text is the place whole: keploy
// cannot write a claim back in a request, so a request place that reads
// one is refused before it is compared.
func (a Address) Within() Address {
	if len(a.Steps) > 0 && strings.HasPrefix(string(a.Steps[0]), cookiePrefix) {
		return Address{Place: a.Place, Steps: a.Steps[:1:1], Word: -1}
	}
	return Address{Place: a.Place, Word: -1}
}

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
			return Address{}, fmt.Errorf("not a word address: %q: step %q (want pct, json, b64, b64url, jwtsig, jwt.<claim> or cookie.<name>)", s, rest[1:n])
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
	case st == StepPct || st == StepJSON || st == StepB64 || st == StepB64URL || st == StepJWTSig:
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

// Overlaps reports whether a word written at a or at b, two addresses of
// one value leaf, would change what the other names. Their steps in
// common are decoded first, so two words of one decoded text are two
// places. Under a step that re-encodes its text (b64, b64url, a JSON
// string, a JWT) a word stands in the whole text it decodes from; pct and
// cookie steps keep the bytes around it. Two words with no byte in common
// still overlap where one would rewrite what the other's steps read:
// under a pct step, a word holding a byte of an escape (written, the
// escape decodes to something else, or a decoded word's escapes become
// letters); under a cookie step, a word in a cookie's name, or a cookie
// read under another step. A write that may change how many words its
// span holds (a value whole under a step, as a cookie's value, or one a
// step re-encodes) moves every word after its start: it overlaps each of
// them but one read through another cookie, by name. An empty text (a
// cookie's empty value, whole) stands at its offset: it overlaps what
// holds or touches that offset. Where leaf does not hold both, it reports
// true: the two cannot be told apart.
func (a Address) Overlaps(b Address, leaf string) bool {
	sa, sb := a.Steps, b.Steps
	for len(sa) > 0 && len(sb) > 0 && sa[0] == sb[0] {
		var ok bool
		if leaf, ok = Decode(sa[0], leaf); !ok {
			return true
		}
		sa, sb = sa[1:], sb[1:]
	}
	alo, ahi, aok := wordSpan(leaf, sa, a.Word)
	blo, bhi, bok := wordSpan(leaf, sb, b.Word)
	if !aok || !bok {
		return true
	}
	if (slices.Contains(sa, StepPct) || slices.Contains(sb, StepPct)) && (holdsEscape(leaf, alo, ahi) || holdsEscape(leaf, blo, bhi)) {
		return true
	}
	if !keepsCookies(leaf, sa, blo, bhi) || !keepsCookies(leaf, sb, alo, ahi) {
		return true
	}
	if moves(sa, a.Word, sb) && bhi > alo || moves(sb, b.Word, sa) && ahi > blo {
		return true
	}
	if alo == ahi || blo == bhi {
		return max(alo, blo) <= min(ahi, bhi)
	}
	return max(alo, blo) < min(ahi, bhi)
}

// moves reports whether a write at the word n of a text decoded by steps
// may change how many words the text holds, so that the words after it
// move, as the other address, read through other, names them: a value
// whole under a step (a cookie's value may be written with any number of
// words), or a word under a step that re-encodes its text. A pct or cookie
// step writes a word for a word. A word read through another cookie is
// read by its name, which no write in this cookie's value moves.
func moves(steps []Step, n int, other []Step) bool {
	if len(other) > 0 && strings.HasPrefix(string(other[0]), cookiePrefix) {
		return false
	}
	return len(steps) > 0 && (n < 0 || slices.ContainsFunc(steps, reencodes))
}

// reencodes reports whether st writes its text anew (b64, b64url, a JSON
// string or number, a JWT claim), not a word for a word.
func reencodes(st Step) bool {
	return st != StepPct && !strings.HasPrefix(string(st), cookiePrefix)
}

// holdsEscape reports whether [lo, hi) of text holds a byte of a "%XX"
// escape.
func holdsEscape(text string, lo, hi int) bool {
	return strings.IndexByte(text[max(lo-2, 0):hi], '%') >= 0
}

// keepsCookies reports whether a word written at [lo, hi) of text keeps
// the cookies steps read: a word written in a cookie's name renames it,
// and a cookie read under another step is not followed. A word for a word
// anywhere else keeps every name.
func keepsCookies(text string, steps []Step, lo, hi int) bool {
	for i, st := range steps {
		if strings.HasPrefix(string(st), cookiePrefix) && (i > 0 || inCookieName(text, lo, hi)) {
			return false
		}
	}
	return true
}

// inCookieName reports whether [lo, hi) of text holds a byte of a cookie's
// name, the pairs read as cookieValue reads them.
func inCookieName(text string, lo, hi int) bool {
	for start := 0; start <= len(text); {
		end := strings.IndexByte(text[start:], ';')
		if end < 0 {
			end = len(text)
		} else {
			end += start
		}
		if name, _, found := strings.Cut(text[start:end], "="); found && lo < start+len(name) && start < hi {
			return true
		}
		start = end + 1
	}
	return false
}

// wordSpan is [lo, hi), the bytes of text that the word n of text decoded
// by steps is written in (WordOf).
func wordSpan(text string, steps []Step, n int) (lo, hi int, ok bool) {
	if len(steps) == 0 {
		if n < 0 {
			return 0, len(text), true
		}
		ws := Words(text)
		if n >= len(ws) {
			return 0, 0, false
		}
		return ws[n][0], ws[n][1], true
	}
	inner, ok := Decode(steps[0], text)
	if !ok {
		return 0, 0, false
	}
	if lo, hi, ok = wordSpan(inner, steps[1:], n); !ok {
		return 0, 0, false
	}
	switch st := steps[0]; {
	case st == StepPct:
		at := pctOffsets(text)
		return at[lo], at[hi], true
	case st == StepJSON && text == inner:
		return lo, hi, true // a number: its text is its value
	case strings.HasPrefix(string(st), cookiePrefix):
		at, _, _ := cookieValue(text, string(st[len(cookiePrefix):]))
		return at + lo, at + hi, true
	case st == StepJWTSig:
		// Written whole, in the token's last part: the bytes before it stay.
		at, _ := jwtSignature(text)
		return at + lo, at + hi, true
	}
	return 0, len(text), true
}

// ReplaceWord is leaf with the word a names set to v, every step encoded
// back the way leaf was, every other byte kept where a step allows (pct
// and cookie keep them; b64 and json re-encode the text, where leaf is
// their own encoding of it). base64 keeps leaf's padding, padded or raw;
// where leaf does not show which (its text needs no '=') and the padded
// and raw forms of the new text differ, it refuses. ok is false where it
// cannot: a step leaf does not decode by, a JWT claim (its signature would
// break), a JWT signature of another form than the one it replaces
// (SignatureForm), a word a does not find, a re-encoding that is not
// leaf's own, a padding leaf does not show, a cookie value v cannot be, or
// a v that a reads back otherwise (one that joins the words beside it).
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

// WordPut is a value written at a word of one text: the text holds From
// at At, and To takes its place.
type WordPut struct {
	At       Address
	From, To string
}

// PutWords is text with each put's To written at its address, in turn, and
// fails closed where a write would move another put's word. text must hold
// each put's From at its address. After each write, every put written
// before still reads its To and every put after still reads its From, each
// at its own address; and any two puts can each be written first on text,
// the other then written after it, and give the same text in both orders,
// so a word the write moved that happens to read the same is refused too,
// and so is a put that can be written only after another. Where it
// refuses, i is the put that cannot be written or whose write moves
// another, and j is the put it moves, or -1.
func PutWords(text string, puts []WordPut) (out string, i, j int, ok bool) {
	for j, p := range puts {
		if got, ok := p.At.WordOf(text); !ok || got != p.From {
			return "", j, -1, false
		}
	}
	out = text
	for i, p := range puts {
		next, ok := p.At.ReplaceWord(out, p.To)
		if !ok {
			return "", i, -1, false
		}
		for j, q := range puts {
			want := q.From
			if j < i {
				want = q.To
			}
			if got, ok := q.At.WordOf(next); j != i && (!ok || got != want) {
				return "", i, j, false
			}
		}
		out = next
	}
	for i := range puts {
		for j := i + 1; j < len(puts); j++ {
			ij, ok1 := writeBoth(text, puts[i], puts[j])
			ji, ok2 := writeBoth(text, puts[j], puts[i])
			if !ok1 || !ok2 || ij != ji {
				return "", i, j, false
			}
		}
	}
	return out, -1, -1, true
}

// writeBoth is text with p written, then q.
func writeBoth(text string, p, q WordPut) (string, bool) {
	out, ok := p.At.ReplaceWord(text, p.To)
	if !ok {
		return "", false
	}
	return q.At.ReplaceWord(out, q.To)
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
	case st == StepJWTSig:
		// A signature is checked by its form only: the one written must be
		// of the form of the one it replaces (SignatureForm), so the
		// token's header, which names the algorithm, holds for it.
		at, ok := jwtSignature(text)
		if ok && SignatureForm(inner, next) {
			return text[:at] + next, true
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
	case st == StepJWTSig:
		if at, ok := jwtSignature(text); ok {
			return text[at:], true
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

// jwtSignature is where text's signature starts, text a JWT (three '.'
// parts, the second a base64url JSON object, as the jwt.<claim> step reads
// one) that is signed (its third part base64url text, non-empty and
// unpadded, as RFC 7515 writes it); ok is false otherwise.
func jwtSignature(text string) (at int, ok bool) {
	parts := strings.Split(text, ".")
	if len(parts) != 3 || !b64url(parts[2]) {
		return 0, false
	}
	payload, ok := Decode(StepB64URL, parts[1])
	var claims map[string]json.RawMessage
	if !ok || json.Unmarshal([]byte(payload), &claims) != nil || claims == nil {
		return 0, false
	}
	return len(text) - len(parts[2]), true
}

// SignatureForm reports whether a and b, two JWT signatures, are of one
// form: base64url text, unpadded, of one length. The signature is all a
// replay checks of it (the founder's 2026-10-31 ruling): it is the
// signer's random-looking output, and its length is the algorithm's, which
// the token's header names.
func SignatureForm(a, b string) bool {
	return len(a) == len(b) && b64url(a) && b64url(b)
}

// b64url reports whether s is non-empty unpadded base64url text, as a JWT
// writes each part.
func b64url(s string) bool {
	if s == "" {
		return false
	}
	_, err := base64.RawURLEncoding.Strict().DecodeString(s)
	return err == nil
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
//
// There are at most len(text)+1 offsets, so their slice is sized to that up
// front. No string is math.MaxInt bytes long, so the sum cannot wrap; the
// check before it says so where CodeQL's allocation-size check can see it.
func pctOffsets(text string) []int {
	if len(text) >= math.MaxInt {
		return nil // not reached
	}
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
