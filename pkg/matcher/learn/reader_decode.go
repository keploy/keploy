package learn

import (
	"bytes"
	"encoding/json"
	"sort"
	"strings"

	"go.keploy.io/server/v3/pkg/variables"
)

// This file opens a drifted leaf's encoded parts (DESIGN §1). A changed
// word's address is the leaf's place, the decode steps that open the text
// it stands in, and its index among that text's words (variables.Address).
// A step is taken only where it decodes in both runs and the opened texts
// still differ; otherwise the text is read as it is, its words judged one
// by one, so a change only of escaping is read as the change it is.
//
//   - pct, json, b64 and b64url open the text whole, only where each run
//     is so encoded: pct where both hold a %XX escape (a run with none is
//     not encoded, and replay could not give it the other's escapes); json
//     where both are a JSON string literal;
//     b64 and b64url where both runs are of minEncodedRun bytes or more,
//     not all digits, and decode to text (main's rule, readings.go: binary,
//     a signature or a hash, is no text, and nothing is read in it). The
//     standard alphabet is tried first; both runs open by the one step.
//   - jwt.<claim> opens a JWT's claims where both runs are a JWT (three
//     '.' parts, the second a base64url JSON object) and every claim that
//     differs is a string or a number in both runs. The text before the
//     payload ("Bearer " and the header) and the signature are read as
//     they are, each apart, so the signature's words stand together and
//     each lists the whole signature token, whatever the payload did.
//   - cookie.<name> opens, in a Cookie or Set-Cookie header, each pair
//     named once in both runs whose value differs, where those names stand
//     in one order in both; the text between them is read as it is. It is
//     tried first there, so a pair's escapes open inside it.
//
// Steps nest up to maxDecodeLevels (a base64url cursor holding percent
// escapes opens as |b64url|pct).

// wholeSteps are the steps that open a text whole, in the order tried.
var wholeSteps = []variables.Step{variables.StepPct, variables.StepJSON, variables.StepB64, variables.StepB64URL}

// openedStep is one text a step opens in both runs.
type openedStep struct {
	Step      variables.Step
	Rec, Live string
}

// openedSpan is where opened texts stand in each run, [start, end) in
// bytes, and the texts opened there: a JWT's payload opens one text per
// claim that differs (none where only its encoding did), a cookie's value
// one.
type openedSpan struct {
	RecAt, LiveAt [2]int
	Parts         []openedStep
}

// readLeaf reads a drifted leaf at place: each text it reads, the leaf's
// own and each a step opens, in the leaf's order, read word by word
// (readTextIn, instants through dc's cache). A reading's At names its text
// whole (Word -1); a changed word's At names it by its index among R's
// words of that text. A word R has no partner for keeps Word -1: it stands
// in a stretch that binds whole or not at all.
func readLeaf(dc *DriftCache, place variables.Place, rec, live string) []textReading {
	var out []textReading
	openText(dc, &out, variables.Address{Place: place, Word: -1}, rec, live, 0)
	return out
}

// openText reads (rec, live) at at: opened by the first step that opens it
// in both runs, else as it is.
func openText(dc *DriftCache, out *[]textReading, at variables.Address, rec, live string, depth int) {
	if rec == live {
		return
	}
	if depth < maxDecodeLevels {
		if cookiePlace(at) {
			if spans, ok := cookieSpans(rec, live); ok {
				readSpans(dc, out, at, rec, live, spans, depth)
				return
			}
		}
		for _, st := range wholeSteps {
			if r, l, ok := openWhole(st, rec, live); ok {
				openText(dc, out, withStep(at, st), r, l, depth+1)
				return
			}
		}
		if span, ok := jwtSpan(rec, live); ok {
			readSpans(dc, out, at, rec, live, []openedSpan{span}, depth)
			return
		}
	}
	readRaw(dc, out, at, rec, live, [2]int{})
}

// withStep is at with st after its steps, naming the opened text whole.
func withStep(at variables.Address, st variables.Step) variables.Address {
	steps := make([]variables.Step, 0, len(at.Steps)+1)
	return variables.Address{Place: at.Place, Steps: append(append(steps, at.Steps...), st), Word: -1}
}

// openWhole is (rec, live) opened by the whole-text step st, ok where it
// opens both runs (see the file's comment) to texts that still differ.
func openWhole(st variables.Step, rec, live string) (string, string, bool) {
	r, rok := variables.Decode(st, rec)
	l, lok := variables.Decode(st, live)
	if !rok || !lok || r == l || r == rec || l == live {
		return "", "", false
	}
	if st == variables.StepB64 || st == variables.StepB64URL {
		for _, c := range [][2]string{{rec, r}, {live, l}} {
			if len(c[0]) < minEncodedRun || allDigits(c[0]) || !isText([]byte(c[1])) {
				return "", "", false
			}
		}
	}
	return r, l, true
}

func allDigits(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// readSpans reads (rec, live) opened at spans: the text between them as it
// is, each part apart and the spans' texts each opened by its step.
func readSpans(dc *DriftCache, out *[]textReading, at variables.Address, rec, live string, spans []openedSpan, depth int) {
	pr, pl := 0, 0
	raw := func(r, l int) {
		off := [2]int{len(variables.Words(rec[:pr])), len(variables.Words(live[:pl]))}
		readRaw(dc, out, at, rec[pr:r], live[pl:l], off)
	}
	for _, sp := range spans {
		raw(sp.RecAt[0], sp.LiveAt[0])
		for _, p := range sp.Parts {
			openText(dc, out, withStep(at, p.Step), p.Rec, p.Live, depth+1)
		}
		pr, pl = sp.RecAt[1], sp.LiveAt[1]
	}
	raw(len(rec), len(live))
}

// readRaw reads (rec, live) as it is, a part of the text at at that off[0]
// words of R's and off[1] of F's come before, and gives its changed words
// their addresses.
func readRaw(dc *DriftCache, out *[]textReading, at variables.Address, rec, live string, off [2]int) {
	if rec == live {
		return
	}
	rd := readTextIn(dc, rec, live)
	rd.At, rd.Off = at, off
	for i := range rd.Words {
		w := &rd.Words[i]
		w.At = at
		if w.RecIdx >= 0 {
			w.At.Word = off[0] + w.RecIdx
		}
	}
	*out = append(*out, rd)
}

// jwtSpan is where the payloads of two JWTs stand and the claims opened
// there: each claim that differs, by name. ok is false where either run is
// no JWT, a claim that differs is missing from a run or is no string or
// number in one, or the payloads differ with no claim that does.
func jwtSpan(rec, live string) (openedSpan, bool) {
	ra, rc, rok := jwtClaims(rec)
	la, lc, lok := jwtClaims(live)
	if !rok || !lok {
		return openedSpan{}, false
	}
	names := make([]string, 0, len(rc)+len(lc))
	for n := range rc {
		names = append(names, n)
	}
	for n := range lc {
		if _, ok := rc[n]; !ok {
			names = append(names, n)
		}
	}
	sort.Strings(names)
	sp := openedSpan{RecAt: ra, LiveAt: la}
	for _, n := range names {
		rv, rin := rc[n]
		lv, lin := lc[n]
		if rin && lin && sameJSON(rv, lv) {
			continue
		}
		if !rin || !lin {
			return openedSpan{}, false
		}
		r, rok := variables.Decode(variables.StepJSON, string(rv))
		l, lok := variables.Decode(variables.StepJSON, string(lv))
		if !rok || !lok {
			return openedSpan{}, false
		}
		if r != l {
			sp.Parts = append(sp.Parts, openedStep{Step: variables.JWTClaim(n), Rec: r, Live: l})
		}
	}
	if len(sp.Parts) == 0 && rec[ra[0]:ra[1]] != live[la[0]:la[1]] {
		return openedSpan{}, false // only the payload's encoding changed
	}
	return sp, true
}

// jwtClaims are where a JWT's payload stands in text and its claims; ok is
// false where text is no JWT (variables.Decode's jwt rule: three '.' parts,
// the second a base64url JSON object).
func jwtClaims(text string) ([2]int, map[string]json.RawMessage, bool) {
	d1 := strings.IndexByte(text, '.')
	if d1 < 0 {
		return [2]int{}, nil, false
	}
	d2 := strings.IndexByte(text[d1+1:], '.') + d1 + 1
	if d2 <= d1 || strings.IndexByte(text[d2+1:], '.') >= 0 {
		return [2]int{}, nil, false
	}
	payload, ok := variables.Decode(variables.StepB64URL, text[d1+1:d2])
	var claims map[string]json.RawMessage
	if !ok || json.Unmarshal([]byte(payload), &claims) != nil || claims == nil {
		return [2]int{}, nil, false
	}
	return [2]int{d1 + 1, d2}, claims, true
}

// sameJSON says whether two JSON values are one text, but for whitespace.
func sameJSON(a, b json.RawMessage) bool {
	var ca, cb bytes.Buffer
	if json.Compact(&ca, a) != nil || json.Compact(&cb, b) != nil {
		return bytes.Equal(a, b)
	}
	return bytes.Equal(ca.Bytes(), cb.Bytes())
}

// cookiePlace says whether at is a Cookie or Set-Cookie header's own text.
func cookiePlace(at variables.Address) bool {
	return len(at.Steps) == 0 && at.Place.Part == variables.PartHeader &&
		(strings.EqualFold(at.Place.Name, "Cookie") || strings.EqualFold(at.Place.Name, "Set-Cookie"))
}

// cookiePair is one "name=value" pair of a cookie text: its name, and
// where its value stands.
type cookiePair struct {
	Name string
	At   [2]int
}

// cookiePairs are text's pairs as variables' cookie step reads them: split
// at ';', the name trimmed, the value up to its trailing blanks. A part
// with no '=' (Set-Cookie's HttpOnly) is no pair.
func cookiePairs(text string) []cookiePair {
	var out []cookiePair
	for start := 0; start <= len(text); {
		end := strings.IndexByte(text[start:], ';')
		if end < 0 {
			end = len(text)
		} else {
			end += start
		}
		pair := text[start:end]
		if eq := strings.IndexByte(pair, '='); eq >= 0 {
			v := strings.TrimRight(pair[eq+1:], " \t")
			out = append(out, cookiePair{Name: strings.TrimSpace(pair[:eq]), At: [2]int{start + eq + 1, start + eq + 1 + len(v)}})
		}
		start = end + 1
	}
	return out
}

// cookieSpans are the values of the pairs named once in both runs whose
// values differ, in R's order; ok is false where there are none, or they
// stand in F in another order.
func cookieSpans(rec, live string) ([]openedSpan, bool) {
	rp, lp := cookiePairs(rec), cookiePairs(live)
	count := func(ps []cookiePair) map[string]int {
		n := make(map[string]int, len(ps))
		for _, p := range ps {
			n[p.Name]++
		}
		return n
	}
	rn, ln := count(rp), count(lp)
	at := make(map[string][2]int, len(lp))
	for _, p := range lp {
		at[p.Name] = p.At
	}
	var spans []openedSpan
	last := -1
	for _, p := range rp {
		la, ok := at[p.Name]
		if rn[p.Name] != 1 || ln[p.Name] != 1 || !ok {
			continue
		}
		r, l := rec[p.At[0]:p.At[1]], live[la[0]:la[1]]
		if r == l {
			continue
		}
		if la[0] < last {
			return nil, false
		}
		last = la[1]
		st := variables.Cookie(p.Name)
		if dr, ok := variables.Decode(st, rec); !ok || dr != r {
			return nil, false
		}
		if dl, ok := variables.Decode(st, live); !ok || dl != l {
			return nil, false
		}
		spans = append(spans, openedSpan{RecAt: p.At, LiveAt: la, Parts: []openedStep{{Step: st, Rec: r, Live: l}}})
	}
	return spans, len(spans) > 0
}
