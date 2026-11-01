package variables

import (
	"encoding/base64"
	"reflect"
	"strings"
	"testing"

	"go.keploy.io/server/v3/pkg/templates"
)

// TestAWordAddressReadsAsItIsWritten: every step and a word index
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
	// A name ending in '\' keeps it before a step or a word.
	for _, a := range []Address{
		{Place: Place{Part: PartQuery, Name: `a\`}, Steps: []Step{StepB64}, Word: 2},
		{Place: Place{Part: PartHeader, Name: `X\`}, Word: 0},
		{Place: Place{Part: PartHeader, Name: "Authorization"}, Steps: []Step{JWTClaim(`e\`), StepPct}, Word: 1},
		{Place: Place{Part: PartHeader, Name: "Cookie"}, Steps: []Step{Cookie(`s\`)}, Word: 0},
	} {
		if b, err := ParseAddress(a.String()); err != nil || !reflect.DeepEqual(a, b) {
			t.Errorf("%+v prints %q, which reads as %+v, %v", a, a.String(), b, err)
		}
	}
	// A quoted JSON key's escaped '"' does not end the key, so a '|' after it
	// is the key's own.
	a = Address{Place: BodyPlace([]templates.PathSeg{{Key: `a"|b`}}), Steps: []Step{StepB64}, Word: 1}
	if b, err := ParseAddress(a.String()); a.String() != `body["a\"|b"]|b64#1` || err != nil || !reflect.DeepEqual(a, b) {
		t.Errorf("%+v prints %q, which reads as %+v, %v", a, a.String(), b, err)
	}
	// One written before the escape reads as it did.
	if a, err := ParseAddress("header.X|Y"); err != nil || a.Place.Name != "X|Y" || a.Word != -1 {
		t.Errorf("an unescaped name reads as %+v, %v", a, err)
	}
	// The limit: a name written before word addresses that ends in #<digits>
	// reads through ParseAddress as a word of the shorter name. Such a field
	// is read with Parse, which keeps the whole name.
	for _, c := range []struct{ s, short, whole string }{{"url.query.page#2", "page", "page#2"}, {"header.X-Id#1", "X-Id", "X-Id#1"}} {
		a, _ := ParseAddress(c.s)
		p, err := Parse(c.s)
		if a.Place.Name != c.short || a.Word < 0 || err != nil || p.Name != c.whole {
			t.Errorf("%q: ParseAddress %+v, Parse %+v %v", c.s, a, p, err)
		}
	}
	// So does one holding '|' and a step's name: ParseAddress reads the step.
	// Parse keeps the whole name (and prints it escaped).
	for _, c := range []struct {
		s, short, whole string
		st              Step
	}{{"url.query.a|json", "url.query.a", `url.query.a\|json`, StepJSON}, {"body.a|b64", "body.a", `body["a|b64"]`, StepB64}} {
		a, err := ParseAddress(c.s)
		short, _ := Parse(c.short)
		whole, perr := Parse(c.s)
		if err != nil || !reflect.DeepEqual(a, Address{Place: short, Steps: []Step{c.st}, Word: -1}) || perr != nil || whole.String() != c.whole {
			t.Errorf("%q: ParseAddress %+v %v, Parse %+v %v", c.s, a, err, whole, perr)
		}
	}

	for _, s := range []string{"url.path[2]|nope", "url.path[2]#x", "url.path[2]#01", "url.path[2]#-1", "header.", "url.path[2]|jwt.", "url.path[2]|cookie.", "body"} {
		if a, err := ParseAddress(s); err == nil || a.Word != -1 {
			t.Errorf("ParseAddress(%q) = %+v, %v; want an error and word -1 (the whole value)", s, a, err)
		}
	}
	// A word or a step of no place names nothing, and prints as Place{} does.
	for _, a := range []Address{{Word: -1}, {Word: 1}, {Steps: []Step{StepB64}, Word: 0}} {
		if s := a.String(); s != "" {
			t.Errorf("%+v prints %q, want \"\"", a, s)
		}
	}
}

// TestAWordIsReadAndReplacedThroughItsSteps: WordOf and ReplaceWord
// through pct, json, b64, b64url and cookie.<name>, every other byte kept;
// a JWT claim reads, and refuses to be replaced.
func TestAWordIsReadAndReplacedThroughItsSteps(t *testing.T) {
	q := Place{Part: PartQuery, Name: "q"}
	b64 := base64.StdEncoding.EncodeToString
	b64url := func(s string) string { return base64.RawURLEncoding.EncodeToString([]byte(s)) }
	jwt := b64url(`{"alg":"HS256"}`) + "." + b64url(`{"sub":"user-17","n":5,"o":{}}`) + ".c2ln"
	// A payload whose base64url text holds both '-' and '_': only the URL
	// alphabet opens it.
	urlJWT := b64url(`{"alg":"HS256"}`) + "." + b64url(`{"sub":"user-17","k":"??~","j":"~~>"}`) + ".c2ln"
	if p := strings.Split(urlJWT, ".")[1]; !strings.Contains(p, "-") || !strings.Contains(p, "_") {
		t.Fatalf("payload %q holds no '-' and '_'", p)
	}
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
		{"b64url in its own alphabet", "-_8tMTc=", []Step{StepB64URL}, 0, "17", "42", "-_8tNDI="},
		{"cookie", "a=1; sid=abc-17; b=2", []Step{Cookie("sid")}, 1, "17", "42", "a=1; sid=abc-42; b=2"},
		{"set-cookie", "sid=abc-17; Path=/", []Step{Cookie("sid")}, -1, "abc-17", "x9", "sid=x9; Path=/"},
		{"jwt claim reads, never replaced", jwt, []Step{JWTClaim("sub")}, 1, "17", "42", ""},
		{"jwt number claim", jwt, []Step{JWTClaim("n")}, -1, "5", "6", ""},
		{"jwt payload in the URL alphabet", urlJWT, []Step{JWTClaim("sub")}, 1, "17", "42", ""},
		{"a value that joins its neighbours", "a-17-b", nil, 1, "17", "x-y", ""},
		{"an empty value", "a-17-b", nil, 1, "17", "", ""},
		{"a cookie value with a ;", "sid=abc-17", []Step{Cookie("sid")}, 1, "17", "4;2", ""},
		{"no plain word", "abc", nil, 0, "abc", "x7", "x7"},
		// base64 keeps the recording's own padding: shown by its '=', or by its
		// length where it needed one and has none; where neither shows it, a
		// value whose padded and raw forms differ is refused.
		{"b64 padded stays padded", "aWQtMTI=", []Step{StepB64}, 1, "12", "1234", "aWQtMTIzNA=="},
		{"b64url raw stays raw", "aWQtMTI", []Step{StepB64URL}, 1, "12", "1234", "aWQtMTIzNA"},
		{"b64url padding not shown, forms equal", "aWQtMTIz", []Step{StepB64URL}, 1, "123", "456", "aWQtNDU2"},
		{"b64url padding not shown, forms differ", "aWQtMTIz", []Step{StepB64URL}, 1, "123", "1234", ""},
		{"b64 padding not shown, forms differ", "aWQtMTIz", []Step{StepB64}, 1, "123", "12", ""},
		// json re-encodes only a string it writes the same way.
		{"json keeps its HTML escape", `"a\u003cb-17"`, []Step{StepJSON}, 2, "17", "18", `"a\u003cb-18"`},
		{"json without an HTML escape", `"a<b-17"`, []Step{StepJSON}, 2, "17", "18", `"a<b-18"`},
		{"json escaping a / no encoder escapes", `"a\/b-17"`, []Step{StepJSON}, 2, "17", "18", ""},
		// A cookie value never takes a byte a cookie value cannot hold.
		{"a cookie value with a space", "sid=abc-17", []Step{Cookie("sid")}, -1, "abc-17", "a b", ""},
		{"a cookie value with a quote", "sid=abc-17", []Step{Cookie("sid")}, -1, "abc-17", `a"b`, ""},
		{"a cookie value with a comma", "sid=abc-17", []Step{Cookie("sid")}, -1, "abc-17", "a,b", ""},
		{"a cookie value with a backslash", "sid=abc-17", []Step{Cookie("sid")}, -1, "abc-17", `a\b`, ""},
		{"a cookie value with a DEL", "sid=abc-17", []Step{Cookie("sid")}, -1, "abc-17", "a\x7fb", ""},
		{"a cookie value with a control byte", "sid=abc-17", []Step{Cookie("sid")}, -1, "abc-17", "a\x01b", ""},
		// Blanks before a cookie's ';' are not its value.
		{"a cookie word before blanks", "sid=abc-17 ; b=2", []Step{Cookie("sid")}, 1, "17", "42", "sid=abc-42 ; b=2"},
		{"a cookie value before blanks", "sid=abc-17\t; b=2", []Step{Cookie("sid")}, -1, "abc-17", "x9", "sid=x9\t; b=2"},
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
		{"a%2", []Step{StepPct}, -1},
		{"a%", []Step{StepPct}, -1},
		{"a%2", []Step{StepPct}, 0},
		{"a=1; a=2", []Step{Cookie("a")}, -1},
		{"b=1", []Step{Cookie("a")}, -1},
		{"not.a.jwt!", []Step{JWTClaim("sub")}, -1},
		{`{"a":1}`, []Step{StepJSON}, -1},
		{`"abc"}`, []Step{StepJSON}, -1},
		{`"abc" "d"`, []Step{StepJSON}, -1},
		{`1]`, []Step{StepJSON}, -1},
		{"a-b", nil, 2},
		{"", []Step{StepB64}, -1},
		{"x." + b64url(`{"sub":"u1"}`), []Step{JWTClaim("sub")}, -1},
		{"x." + b64url(`{"sub":"u1"}`) + ".c.d", []Step{JWTClaim("sub")}, -1},
	} {
		a := Address{Place: q, Steps: c.steps, Word: c.word}
		if got, ok := a.WordOf(c.leaf); ok {
			t.Errorf("WordOf(%q) through %v = %q; want none", c.leaf, c.steps, got)
		}
		if out, ok := a.ReplaceWord(c.leaf, "x"); ok {
			t.Errorf("ReplaceWord(%q) through %v = %q; want a refusal", c.leaf, c.steps, out)
		}
	}

}

// ReadPlace reads a place the recording holds as written whole, and else as
// a word address.
func TestReadPlace(t *testing.T) {
	holds := func(names ...string) func(Place) bool {
		return func(p Place) bool {
			for _, n := range names {
				if p.String() == n {
					return true
				}
			}
			return false
		}
	}
	for _, c := range []struct {
		s, want string
		held    []string
	}{
		{s: "body.a#0", held: []string{`body["a#0"]`}, want: `body["a#0"]`},
		{s: "body.a#0", held: []string{"body.a"}, want: "body.a#0"},
		{s: "body.a#0", held: []string{`body["a#0"]`, "body.a"}, want: `body["a#0"]`},
		{s: "body.a#0", want: "body.a#0"},
		{s: "url.query.page#2", held: []string{`url.query.page\#2`}, want: `url.query.page\#2`},
		{s: "url.query.a|json", held: []string{`url.query.a\|json`}, want: `url.query.a\|json`},
		{s: "url.query.a|json", held: []string{"url.query.a"}, want: "url.query.a|json"},
		{s: "url.path[1]#1", held: []string{"url.path[1]"}, want: "url.path[1]#1"},
		{s: "header.X", held: []string{"header.X"}, want: "header.X"},
	} {
		a, err := ReadPlace(c.s, holds(c.held...))
		if err != nil || a.String() != c.want {
			t.Errorf("ReadPlace(%q) holding %v = %q, %v; want %q", c.s, c.held, a.String(), err, c.want)
		}
	}
	if _, err := ReadPlace("nowhere", holds()); err == nil {
		t.Error("a string of no place read")
	}
}
