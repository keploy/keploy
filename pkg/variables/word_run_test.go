package variables

import (
	"fmt"
	"math/rand"
	"strconv"
	"testing"
)

// uuidBinds is the bindings one call makes at each word of id, a UUID at
// place body.<field> from word first on (a bare id: 0; "APP:"+id: 1), as
// variables prefix0..prefix4, its runs filled (WordRuns).
func uuidBinds(t *testing.T, prefix, field, live, recorded string, first int) []Binding {
	t.Helper()
	lw, rw := wordsOf(live), wordsOf(recorded)
	if len(lw) != len(rw) {
		t.Fatalf("%s and %s hold %d and %d words", live, recorded, len(lw), len(rw))
	}
	at := make([]Address, len(lw))
	for i := range lw {
		a, err := ParseAddress("body." + field + "#" + strconv.Itoa(first+i))
		if err != nil {
			t.Fatal(err)
		}
		at[i] = a
	}
	lr, rr := WordRuns(at, lw, rw)
	out := make([]Binding, len(lw))
	for i := range lw {
		out[i] = Binding{Name: prefix + strconv.Itoa(i), Value: lw[i], Recorded: rw[i], Place: at[i].String(), WordRun: lr[i], RecordedWordRun: rr[i]}
	}
	return out
}

func wordsOf(s string) []string {
	var out []string
	for _, w := range Words(s) {
		out = append(out, s[w[0]:w[1]])
	}
	return out
}

func TestWordRuns_ConsecutiveWordsOfOneTextAreOneRun(t *testing.T) {
	addr := func(s string) Address {
		a, err := ParseAddress(s)
		if err != nil {
			t.Fatal(err)
		}
		return a
	}
	at := []Address{addr("body.what#3"), addr("body.what#1"), addr("body.id#0"), addr("body.what#2"), addr("body.code"), addr("body.what#5"), addr("header.X-Id|b64#0"), addr("header.X-Id#0")}
	live := []string{"c", "a", "z", "b", "whole", "e", "y", "x"}
	rec := []string{"C", "A", "Z", "B", "WHOLE", "E", "Y", "X"}
	lr, rr := WordRuns(at, live, rec)
	want := []string{"a-b-c", "a-b-c", "z", "a-b-c", "", "e", "y", "x"}
	for i := range want {
		if lr[i] != want[i] {
			t.Fatalf("live run of %s: %q, want %q (all %q)", at[i], lr[i], want[i], lr)
		}
		if rr[i] != toUpper(want[i]) {
			t.Fatalf("recorded run of %s: %q, want %q", at[i], rr[i], toUpper(want[i]))
		}
	}
}

func toUpper(s string) string {
	b := []byte(s)
	for i, c := range b {
		if c >= 'a' && c <= 'z' {
			b[i] = c - 'a' + 'A'
		}
	}
	return string(b)
}

// The api-server's shape: two fresh v4 UUIDs, each bound word by word, whose
// version groups (12 random bits) are equal. Drawn from a fixed seed, every
// such pair binds, in one call or two, bare or after "APP:"; one UUID sent
// where two were recorded never does.
func TestTable_AGroupTwoFreshUUIDsShareByChanceBinds(t *testing.T) {
	rng := rand.New(rand.NewSource(20261103))
	uuid := func(version string) string {
		return fmt.Sprintf("%08x-%04x-%s-%04x-%012x", rng.Uint32(), rng.Intn(1<<16), version, 0x8000|rng.Intn(1<<14), rng.Int63n(1<<48))
	}
	for n := 0; n < 200; n++ {
		group := fmt.Sprintf("4%03x", rng.Intn(1<<12))
		recA, recB := uuid(fmt.Sprintf("4%03x", rng.Intn(1<<12))), uuid(fmt.Sprintf("4%03x", rng.Intn(1<<12)))
		liveA, liveB := uuid(group), uuid(group)
		for _, first := range []int{0, 1} {
			a := uuidBinds(t, "ev", "id", liveA, recA, 0)
			b := uuidBinds(t, "su", "key", liveB, recB, first)
			var tb Table
			if bad, with, _, ok := tb.BindAll(tb.Gen(), a); !ok {
				t.Fatalf("pair %d: %s did not bind: %+v beside %+v", n, liveA, bad, with)
			}
			if bad, with, _, ok := tb.BindAll(tb.Gen(), b); !ok {
				t.Fatalf("pair %d (first word %d): %s, whose group %s equals %s's by chance, did not bind: %+v beside %+v", n, first, liveB, group, liveA, bad, with)
			}
			var one Table
			if bad, with, _, ok := one.BindAll(one.Gen(), append(append([]Binding(nil), a...), b...)); !ok {
				t.Fatalf("pair %d (first word %d), in one call: %+v beside %+v", n, first, bad, with)
			}
			var copied Table
			copied.BindAll(copied.Gen(), a)
			if _, with, _, ok := copied.BindAll(copied.Gen(), uuidBinds(t, "su", "key", liveA, recB, first)); ok || with.Name == "" {
				t.Fatalf("pair %d (first word %d): %s sent where %s and %s were recorded bound", n, first, liveA, recA, recB)
			}
		}
	}
}

func TestTwoValues_WordRuns(t *testing.T) {
	word := func(name, value, recorded, run, recRun string) Binding {
		return Binding{Name: name, Value: value, Recorded: recorded, WordRun: run, RecordedWordRun: recRun}
	}
	for _, c := range []struct {
		name    string
		held, b Binding
		refused bool
	}{
		{"one live run, two recorded", word("a2", "41a3", "4011", "x-41a3-y", "p-4011-q"), word("b2", "41a3", "4c89", "x-41a3-y", "r-4c89-s"), true},
		{"two live runs", word("a2", "41a3", "4011", "x-41a3-y", "p-4011-q"), word("b2", "41a3", "4c89", "z-41a3-w", "r-4c89-s"), false},
		{"words of one run", word("a1", "41a3", "9dc0", "x-41a3-41a3", "p-9dc0-4011"), word("a2", "41a3", "4011", "x-41a3-41a3", "p-9dc0-4011"), false},
		{"a word beside a whole value", Binding{Name: "code", Value: "41a3", Recorded: "c0de"}, word("b2", "41a3", "4c89", "z-41a3-w", "r-4c89-s"), true},
		{"a whole value beside a word", word("b2", "41a3", "4c89", "z-41a3-w", "r-4c89-s"), Binding{Name: "code", Value: "41a3", Recorded: "c0de"}, true},
		{"a given word", Binding{Name: "g", Value: "41a3", Recorded: "4011", Given: true, WordRun: "41a3", RecordedWordRun: "4011"}, word("b2", "41a3", "4c89", "41a3", "4c89"), false},
		{"one recorded value", word("a2", "41a3", "4011", "x-41a3-y", "p-4011-q"), word("b2", "41a3", "4011", "x-41a3-y", "r-4011-s"), false},
	} {
		if got := TwoValues(c.held, c.b); got != c.refused {
			t.Fatalf("%s: TwoValues = %v, want %v", c.name, got, c.refused)
		}
	}
}
