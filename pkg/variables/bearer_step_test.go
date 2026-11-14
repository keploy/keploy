package variables

import "testing"

// TestBearerStepOpensTheTokenWhole: bearer opens the token of an
// Authorization value of the Bearer scheme whole, a JWT's dots and a key's
// padding in it, the scheme in any case and any number of spaces after it;
// and nothing that is no such value: another scheme, no space after the
// scheme, no token, a token with a space, a ',' or a '"' in it, or one of
// '=' alone.
func TestBearerStepOpensTheTokenWhole(t *testing.T) {
	tok := sigToken(`{"sub":"u1","iat":1761609600}`, sigB)
	for _, c := range []struct{ text, want string }{
		{"Bearer " + tok, tok},
		{"bearer " + tok, tok},
		{"BEARER   " + tok, tok},
		{"Bearer kep_A1b2+c3/d4~e5==", "kep_A1b2+c3/d4~e5=="},
	} {
		if got, ok := Decode(StepBearer, c.text); !ok || got != c.want {
			t.Errorf("Decode(bearer, %q) = %q, %v; want %q", c.text, got, ok, c.want)
		}
	}
	for _, text := range []string{
		tok,
		"Basic " + tok,
		"Token " + tok,
		"Bearer" + tok,
		"Bearer",
		"Bearer ",
		"Bearer " + tok + " ",
		"Bearer " + tok + ", realm=x",
		`Bearer "` + tok + `"`,
		"Bearer ==",
		"Bearer a=b",
		"Bearers " + tok,
		"Digest " + tok,
	} {
		if got, ok := Decode(StepBearer, text); ok {
			t.Errorf("Decode(bearer, %q) = %q; want no bearer token", text, got)
		}
	}
	a, err := ParseAddress("header.authorization|bearer")
	if err != nil || len(a.Steps) != 1 || a.Steps[0] != StepBearer || a.Word != -1 || a.String() != "header.authorization|bearer" {
		t.Fatalf("ParseAddress = %+v, %v", a, err)
	}
	if got, ok := a.WordOf("Bearer " + tok); !ok || got != tok {
		t.Errorf("WordOf = %q, %v; want %q", got, ok, tok)
	}
	if a, err := ParseAddress("header.authorization|bearer|jwt.sub#0"); err != nil || len(a.Steps) != 2 || a.Word != 0 {
		t.Fatalf("ParseAddress(bearer|jwt.sub#0) = %+v, %v", a, err)
	}
}

// TestBearerStepWritesATokenAndKeepsTheScheme: a write at bearer replaces
// the token whole and keeps the scheme and the spaces after it as written;
// a value that is no token68 there (a space, a ',', empty) is refused, so
// the value stays one Bearer credential, and so is a text that is no
// Bearer value.
func TestBearerStepWritesATokenAndKeepsTheScheme(t *testing.T) {
	at := Address{Steps: []Step{StepBearer}, Word: -1}
	recTok := sigToken(`{"sub":"u1","iat":1761609600}`, sigA)
	liveTok := sigToken(`{"sub":"u1","iat":1761700000}`, sigB)
	for _, c := range []struct{ text, v, want string }{
		{"Bearer " + liveTok, recTok, "Bearer " + recTok},
		{"bearer  " + liveTok, recTok, "bearer  " + recTok},
		{"Bearer kep_1", "kep_22==", "Bearer kep_22=="},
	} {
		if out, ok := at.ReplaceWord(c.text, c.v); !ok || out != c.want {
			t.Errorf("ReplaceWord(%q, %q) = %q, %v; want %q", c.text, c.v, out, ok, c.want)
		}
	}
	for _, c := range []struct{ text, v string }{
		{"Bearer " + liveTok, recTok + " x"},
		{"Bearer " + liveTok, recTok + ",realm=x"},
		{"Bearer " + liveTok, ""},
		{"Basic " + liveTok, recTok},
		{liveTok, recTok},
	} {
		if out, ok := at.ReplaceWord(c.text, c.v); ok {
			t.Errorf("ReplaceWord(%q, %q) = %q; want a refusal", c.text, c.v, out)
		}
	}
}

// TestBearerStepStandsInTheTokenAlone: a word under bearer is a word of
// the token, after the scheme: it overlaps the token whole and a word of
// it, never the scheme, which a write there keeps, nor a word after it.
func TestBearerStepStandsInTheTokenAlone(t *testing.T) {
	leaf := "Bearer kep-abc-def"
	whole := Address{Steps: []Step{StepBearer}, Word: -1}
	word := Address{Steps: []Step{StepBearer}, Word: 1}
	scheme := Address{Word: 0}
	if got, ok := word.WordOf(leaf); !ok || got != "abc" {
		t.Fatalf("WordOf(bearer#1) = %q, %v; want abc", got, ok)
	}
	if out, ok := word.ReplaceWord(leaf, "xyz"); !ok || out != "Bearer kep-xyz-def" {
		t.Fatalf("ReplaceWord(bearer#1) = %q, %v; want Bearer kep-xyz-def", out, ok)
	}
	if !whole.Overlaps(word, leaf) || !word.Overlaps(whole, leaf) {
		t.Errorf("the token whole and a word of it do not overlap")
	}
	if whole.Overlaps(scheme, leaf) || scheme.Overlaps(whole, leaf) || word.Overlaps(scheme, leaf) {
		t.Errorf("a write under bearer overlaps the scheme")
	}
	// A word under bearer is written a word for a word: the words after it,
	// read with no step, stay where they are.
	first, last := Address{Steps: []Step{StepBearer}, Word: 0}, Address{Word: 3}
	if first.Overlaps(last, leaf) || last.Overlaps(first, leaf) {
		t.Errorf("a word of the token overlaps a word after it")
	}
}
