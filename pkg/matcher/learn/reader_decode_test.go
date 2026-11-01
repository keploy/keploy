package learn

import (
	"encoding/base64"
	"slices"
	"strings"
	"testing"

	"go.keploy.io/server/v3/pkg/variables"
)

// leafChanges are a leaf reading's changed words, "<address>: rec>live"
// each, in order.
func leafChanges(rds []textReading) []string {
	var out []string
	for _, rd := range rds {
		for _, w := range rd.Words {
			out = append(out, w.At.String()+": "+w.Rec+">"+w.Live)
		}
	}
	return out
}

// readsBack checks that each changed word's address names it in the leaf:
// R's word by At, F's by At at F's index, both through the steps.
func readsBack(t *testing.T, rec, live string, rds []textReading) {
	t.Helper()
	for _, rd := range rds {
		for _, w := range rd.Words {
			if w.RecIdx >= 0 {
				if got, ok := w.At.WordOf(rec); !ok || got != w.Rec {
					t.Errorf("%s in R reads %q (%v); the word is %q", w.At, got, ok, w.Rec)
				}
			}
			if w.LiveIdx >= 0 {
				la := w.At
				la.Word = rd.Off[1] + w.LiveIdx
				if got, ok := la.WordOf(live); !ok || got != w.Live {
					t.Errorf("%s in F reads %q (%v); the word is %q", la, got, ok, w.Live)
				}
			}
		}
	}
}

// mustAt is the address s names, the leaf whole (Word -1).
func mustAt(t *testing.T, s string) variables.Address {
	t.Helper()
	a, err := variables.ParseAddress(s)
	if err != nil {
		t.Fatalf("address %q: %v", s, err)
	}
	a.Word = -1
	return a
}

// A step opens a text only where both runs are so encoded, both decode,
// and the opened texts still differ: percent escapes, a JSON string
// literal, base64 and base64url that decode to text, nested. Where one run
// does not decode (a base64 of text in R, of binary in F; a broken escape;
// escapes in R alone), or both open to one text (only the escaping
// changed), the text is read as it is.
func TestReaderDecodeStepsOpenWhereBothRunsDecode(t *testing.T) {
	b64 := func(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }
	b64u := func(s string) string { return base64.RawURLEncoding.EncodeToString([]byte(s)) }
	if enc := b64u("id%3D17&r=eu~~~"); !strings.ContainsAny(enc, "-_") {
		t.Fatalf("fixture: %q has no URL-only byte", enc)
	}
	binary := base64.StdEncoding.EncodeToString([]byte{0xff, 0xfe, 0x00, 0x01, 0x80, 0x9f, 0xc3, 0x28, 0x07, 0x1b})
	for _, c := range []struct {
		name, place, rec, live string
		want                   []string
	}{
		{"percent", "body.next", "uploads%2F17%2Fa.png", "uploads%2F18%2Fa.png",
			[]string{"body.next|pct#1: 17>18"}},
		{"percent in R only", "body.next", "a%2F17", "a/18",
			[]string{"body.next#1: 2F17>18"}},
		{"a broken escape in F", "body.next", "a%2F17", "a%zz18",
			[]string{"body.next#1: 2F17>zz18"}},
		{"only the escaping changed", "body.next", "a%2Fb", "a/b",
			[]string{"body.next#1: 2Fb>b"}},
		{"a JSON string literal", "body.v", `"order-17"`, `"order-18"`,
			[]string{"body.v|json#1: 17>18"}},
		{"base64 in both", "body.v", b64("user 17 eu"), b64("user 18 eu"),
			[]string{"body.v|b64#1: 17>18"}},
		{"base64 of text in R, of binary in F", "body.v", b64("user 17 eu"), binary,
			[]string{"body.v#0: dXNlciAxNyBldQ>" + strings.Split(binary, "/")[0]}},
		{"base64url holding escapes", "body.cursor", b64u("id%3D17&r=eu~~~"), b64u("id%3D18&r=eu~~~"),
			[]string{"body.cursor|b64url|pct#1: 17>18"}},
	} {
		place := mustAt(t, c.place)
		rds := readLeaf(nil, place, c.rec, c.live)
		got := leafChanges(rds)
		if c.name == "base64 of text in R, of binary in F" {
			// F's words are binary's; only R's first word and the absence of a step matter.
			if len(got) == 0 || !strings.HasPrefix(got[0], "body.v#0: dXNlciAxNyBldQ>") {
				t.Errorf("%s: %q; want the text read as it is, no step", c.name, got)
			}
			for _, rd := range rds {
				if len(rd.At.Steps) > 0 {
					t.Errorf("%s: a step %v taken where F does not decode to text", c.name, rd.At.Steps)
				}
			}
		} else if !sameStrings(got, c.want) {
			t.Errorf("%s: %q -> %q: %q; want %q", c.name, c.rec, c.live, got, c.want)
		}
		readsBack(t, c.rec, c.live, rds)
	}
}

// jwtOf is a JWT: "Bearer " (where bearer), the header, the payload and
// the signature.
func jwtOf(bearer bool, payload, sig string) string {
	s := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"HS256","typ":"JWT"}`)) + "." +
		base64.RawURLEncoding.EncodeToString([]byte(payload)) + "." + sig
	if bearer {
		return "Bearer " + s
	}
	return s
}

// A JWT opens claim by claim where every claim that differs is a string
// or a number in both runs: an iat is an epoch instant at its claim's
// address, a jti's changed word stands at its own. The signature is read
// apart from the payload, so its words stand at their index in the whole
// value and each lists the whole signature token, whatever the payload
// did. A claim that is an array in both runs keeps the JWT unopened.
func TestReaderJWTClaimsOpen(t *testing.T) {
	place := mustAt(t, "header.Authorization")
	rs, ls := "Sfl-KxwRJ_SMeKKF2QT4fwpMeJf36POk6yJV_adQssw", "Q1x_Yp-9vTq3LmN0Rr4sWkHd2cJ7aBfE5gUoZ_iXyPq"
	rec := jwtOf(true, `{"sub":"u-1","iat":1761955200,"jti":"a-17"}`, rs)
	live := jwtOf(true, `{"sub":"u-1","iat":1761955260,"jti":"a-18"}`, ls)
	rds := readLeaf(nil, place, rec, live)
	readsBack(t, rec, live, rds)
	got := leafChanges(rds)
	want := []string{
		"header.Authorization|jwt.iat#0: 1761955200>1761955260",
		"header.Authorization|jwt.jti#1: 17>18",
	}
	if len(got) < 2 || !sameStrings(got[:2], want) {
		t.Fatalf("claims: %q; want %q first", got, want)
	}
	if rds[0].Words[0].Instant == nil {
		t.Errorf("iat's epoch: no instant")
	}
	sigAt := len(variables.Words(rec[:strings.LastIndexByte(rec, '.')]))
	sig := rds[len(rds)-1]
	if len(sig.At.Steps) != 0 || sig.Off[0] != sigAt || len(sig.Words) == 0 {
		t.Fatalf("signature reading: at %s off %v, %d words; want the leaf's own text from word %d", sig.At, sig.Off, len(sig.Words), sigAt)
	}
	for _, rd := range rds[2:] {
		if rd.Off != sig.Off || len(rd.At.Steps) != 0 {
			t.Errorf("a reading besides the claims and the signature: at %s off %v %q", rd.At, rd.Off, changedPairs(rd))
		}
	}
	for _, w := range sig.Words {
		if w.At.Word < sigAt {
			t.Errorf("signature word %q at %s: before the signature", w.Rec, w.At)
		}
		if len(w.Tokens) == 0 || w.Tokens[0].Kind != tokenID || w.Tokens[0].Rec != rs || w.Tokens[0].Live != ls {
			t.Errorf("signature word %q>%q: tokens %+v; want the whole signature", w.Rec, w.Live, w.Tokens)
		}
	}

	// Only the signature changed: one reading, the signature's.
	same := jwtOf(false, `{"sub":"u-1"}`, rs)
	other := jwtOf(false, `{"sub":"u-1"}`, ls)
	if rds := readLeaf(nil, place, same, other); len(rds) != 1 || rds[0].Off[0] == 0 {
		t.Errorf("only the signature changed: %d readings %q; want one, the signature's", len(rds), leafChanges(rds))
	}

	// The payload re-encoded with no claim changed: no step.
	ro := jwtOf(false, `{"sub":"u-1","n":"a-17"}`, rs)
	lo := jwtOf(false, `{"n":"a-17","sub":"u-1"}`, rs)
	for _, rd := range readLeaf(nil, place, ro, lo) {
		if len(rd.At.Steps) != 0 || rd.Off != [2]int{} {
			t.Errorf("a payload re-encoded: opened at %s off %v", rd.At, rd.Off)
		}
	}

	// A claim that differs as an array is no string or number: no step.
	ra := jwtOf(false, `{"roles":["a-17"]}`, rs)
	la := jwtOf(false, `{"roles":["a-18"]}`, rs)
	for _, rd := range readLeaf(nil, place, ra, la) {
		if len(rd.At.Steps) != 0 || rd.Off != [2]int{} {
			t.Errorf("an array claim: opened at %s off %v", rd.At, rd.Off)
		}
	}
}

// In a Cookie or Set-Cookie header, each pair named once in both runs
// whose value differs opens by its name, so its words keep their index
// whatever pairs stand before it. Pairs whose names stand in another order,
// a name twice, or the same text at another place are read as they are.
func TestReaderCookiePairsOpen(t *testing.T) {
	for _, c := range []struct {
		name, place, rec, live string
		want                   []string
	}{
		{"two values", "header.Cookie", "theme=dark; sid=abc-17; lang=en", "theme=dark; sid=abc-18; lang=fr",
			[]string{"header.Cookie|cookie.sid#1: 17>18", "header.Cookie|cookie.lang#0: en>fr"}},
		{"a pair before it added", "header.Cookie", "sid=abc-17", "consent=yes; sid=abc-18",
			[]string{"header.Cookie>consent", "header.Cookie>yes", "header.Cookie|cookie.sid#1: 17>18"}},
		{"a value's escapes", "header.Cookie", "lang=en; next=a%2F17", "lang=en; next=a%2F18",
			[]string{"header.Cookie|cookie.next|pct#1: 17>18"}},
		{"set-cookie", "header.Set-Cookie", "sid=abc-17; Path=/; HttpOnly", "sid=abc-18; Path=/; HttpOnly",
			[]string{"header.Set-Cookie|cookie.sid#1: 17>18"}},
		{"another order", "header.Cookie", "sid=abc-17; lang=en", "lang=fr; sid=abc-18", nil},
		{"a name twice", "header.Cookie", "sid=abc-17; sid=x", "sid=abc-18; sid=x", nil},
		{"not a cookie header", "body.c", "theme=dark; sid=abc-17", "theme=dark; sid=abc-18", nil},
	} {
		rds := readLeaf(nil, mustAt(t, c.place), c.rec, c.live)
		readsBack(t, c.rec, c.live, rds)
		got := leafChanges(rds)
		if c.want == nil {
			for _, rd := range rds {
				if len(rd.At.Steps) != 0 {
					t.Errorf("%s: opened %s; want the text as it is", c.name, rd.At)
				}
			}
			if len(got) == 0 {
				t.Errorf("%s: no changed word", c.name)
			}
			continue
		}
		// A word F alone has prints its address with no word: "<place>: >w".
		for i := range got {
			got[i] = strings.Replace(got[i], ": >", ">", 1)
		}
		if !sameStrings(got, c.want) {
			t.Errorf("%s: %q; want %q", c.name, got, c.want)
		}
	}
}

// The adapter's address opens first: each of its steps where both runs
// decode by it to texts that still differ, so a base64 too short for main's
// guess opens where the adapter says it is base64, and a text it opens is
// read on from there (escapes inside it open too). At the first step a run
// does not decode by, or that opens both runs to one text, the text is
// read from the steps before it: no reading takes that step.
func TestReaderOpensFromTheAdaptersAddress(t *testing.T) {
	b64 := func(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }
	if len(b64("17")) >= minEncodedRun {
		t.Fatalf("fixture: %q is long enough for main's guess", b64("17"))
	}
	for _, c := range []struct {
		name, at, rec, live string
		want                []string
		noStep              variables.Step
	}{
		{"a short base64 the adapter names", "body.n|b64", b64("17"), b64("42"),
			[]string{"body.n|b64#0: 17>42"}, ""},
		{"escapes inside the adapter's base64", "body.n|b64", b64("a%2F17"), b64("a%2F18"),
			[]string{"body.n|b64|pct#1: 17>18"}, ""},
		{"two steps, both open", "body.n|b64|json", b64(`"order-17"`), b64(`"order-18"`),
			[]string{"body.n|b64|json#1: 17>18"}, ""},
		{"F does not decode by the step", "body.n|b64", b64("17"), "x-42", nil, variables.StepB64},
		{"the second step opens R only", "body.n|b64|json", b64(`"order-17"`), b64("order-18"), nil, variables.StepJSON},
		{"only the padding changed", "body.n|b64", b64("order-17"), strings.TrimRight(b64("order-17"), "="), nil, variables.StepB64},
	} {
		rds := readLeaf(nil, mustAt(t, c.at), c.rec, c.live)
		readsBack(t, c.rec, c.live, rds)
		if c.want != nil {
			if got := leafChanges(rds); !sameStrings(got, c.want) {
				t.Errorf("%s: %q; want %q", c.name, got, c.want)
			}
			continue
		}
		for _, rd := range rds {
			if slices.Contains(rd.At.Steps, c.noStep) {
				t.Errorf("%s: read at %s; the step %s does not open both runs", c.name, rd.At, c.noStep)
			}
		}
	}
}
