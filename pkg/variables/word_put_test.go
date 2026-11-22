package variables

import (
	"encoding/base64"
	"math/rand"
	"os"
	"strconv"
	"strings"
	"testing"
)

// PutWords writes each word where its address names it in the text given,
// or refuses: a write that moves another put's word, even one that then
// reads the same, is named with the put it moves.
func TestPutWordsWritesEachWordWhereItsAddressNamesIt(t *testing.T) {
	put := func(at, from, to string) WordPut {
		a, err := ParseAddress("header.S" + at)
		if err != nil {
			t.Fatal(err)
		}
		return WordPut{At: a, From: from, To: to}
	}
	for _, c := range []struct {
		name, text string
		puts       []WordPut
		want       string
		i, j       int
	}{
		{"two words", "hello appRec7 appRec8", []WordPut{put("#1", "appRec7", "a1"), put("#2", "appRec8", "b2")}, "hello a1 b2", -1, -1},
		{"a cookie of one word, then a word after it", "sid=appRec7; uid=appRec8",
			[]WordPut{put("|cookie.sid", "appRec7", "appLiveA"), put("#3", "appRec8", "appLiveB")}, "sid=appLiveA; uid=appLiveB", -1, -1},
		// Written, "app-LiveA" is two words: word 3 is then "uid".
		{"a cookie of two words moves a later put", "sid=appRec7; uid=appRec8",
			[]WordPut{put("|cookie.sid", "appRec7", "app-LiveA"), put("#3", "appRec8", "appLiveB")}, "", 0, 1},
		// Written, "appA" is one word: there is no word 4 left to write.
		{"a cookie of one word leaves a later put no word", "sid=app-x; uid=appRec8",
			[]WordPut{put("|cookie.sid", "app-x", "appA"), put("#4", "appRec8", "B")}, "", 0, 1},
		{"a cookie of two words moves an earlier put", "sid=appRec7; uid=appRec8",
			[]WordPut{put("#3", "appRec8", "appLiveB"), put("|cookie.sid", "appRec7", "app-LiveA")}, "", 1, 0},
		// Word 3 then reads "v" all the same, the first "v": written in the
		// other order, the second "v" is written.
		{"a moved word that reads the same", "sid=appRec7; v.v",
			[]WordPut{put("|cookie.sid", "appRec7", "app-LiveA"), put("#3", "v", "B")}, "", 0, 1},
		// Written first, the cookie's word cannot be written: its value
		// holds a space, which no cookie value is written with.
		{"a put that can be written only after another", "sid=a b",
			[]WordPut{put("", "sid=a b", "sid=a_b"), put("|cookie.sid#0", "a", "a")}, "", 0, 1},
		// In order, each escape is written as '-' before the cookie's
		// word; the cookie's word written before either, or after only
		// one, leaves a space in its value.
		{"a put that can be written only after two others", "sid=a%20.b%20.c",
			[]WordPut{put("#2", "20", "2D"), put("#4", "20", "2D"), put("|pct|cookie.sid#0", "a", "z")}, "", 0, 2},
		{"a text that does not hold the put's value", "a b", []WordPut{put("#0", "x", "y")}, "", 0, -1},
		{"a value that cannot stand at a word", "a b", []WordPut{put("#0", "a", "x-y")}, "", 0, -1},
	} {
		got, i, j, ok := PutWords(c.text, c.puts)
		if ok != (c.i < 0) || got != c.want || i != c.i || j != c.j {
			t.Errorf("%s: PutWords(%q) = %q, %d, %d, %v; want %q, %d, %d", c.name, c.text, got, i, j, ok, c.want, c.i, c.j)
		}
	}
}

// wordFuzz draws leaves and two addresses of each, as answers and binds of
// one value are written: words, cookies and attributes, escapes, base64.
type wordFuzz struct{ rng *rand.Rand }

func (f wordFuzz) word() string {
	const al = "abcxyzQ019"
	b := make([]byte, 1+f.rng.Intn(5))
	for i := range b {
		b[i] = al[f.rng.Intn(len(al))]
	}
	return string(b)
}

func (f wordFuzz) text(k int) string {
	toks := []string{"ab", "x1", "Q", "appRec7", "9", "-", ".", " ", "%41", "%2F", "%20", "/", "_", "%2D", "Z"}
	var b strings.Builder
	for i := 0; i < k; i++ {
		b.WriteString(toks[f.rng.Intn(len(toks))])
	}
	return b.String()
}

var fuzzCookies = []string{"sid", "uid", "a"}

func (f wordFuzz) leaf() string {
	if f.rng.Intn(3) == 0 {
		return f.text(1 + f.rng.Intn(6))
	}
	var parts []string
	for i := 0; i < 1+f.rng.Intn(3); i++ {
		v := f.text(f.rng.Intn(4))
		if f.rng.Intn(4) == 0 {
			v = base64.StdEncoding.EncodeToString([]byte(f.word() + " " + f.word()))
		}
		parts = append(parts, fuzzCookies[f.rng.Intn(len(fuzzCookies))]+"="+v)
	}
	if f.rng.Intn(3) == 0 {
		parts = append(parts, "Path=/x")
	}
	return strings.Join(parts, "; ")
}

func (f wordFuzz) addr() Address {
	c := Cookie(fuzzCookies[f.rng.Intn(len(fuzzCookies))])
	var steps []Step
	switch f.rng.Intn(7) {
	case 2:
		steps = []Step{StepPct}
	case 3:
		steps = []Step{c}
	case 4:
		steps = []Step{c, StepPct}
	case 5:
		steps = []Step{c, StepB64}
	case 6:
		steps = []Step{StepPct, c}
	}
	return Address{Place: Place{Part: PartHeader, Name: "X"}, Steps: steps, Word: f.rng.Intn(6) - 1}
}

// value is what a call may send at a: a word, or at a whole value a text
// of any words.
func (f wordFuzz) value(a Address) string {
	if a.Word >= 0 || f.rng.Intn(2) == 0 {
		return f.word()
	}
	return f.text(1 + f.rng.Intn(3))
}

func fuzzTries(t *testing.T) int {
	if s := os.Getenv("KEPLOY_WORD_FUZZ_TRIES"); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil {
			t.Fatal(err)
		}
		return n
	}
	return 100000
}

// Two addresses Overlaps calls apart are two places, whatever is written:
// a then b gives what b then a gives, and each reads back its value. And
// what PutWords writes for them is that text.
func TestWordsOverlapsCallsApartAreOrderFree(t *testing.T) {
	f := wordFuzz{rand.New(rand.NewSource(7))}
	n, bad := fuzzTries(t), 0
	for k := 0; k < n; k++ {
		leaf, a, b := f.leaf(), f.addr(), f.addr()
		ra, okA := a.WordOf(leaf)
		rb, okB := b.WordOf(leaf)
		if !okA || !okB || a.Overlaps(b, leaf) {
			continue
		}
		va, vb := f.value(a), f.value(b)
		ab, ok := writeBoth(leaf, WordPut{a, ra, va}, WordPut{b, rb, vb})
		if !ok {
			continue
		}
		ba, okBA := writeBoth(leaf, WordPut{b, rb, vb}, WordPut{a, ra, va})
		ga, _ := a.WordOf(ab)
		gb, _ := b.WordOf(ab)
		put, _, _, okPut := PutWords(leaf, []WordPut{{a, ra, va}, {b, rb, vb}})
		if ga == va && gb == vb && (!okBA || ab == ba) && (!okPut || put == ab) {
			continue
		}
		if bad++; bad <= 5 {
			t.Errorf("Overlaps(%s, %s, %q) = false: a=%q then b=%q gives %q (reads %q, %q), b then a %q (%v), PutWords %q (%v)", a, b, leaf, va, vb, ab, ga, gb, ba, okBA, put, okPut)
		}
	}
	t.Logf("%d counterexamples in %d tries", bad, n)
}

// A request's words, bound and put back as the replay does: for two places
// Overlaps calls apart, a call that sends values at them, in whichever
// order they are written into the recording, binds those values and is put
// back (PutWords) with each place holding its recorded value, or is
// refused. (A recorded value its step cannot write back, as a cookie value
// with a space or base64 in another padding, is refused; a pct escape the
// recording holds for a letter, "%41", is put back as the letter, which
// does not fit the recording. Neither is a wrong bind.)
func TestWordsBoundAndPutBackGiveTheCall(t *testing.T) {
	f := wordFuzz{rand.New(rand.NewSource(11))}
	n, bad := fuzzTries(t), 0
	for k := 0; k < n; k++ {
		rec, a, b := f.leaf(), f.addr(), f.addr()
		ra, okA := a.WordOf(rec)
		rb, okB := b.WordOf(rec)
		if !okA || !okB || a.Overlaps(b, rec) {
			continue
		}
		va, vb := f.value(a), f.value(b)
		p, q := WordPut{a, ra, va}, WordPut{b, rb, vb}
		if f.rng.Intn(2) == 0 {
			p, q = q, p
		}
		live, ok := writeBoth(rec, p, q)
		if !ok {
			continue
		}
		la, _ := a.WordOf(live)
		lb, _ := b.WordOf(live)
		back, _, _, ok := PutWords(live, []WordPut{{a, la, ra}, {b, lb, rb}})
		ga, _ := a.WordOf(back)
		gb, _ := b.WordOf(back)
		if la == va && lb == vb && (!ok || ga == ra && gb == rb) {
			continue
		}
		if bad++; bad <= 5 {
			t.Errorf("Overlaps(%s, %s, %q) = false: the call %q (a=%q, b=%q) binds %q and %q, and is put back as %q (%v), reading %q and %q", a, b, rec, live, va, vb, la, lb, back, ok, ga, gb)
		}
	}
	t.Logf("%d counterexamples in %d tries", bad, n)
}
