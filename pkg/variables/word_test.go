package variables

import (
	"encoding/base64"
	"reflect"
	"testing"

	"go.keploy.io/server/v3/pkg/templates"
)

// TestAWordAddressReadsAsItIsWritten (TY1): every step and a word index
// round-trip through ParseAddress and String, and a plain place reads with
// no step and Word -1, written byte for byte as Parse and Place.String do.
func TestAWordAddressReadsAsItIsWritten(t *testing.T) {
	for _, s := range []string{
		"body.resourceKey[0]#1",
		"header.Authorization|jwt.sub",
		"header.Cookie|cookie.sid#0",
		"url.query.cursor|b64url|pct#2",
		"url.path[3]|pct#0",
		"body.payload|json#0",
		"body.blob|b64",
		`header.X\|Y|cookie.a\#b#3`,
		`body["a|b"]["c#d"]|jwt.e\|f#12`,
	} {
		a, err := ParseAddress(s)
		if err != nil {
			t.Fatalf("ParseAddress(%q): %v", s, err)
		}
		if a.String() != s {
			t.Errorf("ParseAddress(%q).String() = %q", s, a.String())
		}
		if b, err := ParseAddress(a.String()); err != nil || !reflect.DeepEqual(a, b) {
			t.Errorf("%q does not round-trip: %+v then %+v, %v", s, a, b, err)
		}
	}
	a, _ := ParseAddress(`header.X\|Y|cookie.a\#b#3`)
	if a.Place.Name != "X|Y" || !reflect.DeepEqual(a.Steps, []Step{Cookie("a#b")}) || a.Word != 3 {
		t.Errorf("escapes read as %+v", a)
	}

	for _, s := range []string{"body.items[0].id", `body["a.b"].c`, "url.path[2]", "url.query.page", "header.X-Request-Id", `url.query.a\b`} {
		a, err := ParseAddress(s)
		p, perr := Parse(s)
		if err != nil || perr != nil || len(a.Steps) != 0 || a.Word != -1 || !reflect.DeepEqual(a.Place, p) {
			t.Fatalf("plain %q: %+v %v; Parse %+v %v", s, a, err, p, perr)
		}
		if a.String() != s || p.String() != s {
			t.Errorf("plain %q prints %q (Place.String %q)", s, a.String(), p.String())
		}
	}

	// A name or key holding '|', '#' or a trailing '\' is escaped, and
	// reads back as itself through Parse and ParseAddress alike.
	for _, p := range []Place{
		{Part: PartHeader, Name: "X|Y"},
		{Part: PartQuery, Name: `a#1\`},
		{Part: PartQuery, Name: `a\|b\\`},
		BodyPlace([]templates.PathSeg{{Key: "k|v"}, {Key: "n#2"}}),
	} {
		s := p.String()
		q, err := Parse(s)
		a, aerr := ParseAddress(s)
		if err != nil || aerr != nil || !reflect.DeepEqual(q, p) || !reflect.DeepEqual(a, Address{Place: p, Word: -1}) {
			t.Errorf("%+v prints %q, which reads as %+v %v and %+v %v", p, s, q, err, a, aerr)
		}
	}
	// One written before the escape reads as it did.
	if a, err := ParseAddress("header.X|Y"); err != nil || a.Place.Name != "X|Y" || a.Word != -1 {
		t.Errorf("an unescaped name reads as %+v, %v", a, err)
	}

	for _, s := range []string{"url.path[2]|nope", "url.path[2]#x", "url.path[2]#01", "header.", "url.path[2]|jwt.", "body"} {
		if a, err := ParseAddress(s); err == nil {
			t.Errorf("ParseAddress(%q) = %+v, want an error", s, a)
		}
	}
	// A word or a step of no place names nothing, and prints as Place{} does.
	for _, a := range []Address{{Word: -1}, {Word: 1}, {Steps: []Step{StepB64}, Word: 0}} {
		if s := a.String(); s != "" {
			t.Errorf("%+v prints %q, want \"\"", a, s)
		}
	}
}

// TestAWordIsReadAndReplacedThroughItsSteps (TY2): WordOf and ReplaceWord
// through pct, json, b64, b64url and cookie.<name>, every other byte kept;
// a JWT claim reads, and refuses to be replaced.
func TestAWordIsReadAndReplacedThroughItsSteps(t *testing.T) {
	q := Place{Part: PartQuery, Name: "q"}
	b64 := base64.StdEncoding.EncodeToString
	b64url := func(s string) string { return base64.RawURLEncoding.EncodeToString([]byte(s)) }
	jwt := b64url(`{"alg":"HS256"}`) + "." + b64url(`{"sub":"user-17","n":5,"o":{}}`) + ".c2ln"
	for _, c := range []struct {
		name       string
		leaf       string
		steps      []Step
		word       int
		was, value string
		want       string // "" where ReplaceWord refuses
	}{
		{"pct keeps its other bytes as written", "order%2f17%2Dx", []Step{StepPct}, 1, "17", "42", "order%2f42%2Dx"},
		{"pct encodes what it puts", "a%20b", []Step{StepPct}, -1, "a b", "c/d", "c%2Fd"},
		{"json", `"abc-17"`, []Step{StepJSON}, 1, "17", "42", `"abc-42"`},
		{"json number", `17`, []Step{StepJSON}, 0, "17", "42", `42`},
		{"b64", b64([]byte("user:17")), []Step{StepB64}, 1, "17", "42", b64([]byte("user:42"))},
		{"b64url", b64url(`{"id":"o-17"}`), []Step{StepB64URL}, 2, "17", "42", b64url(`{"id":"o-42"}`)},
		{"b64url then json", b64url(`"o-17"`), []Step{StepB64URL, StepJSON}, 1, "17", "42", b64url(`"o-42"`)},
		{"cookie", "a=1; sid=abc-17; b=2", []Step{Cookie("sid")}, 1, "17", "42", "a=1; sid=abc-42; b=2"},
		{"set-cookie", "sid=abc-17; Path=/", []Step{Cookie("sid")}, -1, "abc-17", "x9", "sid=x9; Path=/"},
		{"jwt claim reads, never replaced", jwt, []Step{JWTClaim("sub")}, 1, "17", "42", ""},
		{"jwt number claim", jwt, []Step{JWTClaim("n")}, -1, "5", "6", ""},
		{"a value that joins its neighbours", "a-17-b", nil, 1, "17", "x-y", ""},
		{"an empty value", "a-17-b", nil, 1, "17", "", ""},
		{"a cookie value with a ;", "sid=abc-17", []Step{Cookie("sid")}, 1, "17", "4;2", ""},
		{"no plain word", "abc", nil, 0, "abc", "x7", "x7"},
	} {
		a := Address{Place: q, Steps: c.steps, Word: c.word}
		got, ok := a.WordOf(c.leaf)
		if !ok || got != c.was {
			t.Errorf("%s: WordOf(%q) = %q, %v; want %q", c.name, c.leaf, got, ok, c.was)
		}
		out, ok := a.ReplaceWord(c.leaf, c.value)
		if c.want == "" {
			if ok {
				t.Errorf("%s: ReplaceWord = %q; want a refusal", c.name, out)
			}
			continue
		}
		if !ok || out != c.want {
			t.Errorf("%s: ReplaceWord(%q, %q) = %q, %v; want %q", c.name, c.leaf, c.value, out, ok, c.want)
		}
	}

	for _, c := range []struct {
		leaf  string
		steps []Step
		word  int
	}{
		{"%zz", []Step{StepPct}, -1},
		{"a=1; a=2", []Step{Cookie("a")}, -1},
		{"b=1", []Step{Cookie("a")}, -1},
		{"not.a.jwt!", []Step{JWTClaim("sub")}, -1},
		{`{"a":1}`, []Step{StepJSON}, -1},
		{"a-b", nil, 2},
		{"", []Step{StepB64}, -1},
	} {
		a := Address{Place: q, Steps: c.steps, Word: c.word}
		if got, ok := a.WordOf(c.leaf); ok {
			t.Errorf("WordOf(%q) through %v = %q; want none", c.leaf, c.steps, got)
		}
		if out, ok := a.ReplaceWord(c.leaf, "x"); ok {
			t.Errorf("ReplaceWord(%q) through %v = %q; want a refusal", c.leaf, c.steps, out)
		}
	}

	for _, st := range []Step{StepPct, StepJSON, StepB64, StepB64URL} {
		e, ok := Encode(st, "abc-17")
		d, dok := Decode(st, e)
		if !ok || !dok || d != "abc-17" {
			t.Errorf("Encode then Decode through %s: %q %v then %q %v", st, e, ok, d, dok)
		}
	}
	if e, ok := Encode(JWTClaim("sub"), "x"); ok {
		t.Errorf("Encode of a JWT claim = %q; want a refusal", e)
	}
	if e, ok := Encode(Cookie("c"), "abc-17"); !ok || e != "abc-17" {
		t.Errorf("Encode of a cookie value = %q, %v", e, ok)
	}
	if e, ok := Encode(Cookie("c"), "a;b"); ok {
		t.Errorf("Encode of a cookie value holding ; = %q; want a refusal", e)
	}
}
