package learn

import (
	"math"
	"testing"
)

// wordBits is the one definition of how many bits a word carries by its
// text: a UUID's 4-hex group carries 16, below the 32 a word needs to be
// evidence by its text alone; its 8-hex group carries 32; the whole UUID,
// the id token the reader lists beside each of its changed words, 128.
func TestReaderWordBits(t *testing.T) {
	for _, c := range []struct {
		word string
		bits float64
	}{
		{"4f3a", 16},
		{"1f00a000", 32},
		{"1730361600", 10 * math.Log2(10)},
		{"order", 5 * math.Log2(26)},
		{"a1b2c3", 6 * 4},
		{"q7", 2 * math.Log2(36)},
		{"ab-cd", 4 * 4},
		{"a+b", 2*4 + 8},
	} {
		if got := wordBits(c.word); math.Abs(got-c.bits) > 1e-9 {
			t.Errorf("wordBits(%q) = %v; want %v", c.word, got, c.bits)
		}
	}
	if chanceBits != 32 {
		t.Errorf("chanceBits = %d; want 32", chanceBits)
	}
	if wordBits("4f3a") >= chanceBits || wordBits("1f00a000") < chanceBits {
		t.Errorf("the threshold: a 4-hex group must fall below it, an 8-hex group reach it")
	}
	rec, live := "1f00a000-4f3a-4000-8000-5e0000000000", "1f00a000-9c1d-4000-8000-5e0000000000"
	rd := readText(rec, live)
	if len(rd.Words) != 1 {
		t.Fatalf("%q -> %q: %q; want the one changed group", rec, live, changedPairs(rd))
	}
	w := rd.Words[0]
	if w.RecBits != 16 || w.LiveBits != 16 {
		t.Errorf("the changed group's bits %v/%v; want 16", w.RecBits, w.LiveBits)
	}
	if len(w.Tokens) == 0 || w.Tokens[0].Kind != tokenID || w.Tokens[0].Rec != rec || wordBits(w.Tokens[0].Rec) != 128 {
		t.Errorf("the group's whole id token %+v; want the UUID, 128 bits", w.Tokens)
	}
}
