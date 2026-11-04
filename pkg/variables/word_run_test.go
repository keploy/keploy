package variables

import (
	"fmt"
	"math/rand"
	"strconv"
	"strings"
	"testing"

	"go.keploy.io/server/v3/pkg/provenswitch"
)

// uuidBinds is the bindings one call makes at each word of id, a UUID at
// place body.<field> from word first on (a bare id: 0; "APP:"+id: 1), as
// variables prefix0..prefix4, its runs filled (WordRuns) in a mock that
// holds the recorded UUID whole at another place (the api-server's ids, each
// also sent whole in a field of its own), each word named a word of that one
// learned value (WordBind.Of).
func uuidBinds(t *testing.T, prefix, field, live, recorded string, first int) []Binding {
	t.Helper()
	return uuidBindsHeld(t, heldAt(recorded), recorded, prefix, field, live, recorded, first)
}

// uuidBindsHeld is uuidBinds in a mock that holds whole elsewhere what
// elsewhere says, each word named a word of the learned value of ("": none).
func uuidBindsHeld(t *testing.T, elsewhere func(string) bool, of, prefix, field, live, recorded string, first int) []Binding {
	t.Helper()
	lw, rw := wordsOf(live), wordsOf(recorded)
	if len(lw) != len(rw) {
		t.Fatalf("%s and %s hold %d and %d words", live, recorded, len(lw), len(rw))
	}
	text := recorded
	if first == 1 {
		text = "APP:" + recorded
	}
	ws := make([]WordBind, len(lw))
	for i := range lw {
		ws[i] = WordBind{Text: "body." + field, Word: first + i, Live: lw[i], Recorded: rw[i], RecordedText: text, Elsewhere: elsewhere, Of: of}
	}
	runs := WordRuns(ws)
	out := make([]Binding, len(lw))
	for i := range lw {
		out[i] = runs[i].On(Binding{Name: prefix + strconv.Itoa(i), Value: lw[i], Recorded: rw[i], Place: "body." + field + "#" + strconv.Itoa(first+i)})
	}
	return out
}

// heldNowhere is a mock that holds no stretch of a run whole elsewhere: each
// word is its own value (main's word by word check).
func heldNowhere(string) bool { return false }

// heldAt is a mock that holds each of values whole at another place.
func heldAt(values ...string) func(string) bool {
	return func(s string) bool {
		for _, v := range values {
			if v == s {
				return true
			}
		}
		return false
	}
}

// learnedAs is a learning that names each word of a text a word of the
// shortest of values the text holds over it (WordBind.Of), and of no value
// where none does.
func learnedAs(values ...string) func(text string, word int) string {
	return func(text string, word int) string {
		ws := Words(text)
		if word >= len(ws) {
			return ""
		}
		best := ""
		for _, v := range values {
			for off := 0; off < len(text); {
				i := strings.Index(text[off:], v)
				if i < 0 {
					break
				}
				if from := off + i; ws[word][0] >= from && ws[word][1] <= from+len(v) && (best == "" || len(v) < len(best)) {
					best = v
				}
				off += i + 1
			}
		}
		return best
	}
}

func wordsOf(s string) []string {
	var out []string
	for _, w := range Words(s) {
		out = append(out, s[w[0]:w[1]])
	}
	return out
}

func TestWordRuns_ConsecutiveWordsOfOneTextAreOneRun(t *testing.T) {
	recorded := map[string]string{"body.what": "w A B C v E", "body.id": "Z", "body.code": "WHOLE", "header.X-Id|b64": "q Y", "header.X-Id": "X"}
	ws := []WordBind{
		{Text: "body.what", Word: 3, Live: "c", Recorded: "C"}, {Text: "body.what", Word: 1, Live: "a", Recorded: "A"}, {Text: "body.id", Word: 0, Live: "z", Recorded: "Z"}, {Text: "body.what", Word: 2, Live: "b", Recorded: "B"},
		{Text: "body.code", Word: -1, Live: "whole", Recorded: "WHOLE"}, {Text: "body.what", Word: 5, Live: "e", Recorded: "E"}, {Text: "header.X-Id|b64", Word: 1, Live: "y", Recorded: "Y"}, {Text: "header.X-Id", Word: 0, Live: "x", Recorded: "X"},
	}
	for i := range ws {
		ws[i].RecordedText, ws[i].Elsewhere = recorded[ws[i].Text], heldAt("A B C")
	}
	alone := []Run{{"c", "C", 0}, {"a", "A", 0}, {"z", "Z", 0}, {"b", "B", 0}, {}, {"e", "E", 0}, {"y", "Y", 0}, {"x", "X", 0}}
	// Held whole elsewhere, but named a word of no learned value: each word
	// is its own value.
	if runs := WordRuns(ws); fmt.Sprint(runs) != fmt.Sprint(alone) {
		t.Fatalf("runs no learned value names: %+v, want each word alone %+v", runs, alone)
	}
	of := learnedAs("A B C")
	for i := range ws {
		if ws[i].Word >= 0 {
			ws[i].Of = of(ws[i].RecordedText, ws[i].Word)
		}
	}
	runs := WordRuns(ws)
	want := []Run{{"a-b-c", "A-B-C", 2}, {"a-b-c", "A-B-C", 0}, {"z", "Z", 0}, {"a-b-c", "A-B-C", 1}, {}, {"e", "E", 0}, {"y", "Y", 0}, {"x", "X", 0}}
	for i := range want {
		if runs[i] != want[i] {
			t.Fatalf("run of %+v: %+v, want %+v (all %+v)", ws[i], runs[i], want[i], runs)
		}
	}
	// Words held as one stretch but named two learned values are two.
	ws[3].Of = "B"
	if runs := WordRuns(ws); fmt.Sprint(runs) != fmt.Sprint(alone) {
		t.Fatalf("runs of a held stretch learned as two values: %+v, want each word alone %+v", runs, alone)
	}
}

// The api-server's shape: two fresh v4 UUIDs, each bound word by word, each
// held whole at another place of its mock and named one learned value, whose
// version groups (12 random bits) are equal. Drawn from a fixed seed, every
// such pair binds, in one call or two, bare or after "APP:"; one UUID sent
// where two were recorded never does. In mocks that hold no part of them
// whole elsewhere, or where no learned value names their words, the pair
// shows no value, so each word is checked alone, and the group equal by
// chance is refused, as main refuses it.
func TestTable_AGroupTwoFreshUUIDsShareByChanceBinds(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
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
			var bare Table
			bare.BindAll(bare.Gen(), uuidBindsHeld(t, heldNowhere, recA, "ev", "id", liveA, recA, 0))
			if _, with, _, ok := bare.BindAll(bare.Gen(), uuidBindsHeld(t, heldNowhere, recB, "su", "key", liveB, recB, first)); ok || with.Name == "" {
				t.Fatalf("pair %d (first word %d): %s beside %s, neither held whole elsewhere, bound; main refuses the group %s", n, first, liveB, liveA, group)
			}
			var unnamed Table
			unnamed.BindAll(unnamed.Gen(), uuidBindsHeld(t, heldAt(recA), "", "ev", "id", liveA, recA, 0))
			if _, with, _, ok := unnamed.BindAll(unnamed.Gen(), uuidBindsHeld(t, heldAt(recB), "", "su", "key", liveB, recB, first)); ok || with.Name == "" {
				t.Fatalf("pair %d (first word %d): %s beside %s, each held whole but named no learned value, bound; main refuses the group %s", n, first, liveB, liveA, group)
			}
		}
	}
}

// Under the proven switch two word bindings that share a live value are set
// side by side at that word: a pair of words equal live where its recorded
// words differ is a copy (refused), a pair recorded as one word shows
// nothing, and only a pair differing live and as recorded, with no copy,
// reads the shared word as chance. With the switch off each case is main's
// word by word check.
func TestTwoValues_WordRuns(t *testing.T) {
	word := func(name, value, recorded, run, runRecorded string, at int) Binding {
		return Binding{Name: name, Value: value, Recorded: recorded, WordRun: run, WordRunRecorded: runRecorded, WordRunAt: at}
	}
	cases := []struct {
		name    string
		held, b Binding
		refused bool
	}{
		{"one live run, two recorded", word("a2", "41a3", "4011", "x-41a3-y", "p-4011-q", 1), word("b2", "41a3", "4c89", "x-41a3-y", "r-4c89-s", 1), true},
		{"two live runs", word("a2", "41a3", "4011", "x-41a3-y", "p-4011-q", 1), word("b2", "41a3", "4c89", "z-41a3-w", "r-4c89-s", 1), false},
		{"words of one run", word("a1", "41a3", "9dc0", "x-41a3-41a3", "p-9dc0-4011", 1), word("a2", "41a3", "4011", "x-41a3-41a3", "p-9dc0-4011", 2), false},
		{"a word beside a whole value", Binding{Name: "code", Value: "41a3", Recorded: "c0de"}, word("b2", "41a3", "4c89", "z-41a3-w", "r-4c89-s", 1), true},
		{"a whole value beside a word", word("b2", "41a3", "4c89", "z-41a3-w", "r-4c89-s", 1), Binding{Name: "code", Value: "41a3", Recorded: "c0de"}, true},
		{"a given word", Binding{Name: "g", Value: "41a3", Recorded: "4011", Given: true, WordRun: "41a3", WordRunRecorded: "4011"}, word("b2", "41a3", "4c89", "41a3", "4c89", 0), false},
		{"one recorded value", word("a2", "41a3", "4011", "x-41a3-y", "p-4011-q", 1), word("b2", "41a3", "4011", "x-41a3-y", "p-4011-q", 1), false},
		// A pair equal live that was two words when recorded: a copy.
		{"a short run inside a longer one", word("c1", "41a3", "4c89", "x-41a3-y", "a-4c89-b", 1), word("k1", "41a3", "4011", "x-41a3-y-p-41a3-q", "c-4011-d-e-f-g", 1), true},
		{"the same, the shared word later in the longer run", word("c1", "41a3", "4c89", "x-41a3-y", "a-4c89-b", 1), word("k4", "41a3", "4011", "p-q-r-x-41a3-y", "e-f-g-c-4011-d", 4), true},
		{"one value twice in one run", word("k1", "41a3", "4011", "x-41a3-x-41a3", "a-4011-b-4c89", 1), word("k3", "41a3", "4c89", "x-41a3-x-41a3", "a-4011-b-4c89", 3), true},
		{"a short run, one neighbour differing and one copied", word("c1", "41a3", "4c89", "z-41a3-y", "a-4c89-b", 1), word("k1", "41a3", "4011", "x-41a3-y-p-41a3-q", "c-4011-d-e-f-g", 1), true},
		// No pair to read, or none that differs as recorded: refused.
		{"a run alone beside a word of another", word("c0", "41a3", "4c89", "41a3", "4c89", 0), word("k1", "41a3", "4011", "x-41a3-y", "c-4011-d", 1), true},
		{"a neighbour recorded as one word, equal live", word("a1", "41a3", "4011", "x-41a3", "a-4011", 1), word("b1", "41a3", "4c89", "x-41a3", "a-4c89", 1), true},
		{"a neighbour recorded as one word, differing live", word("a1", "41a3", "4011", "x-41a3", "a-4011", 1), word("b1", "41a3", "4c89", "z-41a3", "a-4c89", 1), true},
		{"recorded words not as many as live ones", word("a2", "41a3", "4011", "x-41a3-y", "p-4011", 1), word("b2", "41a3", "4c89", "z-41a3-w", "r-4c89-s", 1), true},
		// Pairs differing live and as recorded, none copied: chance.
		{"a short run whose neighbours differ", word("c1", "41a3", "4c89", "z-41a3-w", "a-4c89-b", 1), word("k1", "41a3", "4011", "x-41a3-y-p-41a3-q", "c-4011-d-e-f-g", 1), false},
		{"aligned at the shared word, not at the first", word("c0", "41a3", "4c89", "41a3-w", "4c89-b", 0), word("k4", "41a3", "4011", "p-q-r-x-41a3-y", "e-f-g-c-4011-d", 4), false},
		{"the one neighbour, before the shared word, differs", word("a1", "41a3", "4011", "z-41a3", "a-4011", 1), word("b1", "41a3", "4c89", "x-41a3", "c-4c89", 1), false},
	}
	t.Run("switch on", func(t *testing.T) {
		t.Setenv(provenswitch.Env, "1")
		for _, c := range cases {
			if got := TwoValues(c.held, c.b); got != c.refused {
				t.Errorf("%s: TwoValues = %v, want %v", c.name, got, c.refused)
			}
		}
	})
	t.Run("switch off: main's word by word check", func(t *testing.T) {
		t.Setenv(provenswitch.Env, "")
		for _, c := range cases {
			main := !c.held.Given && c.held.Name != c.b.Name && c.held.Value == c.b.Value && c.held.Recorded != c.b.Recorded
			if got := TwoValues(c.held, c.b); got != main {
				t.Errorf("%s: TwoValues = %v, want main's %v", c.name, got, main)
			}
		}
	})
}

// textBindsHeld is one call's bindings at every word of each text's value,
// live and recorded, as variables prefix0, prefix1..., their runs filled, in
// a mock that holds whole elsewhere what elsewhere says.
func textBindsHeld(t *testing.T, elsewhere func(string) bool, texts ...[4]string) []Binding {
	t.Helper()
	return textBindsNamed(t, elsewhere, nil, texts...)
}

// textBindsNamed is textBindsHeld with each word named a word of the learned
// value of says (nil: none).
func textBindsNamed(t *testing.T, elsewhere func(string) bool, of func(string, int) string, texts ...[4]string) []Binding {
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
			w := WordBind{Text: text, Word: i, Live: lw[i], Recorded: rw[i], RecordedText: recorded, Elsewhere: elsewhere}
			if of != nil {
				w.Of = of(recorded, i)
			}
			ws = append(ws, w)
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
// beside another place, in one call or two, and twice in one text, whether
// the mocks hold each UUID whole elsewhere or nothing, and whether learning
// names each UUID one value or not. Two fresh UUIDs side by side, whose
// groups are equal by chance, bind only where the mocks hold each UUID whole
// elsewhere and learning names each one value, and are refused, as main
// refuses them, where they hold no part of either text or learning names no
// value.
func TestTable_OneUUIDWhereTwoWereRecordedBesideAnAdjacentBoundUUIDIsRefused(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	const (
		recA  = "6f9619ff-8b86-4011-b42d-00c04fc964ff"
		recB  = "c9bf9e57-1685-4c89-bafb-ff5af830be8a"
		recC  = "1e2d3c4b-5a69-4788-9766-554433221100"
		recD  = "aabbccdd-eeff-4a11-9b22-334455667788"
		liveX = "7d444840-9dc0-41a3-a15b-5b4c7a3f9e21"
		liveY = "0a0b0c0d-1e1f-4a2b-8c3d-4e5f6a7b8c9d"
		liveZ = "e3b0c442-98fc-41a3-8d2e-1c5a4e9b7f30"
		liveP = "11112222-3333-4444-8555-666677778888"
		liveQ = "9999aaaa-bbbb-4ccc-9ddd-eeeeffff0000"
	)
	check := func(t *testing.T, held func(string) bool, of func(string, int) string, uuids bool) {
		bindText := func(texts ...[4]string) []Binding { return textBindsNamed(t, held, of, texts...) }
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
			refused(t, bindText([4]string{"body.key", "k", liveX + "/" + liveY, recA + "/" + recB}, [4]string{"body.c", "c", liveX, recC}))
		})
		t.Run("beside another place, the run's own UUID second", func(t *testing.T) {
			refused(t, bindText([4]string{"body.key", "k", liveY + "/" + liveX, recB + "/" + recA}, [4]string{"body.c", "c", liveX, recC}))
		})
		t.Run("across two calls", func(t *testing.T) {
			refused(t, bindText([4]string{"body.key", "k", liveX + "/" + liveY, recA + "/" + recB}), bindText([4]string{"body.c", "c", liveX, recC}))
		})
		t.Run("across two calls, the long run second", func(t *testing.T) {
			refused(t, bindText([4]string{"body.c", "c", liveX, recC}), bindText([4]string{"body.key", "k", liveX + "/" + liveY, recA + "/" + recB}))
		})
		t.Run("twice in one text", func(t *testing.T) {
			refused(t, bindText([4]string{"body.key", "k", liveX + "/" + liveX, recA + "/" + recB}))
		})
		// One UUID at two places where two were recorded, each beside another
		// bound UUID that differs live: the rest of the shared UUID is copied.
		t.Run("first in each text, beside different seconds, in one call", func(t *testing.T) {
			refused(t, bindText([4]string{"body.k1", "k", liveX + "/" + liveY, recA + "/" + recB}, [4]string{"body.k2", "c", liveX + "/" + liveZ, recC + "/" + recD}))
		})
		t.Run("first in each text, beside different seconds, in two calls", func(t *testing.T) {
			refused(t, bindText([4]string{"body.k1", "k", liveX + "/" + liveY, recA + "/" + recB}), bindText([4]string{"body.k2", "c", liveX + "/" + liveZ, recC + "/" + recD}))
		})
		t.Run("second in each text, beside different firsts, in one call", func(t *testing.T) {
			refused(t, bindText([4]string{"body.k1", "k", liveP + "/" + liveX, recB + "/" + recA}, [4]string{"body.k2", "c", liveQ + "/" + liveX, recD + "/" + recC}))
		})
		t.Run("second in each text, beside different firsts, in two calls", func(t *testing.T) {
			refused(t, bindText([4]string{"body.k1", "k", liveP + "/" + liveX, recB + "/" + recA}), bindText([4]string{"body.k2", "c", liveQ + "/" + liveX, recD + "/" + recC}))
		})
		t.Run("both halves equal", func(t *testing.T) {
			refused(t, bindText([4]string{"body.k1", "k", liveX + "/" + liveY, recA + "/" + recB}, [4]string{"body.k2", "c", liveX + "/" + liveY, recC + "/" + recD}))
		})
		chance := func() []Binding {
			return bindText([4]string{"body.key", "k", liveX + "/" + liveY, recA + "/" + recB}, [4]string{"body.c", "c", liveZ, recC})
		}
		if !uuids {
			// No part of either text is held whole elsewhere, or no learned
			// value names it: each word is its own value, and the group equal
			// by chance is refused, as main refuses it.
			t.Run("two fresh UUIDs side by side, a group equal by chance, are refused", func(t *testing.T) {
				var tb Table
				if _, with, _, ok := tb.BindAll(tb.Gen(), chance()); ok || with.Name == "" {
					t.Fatalf("%s/%s beside %s, no part held elsewhere, bound; main refuses one live word of two recorded", liveX, liveY, liveZ)
				}
			})
			return
		}
		t.Run("two fresh UUIDs side by side, a group equal by chance, bind", func(t *testing.T) {
			var tb Table
			if bad, with, _, ok := tb.BindAll(tb.Gen(), chance()); !ok {
				t.Fatalf("%s/%s beside %s (group 41a3 equal by chance) did not bind: %+v beside %+v", liveX, liveY, liveZ, bad, with)
			}
		})
		t.Run("switch off: the group equal by chance is refused, as main", func(t *testing.T) {
			t.Setenv(provenswitch.Env, "")
			var tb Table
			if _, with, _, ok := tb.BindAll(tb.Gen(), chance()); ok || with.Name == "" {
				t.Fatalf("with the switch off, %s/%s beside %s bound; main refuses one live word of two recorded", liveX, liveY, liveZ)
			}
		})
	}
	for _, m := range []struct {
		name  string
		held  func(string) bool
		of    func(string, int) string
		uuids bool
	}{
		{"each UUID held whole elsewhere and learned as one value", heldAt(recA, recB, recC, recD), learnedAs(recA, recB, recC, recD), true},
		{"each UUID held whole elsewhere, no learned value named", heldAt(recA, recB, recC, recD), nil, false},
		{"nothing held elsewhere, each UUID learned as one value", heldNowhere, learnedAs(recA, recB, recC, recD), false},
	} {
		t.Run(m.name, func(t *testing.T) { check(t, m.held, m.of, m.uuids) })
	}
}

// A run's value spans: the shortest stretch around a word that its mock
// holds whole elsewhere, that holds no shorter such stretch, and whose words
// learning names one value. Every other word is its own span: with no
// stretch held, no evidence, a held stretch no one learned value names, a
// word no held stretch covers (never the words around it), or a recorded
// text that does not hold the run's words.
func TestWordRuns_AStretchItsMockHoldsElsewhereIsAValueOfItsOwn(t *testing.T) {
	const cid, app = "c9bf9e57-1685-4c89-bafb-ff5af830be8a", "6f9619ff-8b86-4011-b42d-00c04fc964ff"
	runs := func(recorded string, elsewhere func(string) bool, of func(string, int) string) []Run {
		rw := wordsOf(recorded)
		ws := make([]WordBind, len(rw))
		for i, w := range rw {
			ws[i] = WordBind{Text: "body.key", Word: i, Live: "L" + w, Recorded: w, RecordedText: recorded, Elsewhere: elsewhere}
			if of != nil {
				ws[i].Of = of(recorded, i)
			}
		}
		return WordRuns(ws)
	}
	span := func(r Run) string { return r.Recorded + "@" + strconv.Itoa(r.At) }
	cids := []string{cid + "@0", cid + "@1", cid + "@2", cid + "@3", cid + "@4"}
	cidAlone := []string{"c9bf9e57@0", "1685@0", "4c89@0", "bafb@0", "ff5af830be8a@0"}
	appAlone := []string{"6f9619ff@0", "8b86@0", "4011@0", "b42d@0", "00c04fc964ff@0"}
	cat := func(xs ...[]string) []string {
		var out []string
		for _, x := range xs {
			out = append(out, x...)
		}
		return out
	}
	cases := []struct {
		name, recorded string
		held           []string // held whole elsewhere, and learned as one value each
		unnamed        bool     // learning names no word's value
		none           bool     // no evidence at all (no Elsewhere)
		want           []string
	}{
		// No part of the text held whole elsewhere shows no value: each word
		// is its own (main's word by word check).
		{name: "nothing held elsewhere: each word alone", recorded: cid + "/3", want: cat(cidAlone, []string{"3@0"})},
		{name: "the text whole held elsewhere: the run whole", recorded: cid + "/3", held: []string{cid + "/3"}, want: []string{"c9bf9e57-1685-4c89-bafb-ff5af830be8a-3@0", "c9bf9e57-1685-4c89-bafb-ff5af830be8a-3@1", "c9bf9e57-1685-4c89-bafb-ff5af830be8a-3@2", "c9bf9e57-1685-4c89-bafb-ff5af830be8a-3@3", "c9bf9e57-1685-4c89-bafb-ff5af830be8a-3@4", "c9bf9e57-1685-4c89-bafb-ff5af830be8a-3@5"}},
		// Held whole is no proof of one value ("<B>/3" may be an id, or a
		// cid beside a count): with no learned value named, each word alone.
		{name: "the text whole held elsewhere, no learned value: each word alone", recorded: cid + "/3", held: []string{cid + "/3"}, unnamed: true, want: cat(cidAlone, []string{"3@0"})},
		{name: "the cid held elsewhere: the word beside it alone", recorded: cid + "/3", held: []string{cid}, want: cat(cids, []string{"3@0"})},
		// A word no held stretch covers is its own value, never read with the
		// words around it.
		{name: "the cid held elsewhere: the appId's words each alone", recorded: cid + "/" + app, held: []string{cid}, want: cat(cids, appAlone)},
		{name: "the cid held elsewhere: two words after it each alone", recorded: cid + "/x/y", held: []string{cid}, want: cat(cids, []string{"x@0", "y@0"})},
		// The whole held beside the appId is two values: the cid's words, no
		// held stretch of their own, are each alone.
		{name: "a stretch holding a shorter held one is two values", recorded: cid + "/" + app, held: []string{cid + "/" + app, app}, want: cat(cidAlone, []string{app + "@0", app + "@1", app + "@2", app + "@3", app + "@4"})},
		// Two held stretches overlap at "cd", which learning names a word of
		// the shorter: the longer is not one value.
		{name: "the shortest stretch held", recorded: "ab/cd/ef/gh", held: []string{"ab/cd", "cd/ef/gh"}, want: []string{"ab-cd@0", "ab-cd@1", "ef@0", "gh@0"}},
		// A held stretch inside the whole, at its end, its start or apart
		// from the end words, makes the whole two values.
		{name: "the whole held beside the cid after it", recorded: "3/" + cid, held: []string{cid, "3/" + cid}, want: cat([]string{"3@0"}, cids)},
		{name: "the whole held beside the cid before it", recorded: cid + "/3", held: []string{cid, cid + "/3"}, want: cat(cids, []string{"3@0"})},
		{name: "the whole held beside the cid after two words", recorded: "x/y/" + cid, held: []string{cid, "x/y/" + cid}, want: cat([]string{"x@0", "y@0"}, cids)},
		{name: "the whole held beside the cid before two words", recorded: cid + "/x/y", held: []string{cid, cid + "/x/y"}, want: cat(cids, []string{"x@0", "y@0"})},
		{name: "no evidence: each word alone", recorded: "ab/3", held: []string{"ab/3"}, none: true, want: []string{"ab@0", "3@0"}},
		{name: "the cid held elsewhere: the word before it alone", recorded: "3/" + cid, held: []string{cid}, want: cat([]string{"3@0"}, cids)},
		{name: "a run longer than spans are read in: each word alone", recorded: strings.Repeat("ab/", maxSpanRun) + "3", held: []string{"ab/ab", "3"}, want: []string{"ab@0", "ab@0"}},
	}
	for _, c := range cases {
		var got []Run
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("%s: WordRuns panicked: %v", c.name, r)
				}
			}()
			elsewhere, of := heldAt(c.held...), learnedAs(c.held...)
			if c.unnamed {
				of = nil
			}
			if c.none {
				elsewhere = nil
			}
			got = runs(c.recorded, elsewhere, of)
		}()
		if got == nil {
			continue
		}
		for i := range c.want {
			if span(got[i]) != c.want[i] {
				t.Errorf("%s: word %d spans %s, want %s", c.name, i, span(got[i]), c.want[i])
			}
		}
	}
	// A text whose words differ from the run's ("zz/3"), or that ends before
	// the run does, at its first word ("3") or after a first word it holds
	// ("ab"): each word alone, though its mock holds the text whole elsewhere
	// and learning names it one value.
	for _, text := range []string{"zz/3", "3", "ab"} {
		ws := []WordBind{{Text: "body.key", Word: 0, Live: "x", Recorded: "ab", RecordedText: text, Elsewhere: heldAt(text), Of: text}, {Text: "body.key", Word: 1, Live: "7", Recorded: "3", RecordedText: text, Elsewhere: heldAt(text), Of: text}}
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("recorded text %q: WordRuns panicked: %v", text, r)
				}
			}()
			if got := WordRuns(ws); got[0].Recorded != "ab" || got[1].Recorded != "3" {
				t.Errorf("recorded text %q does not hold the run's words: %+v, want each word alone", text, got)
			}
		}()
	}
}

// A one-word value beside a different bound UUID at each of two places, where
// two values were recorded ("<P>/7" and "<Q>/7" where "<B>/3" and "<D>/5"
// were), and each mock holds its UUID whole elsewhere (a cid field): the
// UUIDs are other values, so "7" is its own value and refused, as main
// refuses it, in one call or two, when only one of the mocks shows it, and
// when neither holds any part of its text whole elsewhere.
// Two appIds beside cids held elsewhere and learned as one value each, a
// group of the appIds equal by chance, still bind; named no learned value,
// they are refused, as main refuses them.
func TestTable_OneWordBesideAnotherHeldValueIsItsOwnValue(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	const (
		recA  = "6f9619ff-8b86-4011-b42d-00c04fc964ff"
		recB  = "c9bf9e57-1685-4c89-bafb-ff5af830be8a"
		recC  = "1e2d3c4b-5a69-4788-9766-554433221100"
		recD  = "aabbccdd-eeff-4a11-9b22-334455667788"
		liveX = "7d444840-9dc0-41a3-a15b-5b4c7a3f9e21"
		liveZ = "e3b0c442-98fc-41a3-8d2e-1c5a4e9b7f30"
		liveP = "11112222-3333-4444-8555-666677778888"
		liveQ = "9999aaaa-bbbb-4ccc-9ddd-eeeeffff0000"
	)
	bind := func(calls ...[]Binding) bool {
		var tb Table
		for _, call := range calls {
			if _, _, _, ok := tb.BindAll(tb.Gen(), call); !ok {
				return false
			}
		}
		return true
	}
	k := func(held func(string) bool) []Binding {
		return textBindsHeld(t, held, [4]string{"body.k1", "k", liveP + "/7", recB + "/3"})
	}
	c := func(held func(string) bool) []Binding {
		return textBindsHeld(t, held, [4]string{"body.k2", "c", liveQ + "/7", recD + "/5"})
	}
	for _, x := range []struct {
		name   string
		kh, ch func(string) bool
	}{
		{"both mocks hold their UUID", heldAt(recB), heldAt(recD)},
		{"only the first", heldAt(recB), heldNowhere},
		{"only the second", heldNowhere, heldAt(recD)},
		// The bare probe: no part of either text held whole elsewhere.
		{"neither mock holds any part of its text", heldNowhere, heldNowhere},
		// The whole text held too ("<B>/3" in a field of its own beside
		// "<B>") is two values, never one: "3" stays its own.
		{"both mocks hold their UUID and their whole text", heldAt(recB, recB+"/3"), heldAt(recD, recD+"/5")},
	} {
		if bind(append(k(x.kh), c(x.ch)...)) {
			t.Errorf("%s, in one call: \"<P>/7\" and \"<Q>/7\" bound where \"<B>/3\" and \"<D>/5\" were recorded", x.name)
		}
		if bind(k(x.kh), c(x.ch)) {
			t.Errorf("%s, in two calls: \"<P>/7\" and \"<Q>/7\" bound where \"<B>/3\" and \"<D>/5\" were recorded", x.name)
		}
	}
	learned := learnedAs(recA, recB, recC, recD)
	a := textBindsNamed(t, heldAt(recB, recA), learned, [4]string{"body.k1", "k", liveP + "/" + liveX, recB + "/" + recA})
	b := textBindsNamed(t, heldAt(recD, recC), learned, [4]string{"body.k2", "c", liveQ + "/" + liveZ, recD + "/" + recC})
	if !bind(a, b) || !bind(append(append([]Binding(nil), a...), b...)) {
		t.Errorf("two appIds beside cids held elsewhere, a group equal by chance (41a3), did not bind")
	}
	ua := textBindsHeld(t, heldAt(recB, recA), [4]string{"body.k1", "k", liveP + "/" + liveX, recB + "/" + recA})
	ub := textBindsHeld(t, heldAt(recD, recC), [4]string{"body.k2", "c", liveQ + "/" + liveZ, recD + "/" + recC})
	if bind(ua, ub) || bind(append(append([]Binding(nil), ua...), ub...)) {
		t.Errorf("two appIds beside cids held elsewhere but named no learned value, a group equal by chance (41a3), bound; main refuses the group")
	}
	copied := textBindsNamed(t, heldAt(recD, recC), learned, [4]string{"body.k2", "c", liveQ + "/" + liveX, recD + "/" + recC})
	if bind(a, copied) {
		t.Errorf("one appId beside two cids held elsewhere, where two were recorded, bound")
	}
	// The whole marker held as well changes neither: the cid and the appId
	// stay two values.
	aw := textBindsNamed(t, heldAt(recB, recA, recB+"/"+recA), learned, [4]string{"body.k1", "k", liveP + "/" + liveX, recB + "/" + recA})
	bw := textBindsNamed(t, heldAt(recD, recC, recD+"/"+recC), learned, [4]string{"body.k2", "c", liveQ + "/" + liveZ, recD + "/" + recC})
	cw := textBindsNamed(t, heldAt(recD, recC, recD+"/"+recC), learned, [4]string{"body.k2", "c", liveQ + "/" + liveX, recD + "/" + recC})
	if !bind(aw, bw) {
		t.Errorf("two appIds beside cids, each held elsewhere and the whole marker too, a group equal by chance, did not bind")
	}
	if bind(aw, cw) {
		t.Errorf("one appId beside two cids, each held elsewhere and the whole marker too, where two were recorded, bound")
	}
}

// One word sent at two places where two values were recorded, beside words
// of other values, in shapes main's word by word check refuses: refused, in
// one call and in two, under the switch. A word no held stretch covers is
// its own value, never read with the unheld words around it ("<P>/<X>/7"
// with only the cid held: "7" is not read with the appId's words), and a
// held stretch is one value only where learning names its words one value:
// "<B>/3" held whole in a field of its own may be one id or a cid beside a
// count, and the recording does not say which. The control holds the cid
// and the appId too, and the last row names "<B>/3" one learned value.
func TestTable_OneWordWhereTwoWereRecordedBesideOtherValuesIsRefused(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	const (
		recA  = "6f9619ff-8b86-4011-b42d-00c04fc964ff"
		recB  = "c9bf9e57-1685-4c89-bafb-ff5af830be8a"
		recC  = "1e2d3c4b-5a69-4788-9766-554433221100"
		recD  = "aabbccdd-eeff-4a11-9b22-334455667788"
		liveX = "7d444840-9dc0-41a3-a15b-5b4c7a3f9e21"
		liveY = "e3b0c442-98fc-4b11-8d2e-1c5a4e9b7f30"
		liveP = "11112222-3333-4444-8555-666677778888"
		liveQ = "9999aaaa-bbbb-4ccc-9ddd-eeeeffff0000"
	)
	bind := func(calls ...[]Binding) bool {
		var tb Table
		for _, call := range calls {
			if _, _, _, ok := tb.BindAll(tb.Gen(), call); !ok {
				return false
			}
		}
		return true
	}
	type side struct {
		held           []string
		live, recorded string
	}
	for _, c := range []struct {
		name string
		k, c side
		of   bool // learning names each held value one value
	}{
		{"the cid held, the appId not, the word after the appId", side{[]string{recB}, liveP + "/" + liveX + "/7", recB + "/" + recA + "/3"}, side{[]string{recD}, liveQ + "/" + liveY + "/7", recD + "/" + recC + "/5"}, false},
		{"the same, learned", side{[]string{recB}, liveP + "/" + liveX + "/7", recB + "/" + recA + "/3"}, side{[]string{recD}, liveQ + "/" + liveY + "/7", recD + "/" + recC + "/5"}, true},
		{"the word between an unheld appId and a held cid", side{[]string{recB}, liveX + "/7/" + liveP, recA + "/3/" + recB}, side{[]string{recD}, liveY + "/7/" + liveQ, recC + "/5/" + recD}, false},
		{"the same, learned", side{[]string{recB}, liveX + "/7/" + liveP, recA + "/3/" + recB}, side{[]string{recD}, liveY + "/7/" + liveQ, recC + "/5/" + recD}, true},
		{"only the whole text held", side{[]string{recB + "/3"}, liveP + "/7", recB + "/3"}, side{[]string{recD + "/5"}, liveQ + "/7", recD + "/5"}, false},
		{"two unheld one-word values beside a held cid", side{[]string{recB}, "11/7/" + liveP, "22/3/" + recB}, side{[]string{recD}, "44/7/" + liveQ, "55/5/" + recD}, false},
		{"the same, learned", side{[]string{recB}, "11/7/" + liveP, "22/3/" + recB}, side{[]string{recD}, "44/7/" + liveQ, "55/5/" + recD}, true},
		{"control: the cid and the appId held", side{[]string{recB, recA}, liveP + "/" + liveX + "/7", recB + "/" + recA + "/3"}, side{[]string{recD, recC}, liveQ + "/" + liveY + "/7", recD + "/" + recC + "/5"}, true},
	} {
		binds := func(s side, text, prefix string) []Binding {
			var of func(string, int) string
			if c.of {
				of = learnedAs(s.held...)
			}
			return textBindsNamed(t, heldAt(s.held...), of, [4]string{text, prefix, s.live, s.recorded})
		}
		k, cc := binds(c.k, "body.k1", "k"), binds(c.c, "body.k2", "c")
		if bind(k, cc) {
			t.Errorf("%s, in two calls: one word bound where two values were recorded", c.name)
		}
		if bind(append(append([]Binding(nil), k...), cc...)) {
			t.Errorf("%s, in one call: one word bound where two values were recorded", c.name)
		}
	}
	// Named one learned value, "<B>/3" is one value whose other words differ
	// live and as recorded: "7" is equal by chance, and binds.
	k := textBindsNamed(t, heldAt(recB+"/3"), learnedAs(recB+"/3"), [4]string{"body.k1", "k", liveP + "/7", recB + "/3"})
	cc := textBindsNamed(t, heldAt(recD+"/5"), learnedAs(recD+"/5"), [4]string{"body.k2", "c", liveQ + "/7", recD + "/5"})
	if !bind(k, cc) || !bind(append(append([]Binding(nil), k...), cc...)) {
		t.Errorf("\"<P>/7\" and \"<Q>/7\", each learned as one value, did not bind")
	}
}
