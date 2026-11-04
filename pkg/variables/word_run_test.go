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
	ws := make([]WordBind, len(lw))
	for i := range lw {
		ws[i] = WordBind{Text: "body." + field, Word: first + i, Live: lw[i]}
	}
	runs := WordRuns(ws)
	out := make([]Binding, len(lw))
	for i := range lw {
		out[i] = runs[i].On(Binding{Name: prefix + strconv.Itoa(i), Value: lw[i], Recorded: rw[i], Place: "body." + field + "#" + strconv.Itoa(first+i)})
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
	ws := []WordBind{
		{"body.what", 3, "c"}, {"body.what", 1, "a"}, {"body.id", 0, "z"}, {"body.what", 2, "b"},
		{"body.code", -1, "whole"}, {"body.what", 5, "e"}, {"header.X-Id|b64", 1, "y"}, {"header.X-Id", 0, "x"},
	}
	runs := WordRuns(ws)
	want := []Run{{"a-b-c", 2}, {"a-b-c", 0}, {"z", 0}, {"a-b-c", 1}, {}, {"e", 0}, {"y", 0}, {"x", 0}}
	for i := range want {
		if runs[i] != want[i] {
			t.Fatalf("run of %+v: %+v, want %+v (all %+v)", ws[i], runs[i], want[i], runs)
		}
	}
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
	word := func(name, value, recorded, run string, at int) Binding {
		return Binding{Name: name, Value: value, Recorded: recorded, WordRun: run, WordRunAt: at}
	}
	for _, c := range []struct {
		name    string
		held, b Binding
		refused bool
	}{
		{"one live run, two recorded", word("a2", "41a3", "4011", "x-41a3-y", 1), word("b2", "41a3", "4c89", "x-41a3-y", 1), true},
		{"two live runs", word("a2", "41a3", "4011", "x-41a3-y", 1), word("b2", "41a3", "4c89", "z-41a3-w", 1), false},
		{"words of one run", word("a1", "41a3", "9dc0", "x-41a3-41a3", 1), word("a2", "41a3", "4011", "x-41a3-41a3", 2), false},
		{"a word beside a whole value", Binding{Name: "code", Value: "41a3", Recorded: "c0de"}, word("b2", "41a3", "4c89", "z-41a3-w", 1), true},
		{"a whole value beside a word", word("b2", "41a3", "4c89", "z-41a3-w", 1), Binding{Name: "code", Value: "41a3", Recorded: "c0de"}, true},
		{"a given word", Binding{Name: "g", Value: "41a3", Recorded: "4011", Given: true, WordRun: "41a3"}, word("b2", "41a3", "4c89", "41a3", 0), false},
		{"one recorded value", word("a2", "41a3", "4011", "x-41a3-y", 1), word("b2", "41a3", "4011", "x-41a3-y", 1), false},
		// Set side by side at the shared word, the runs' common words are
		// all equal: one value, beside another word or in another layout.
		{"a short run inside a longer one", word("c1", "41a3", "4c89", "x-41a3-y", 1), word("k1", "41a3", "4011", "x-41a3-y-p-41a3-q", 1), true},
		{"the same, the shared word later in the longer run", word("c1", "41a3", "4c89", "x-41a3-y", 1), word("k4", "41a3", "4011", "p-q-r-x-41a3-y", 4), true},
		{"a run alone beside a word of another", word("c0", "41a3", "4c89", "41a3", 0), word("k1", "41a3", "4011", "x-41a3-y", 1), true},
		{"one value twice in one run", word("k1", "41a3", "4011", "x-41a3-x-41a3", 1), word("k3", "41a3", "4c89", "x-41a3-x-41a3", 3), true},
		// A word they both hold there differs live: chance.
		{"a short run whose neighbour differs", word("c1", "41a3", "4c89", "z-41a3-y", 1), word("k1", "41a3", "4011", "x-41a3-y-p-41a3-q", 1), false},
		{"aligned at the shared word, not at the first", word("c0", "41a3", "4c89", "41a3-w", 0), word("k4", "41a3", "4011", "p-q-r-x-41a3-y", 4), false},
	} {
		if got := TwoValues(c.held, c.b); got != c.refused {
			t.Fatalf("%s: TwoValues = %v, want %v", c.name, got, c.refused)
		}
	}
}

// textBinds is one call's bindings at every word of each text's value, live
// and recorded, as variables prefix0, prefix1..., their runs filled.
func textBinds(t *testing.T, texts ...[4]string) []Binding {
	t.Helper()
	var ws []WordBind
	var out []Binding
	for _, x := range texts {
		text, prefix, live, recorded := x[0], x[1], x[2], x[3]
		lw, rw := wordsOf(live), wordsOf(recorded)
		if len(lw) != len(rw) {
			t.Fatalf("%s and %s hold %d and %d words", live, recorded, len(lw), len(rw))
		}
		for i := range lw {
			ws = append(ws, WordBind{Text: text, Word: i, Live: lw[i]})
			out = append(out, Binding{Name: prefix + strconv.Itoa(i), Value: lw[i], Recorded: rw[i], Place: text + "#" + strconv.Itoa(i)})
		}
	}
	for i, r := range WordRuns(ws) {
		out[i] = r.On(out[i])
	}
	return out
}

// Two values bound word by word and set apart only by punctuation are one
// run ("<A>/<B>"). One UUID sent where two were recorded stays refused there:
// beside another place, in one call or two, and twice in one text. Two fresh
// UUIDs side by side, whose groups are equal by chance, still bind.
func TestTable_OneUUIDWhereTwoWereRecordedBesideAnAdjacentBoundUUIDIsRefused(t *testing.T) {
	const (
		recA  = "6f9619ff-8b86-4011-b42d-00c04fc964ff"
		recB  = "c9bf9e57-1685-4c89-bafb-ff5af830be8a"
		recC  = "1e2d3c4b-5a69-4788-9766-554433221100"
		liveX = "7d444840-9dc0-41a3-a15b-5b4c7a3f9e21"
		liveY = "0a0b0c0d-1e1f-4a2b-8c3d-4e5f6a7b8c9d"
		liveZ = "e3b0c442-98fc-41a3-8d2e-1c5a4e9b7f30"
	)
	refused := func(t *testing.T, calls ...[]Binding) {
		t.Helper()
		var tb Table
		for i, call := range calls {
			bad, with, _, ok := tb.BindAll(tb.Gen(), call)
			if i < len(calls)-1 {
				if !ok {
					t.Fatalf("call %d did not bind: %+v beside %+v", i, bad, with)
				}
				continue
			}
			if ok || with.Name == "" {
				t.Fatalf("one value where two were recorded bound (bad %+v, beside %+v)", bad, with)
			}
		}
	}
	t.Run("beside another place, in one call", func(t *testing.T) {
		refused(t, textBinds(t, [4]string{"body.key", "k", liveX + "/" + liveY, recA + "/" + recB}, [4]string{"body.c", "c", liveX, recC}))
	})
	t.Run("beside another place, the run's own UUID second", func(t *testing.T) {
		refused(t, textBinds(t, [4]string{"body.key", "k", liveY + "/" + liveX, recB + "/" + recA}, [4]string{"body.c", "c", liveX, recC}))
	})
	t.Run("across two calls", func(t *testing.T) {
		refused(t, textBinds(t, [4]string{"body.key", "k", liveX + "/" + liveY, recA + "/" + recB}), textBinds(t, [4]string{"body.c", "c", liveX, recC}))
	})
	t.Run("across two calls, the long run second", func(t *testing.T) {
		refused(t, textBinds(t, [4]string{"body.c", "c", liveX, recC}), textBinds(t, [4]string{"body.key", "k", liveX + "/" + liveY, recA + "/" + recB}))
	})
	t.Run("twice in one text", func(t *testing.T) {
		refused(t, textBinds(t, [4]string{"body.key", "k", liveX + "/" + liveX, recA + "/" + recB}))
	})
	t.Run("two fresh UUIDs side by side, a group equal by chance, bind", func(t *testing.T) {
		var tb Table
		call := textBinds(t, [4]string{"body.key", "k", liveX + "/" + liveY, recA + "/" + recB}, [4]string{"body.c", "c", liveZ, recC})
		if bad, with, _, ok := tb.BindAll(tb.Gen(), call); !ok {
			t.Fatalf("%s/%s beside %s (group 41a3 equal by chance) did not bind: %+v beside %+v", liveX, liveY, liveZ, bad, with)
		}
	})
}
