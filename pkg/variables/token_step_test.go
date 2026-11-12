package variables

import "testing"

// A token step names one token of a longer text (EachToken), read whole
// wherever its own '-' and '_' fall: an id in a consent URL, a key after
// "Bearer ". It parses and prints as written, and only a number from 0
// written as Token writes it is one.
func TestTokenStepParsesAndPrints(t *testing.T) {
	for _, s := range []string{"header.Location|token.4", "body.user|token.0", "header.Location|token.4#1", "header.Location|pct|token.12"} {
		a, err := ParseAddress(s)
		if err != nil || a.String() != s {
			t.Errorf("ParseAddress(%q) = %q, %v; want it back as written", s, a.String(), err)
		}
	}
	if a, _ := ParseAddress("header.Location|token.4"); len(a.Steps) != 1 || a.Steps[0] != Token(4) || a.Word != -1 {
		t.Errorf("header.Location|token.4: steps %v word %d; want token.4, the token whole", a.Steps, a.Word)
	}
	for _, s := range []string{"body.user|token.", "body.user|token.x", "body.user|token.01", "body.user|token.-1", "body.user|token.+1"} {
		if a, err := parseAddress(s); err == nil {
			t.Errorf("parseAddress(%q) = %v; want no token step", s, a)
		}
		if _, ok := Decode(Step(s[len("body.user|"):]), "a b"); ok {
			t.Errorf("Decode(%q) opened a text; want no such step", s[len("body.user|"):])
		}
	}
}

const (
	consent = "https://app.keploy.io/oauth/consent?request="
	tokRec  = "kepreq_PnlPWeHYp6SxRRGMfFi0kUxyEdiumEmDVfOVZBrF68E"
	tokNext = "kepreq_q4X-Rm8LwZ2vNc7_bJ0sYh5GdP3fEa9TnU1oVi6WxQk"
)

// A token step reads the token whole and writes the next run's token in
// its place, every other byte kept, whatever its count of words; a value
// that would be no token there (it holds a byte outside the alphabets, or
// joins the bytes beside it) is refused, and so is a token the text lacks.
func TestTokenStepReadsAndWritesTheTokenWhole(t *testing.T) {
	a, _ := ParseAddress("header.Location|token.4")
	url := consent + tokRec + "&state=on"
	if got, ok := a.WordOf(url); !ok || got != tokRec {
		t.Errorf("WordOf = %q, %v; want the token whole", got, ok)
	}
	if got, ok := (Address{Steps: []Step{Token(4)}, Word: 1}).WordOf(url); !ok || got != "PnlPWeHYp6SxRRGMfFi0kUxyEdiumEmDVfOVZBrF68E" {
		t.Errorf("WordOf at word 1 of the token = %q, %v; want its random word", got, ok)
	}
	if got, ok := a.ReplaceWord(url, tokNext); !ok || got != consent+tokNext+"&state=on" {
		t.Errorf("ReplaceWord = %q, %v; want the next token in place, every other byte kept", got, ok)
	}
	for _, v := range []string{"kepreq.x", "kepreq&a", ""} {
		if got, ok := a.ReplaceWord(url, v); ok {
			t.Errorf("ReplaceWord(%q) = %q; want it refused, no token there", v, got)
		}
	}
	if got, ok := (Address{Steps: []Step{Token(7)}, Word: -1}).WordOf(url); ok {
		t.Errorf("WordOf at a token the text lacks = %q; want none", got)
	}
	// A write at a token the text lacks is refused, even where the value
	// put before the text would read back there (" a b" has two tokens:
	// "b" before it is the third token of "b a b").
	if got, ok := (Address{Steps: []Step{Token(2)}, Word: -1}).ReplaceWord(" a b", "b"); ok {
		t.Errorf("ReplaceWord at a token the text lacks = %q; want it refused", got)
	}
}

// A token step's span is the token's own bytes: a word before it, or a
// word after it that a write of one of the token's words cannot move, is
// another place; a word inside it is the same one.
func TestTokenStepOverlapsOnlyWhatItSpans(t *testing.T) {
	url := consent + tokRec + "&state=on"
	tok := Address{Steps: []Step{Token(4)}, Word: -1}
	first := Address{Steps: []Step{Token(4)}, Word: 0}
	if tok.Overlaps(Address{Word: 0}, url) {
		t.Error("the token overlaps the URL's first word; want two places")
	}
	if first.Overlaps(Address{Word: 10}, url) {
		t.Error("a word of the token, written word for word, overlaps a word after it; want two places")
	}
	if !tok.Overlaps(Address{Word: 7}, url) {
		t.Error("the token whole does not overlap its own random word; want one place")
	}
}

// EachToken cuts a run that the alphabets' bytes follow past its padding
// as none, and the bytes after it anew.
func TestEachTokenCutsAfterAPaddedRun(t *testing.T) {
	var got []string
	s := "tok=ab+cd/ef==;x"
	EachToken(s, 0, len(s), func(a, b int) bool { got = append(got, s[a:b]); return true })
	if len(got) != 2 || got[0] != "ab+cd/ef==" || got[1] != "x" {
		t.Errorf("EachToken = %q; want [ab+cd/ef== x]", got)
	}
}
