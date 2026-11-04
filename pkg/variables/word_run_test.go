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
// variables prefix0..prefix4, its runs filled (WordRuns).
func uuidBinds(t *testing.T, prefix, field, live, recorded string, first int) []Binding {
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
		ws[i] = WordBind{Text: "body." + field, Word: first + i, Live: lw[i], Recorded: rw[i], RecordedText: text, Elsewhere: heldNowhere}
	}
	runs := WordRuns(ws)
	out := make([]Binding, len(lw))
	for i := range lw {
		out[i] = runs[i].On(Binding{Name: prefix + strconv.Itoa(i), Value: lw[i], Recorded: rw[i], Place: "body." + field + "#" + strconv.Itoa(first+i)})
	}
	return out
}

// heldNowhere is a mock that holds no stretch of a run whole elsewhere: each
// run is one value.
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
		ws[i].RecordedText, ws[i].Elsewhere = recorded[ws[i].Text], heldNowhere
	}
	runs := WordRuns(ws)
	want := []Run{{"a-b-c", "A-B-C", 2}, {"a-b-c", "A-B-C", 0}, {"z", "Z", 0}, {"a-b-c", "A-B-C", 1}, {}, {"e", "E", 0}, {"y", "Y", 0}, {"x", "X", 0}}
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

// textBinds is one call's bindings at every word of each text's value, live
// and recorded, as variables prefix0, prefix1..., their runs filled.
func textBinds(t *testing.T, texts ...[4]string) []Binding {
	t.Helper()
	return textBindsHeld(t, heldNowhere, texts...)
}

// textBindsHeld is textBinds in a mock that holds whole elsewhere what
// elsewhere says.
func textBindsHeld(t *testing.T, elsewhere func(string) bool, texts ...[4]string) []Binding {
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
			ws = append(ws, WordBind{Text: text, Word: i, Live: lw[i], Recorded: rw[i], RecordedText: recorded, Elsewhere: elsewhere})
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
	// One UUID at two places where two were recorded, each beside another
	// bound UUID that differs live: the rest of the shared UUID is copied.
	t.Run("first in each text, beside different seconds, in one call", func(t *testing.T) {
		refused(t, textBinds(t, [4]string{"body.k1", "k", liveX + "/" + liveY, recA + "/" + recB}, [4]string{"body.k2", "c", liveX + "/" + liveZ, recC + "/" + recD}))
	})
	t.Run("first in each text, beside different seconds, in two calls", func(t *testing.T) {
		refused(t, textBinds(t, [4]string{"body.k1", "k", liveX + "/" + liveY, recA + "/" + recB}), textBinds(t, [4]string{"body.k2", "c", liveX + "/" + liveZ, recC + "/" + recD}))
	})
	t.Run("second in each text, beside different firsts, in one call", func(t *testing.T) {
		refused(t, textBinds(t, [4]string{"body.k1", "k", liveP + "/" + liveX, recB + "/" + recA}, [4]string{"body.k2", "c", liveQ + "/" + liveX, recD + "/" + recC}))
	})
	t.Run("second in each text, beside different firsts, in two calls", func(t *testing.T) {
		refused(t, textBinds(t, [4]string{"body.k1", "k", liveP + "/" + liveX, recB + "/" + recA}), textBinds(t, [4]string{"body.k2", "c", liveQ + "/" + liveX, recD + "/" + recC}))
	})
	t.Run("both halves equal", func(t *testing.T) {
		refused(t, textBinds(t, [4]string{"body.k1", "k", liveX + "/" + liveY, recA + "/" + recB}, [4]string{"body.k2", "c", liveX + "/" + liveY, recC + "/" + recD}))
	})
	chance := func() []Binding {
		return textBinds(t, [4]string{"body.key", "k", liveX + "/" + liveY, recA + "/" + recB}, [4]string{"body.c", "c", liveZ, recC})
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

// A run's value spans: the shortest stretch around a word that its mock
// holds whole elsewhere and that holds no shorter such stretch, else the
// words no such stretch covers. With no
// evidence, or a recorded text that does not hold the run's words, a word is
// its own span.
func TestWordRuns_AStretchItsMockHoldsElsewhereIsAValueOfItsOwn(t *testing.T) {
	const cid, app = "c9bf9e57-1685-4c89-bafb-ff5af830be8a", "6f9619ff-8b86-4011-b42d-00c04fc964ff"
	runs := func(recorded string, elsewhere func(string) bool) []Run {
		rw := wordsOf(recorded)
		ws := make([]WordBind, len(rw))
		for i, w := range rw {
			ws[i] = WordBind{Text: "body.key", Word: i, Live: "L" + w, Recorded: w, RecordedText: recorded, Elsewhere: elsewhere}
		}
		return WordRuns(ws)
	}
	span := func(r Run) string { return r.Recorded + "@" + strconv.Itoa(r.At) }
	cases := []struct {
		name, recorded string
		elsewhere      func(string) bool
		want           []string
	}{
		{"nothing held elsewhere: the run whole", cid + "/3", heldNowhere, []string{"c9bf9e57-1685-4c89-bafb-ff5af830be8a-3@0", "c9bf9e57-1685-4c89-bafb-ff5af830be8a-3@1", "c9bf9e57-1685-4c89-bafb-ff5af830be8a-3@2", "c9bf9e57-1685-4c89-bafb-ff5af830be8a-3@3", "c9bf9e57-1685-4c89-bafb-ff5af830be8a-3@4", "c9bf9e57-1685-4c89-bafb-ff5af830be8a-3@5"}},
		{"the cid held elsewhere: the word beside it alone", cid + "/3", heldAt(cid), []string{cid + "@0", cid + "@1", cid + "@2", cid + "@3", cid + "@4", "3@0"}},
		{"the cid held elsewhere: the appId's words", cid + "/" + app, heldAt(cid), []string{cid + "@0", cid + "@1", cid + "@2", cid + "@3", cid + "@4", app + "@0", app + "@1", app + "@2", app + "@3", app + "@4"}},
		// The whole held beside the appId is two values: the cid's words are
		// the ones the appId leaves.
		{"a stretch holding a shorter held one is two values", cid + "/" + app, heldAt(cid+"/"+app, app), []string{cid + "@0", cid + "@1", cid + "@2", cid + "@3", cid + "@4", app + "@0", app + "@1", app + "@2", app + "@3", app + "@4"}},
		{"the shortest stretch held", "ab/cd/ef/gh", heldAt("ab/cd", "cd/ef/gh"), []string{"ab-cd@0", "ab-cd@1", "cd-ef-gh@1", "cd-ef-gh@2"}},
		// A held stretch inside the whole, at its end, its start or apart
		// from the end words, makes the whole two values.
		{"the whole held beside the cid after it", "3/" + cid, heldAt(cid, "3/"+cid), []string{"3@0", cid + "@0", cid + "@1", cid + "@2", cid + "@3", cid + "@4"}},
		{"the whole held beside the cid before it", cid + "/3", heldAt(cid, cid+"/3"), []string{cid + "@0", cid + "@1", cid + "@2", cid + "@3", cid + "@4", "3@0"}},
		{"the whole held beside the cid after two words", "x/y/" + cid, heldAt(cid, "x/y/"+cid), []string{"x-y@0", "x-y@1", cid + "@0", cid + "@1", cid + "@2", cid + "@3", cid + "@4"}},
		{"the whole held beside the cid before two words", cid + "/x/y", heldAt(cid, cid+"/x/y"), []string{cid + "@0", cid + "@1", cid + "@2", cid + "@3", cid + "@4", "x-y@0", "x-y@1"}},
		{"no evidence: each word alone", "ab/3", nil, []string{"ab@0", "3@0"}},
		{"the cid held elsewhere: the word before it alone", "3/" + cid, heldAt(cid), []string{"3@0", cid + "@0", cid + "@1", cid + "@2", cid + "@3", cid + "@4"}},
		{"a run longer than spans are read in: each word alone", strings.Repeat("ab/", maxSpanRun) + "3", heldNowhere, []string{"ab@0", "ab@0"}},
	}
	for _, c := range cases {
		var got []Run
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("%s: WordRuns panicked: %v", c.name, r)
				}
			}()
			got = runs(c.recorded, c.elsewhere)
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
	for _, text := range []string{"zz/3", "3"} {
		ws := []WordBind{{Text: "body.key", Word: 0, Live: "x", Recorded: "ab", RecordedText: text, Elsewhere: heldNowhere}, {Text: "body.key", Word: 1, Live: "7", Recorded: "3", RecordedText: text, Elsewhere: heldNowhere}}
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
// refuses it, in one call or two, and when only one of the mocks shows it.
// Two appIds beside cids held elsewhere, a group of the appIds equal by
// chance, still bind.
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
	a := textBindsHeld(t, heldAt(recB, recA), [4]string{"body.k1", "k", liveP + "/" + liveX, recB + "/" + recA})
	b := textBindsHeld(t, heldAt(recD, recC), [4]string{"body.k2", "c", liveQ + "/" + liveZ, recD + "/" + recC})
	if !bind(a, b) || !bind(append(append([]Binding(nil), a...), b...)) {
		t.Errorf("two appIds beside cids held elsewhere, a group equal by chance (41a3), did not bind")
	}
	copied := textBindsHeld(t, heldAt(recD, recC), [4]string{"body.k2", "c", liveQ + "/" + liveX, recD + "/" + recC})
	if bind(a, copied) {
		t.Errorf("one appId beside two cids held elsewhere, where two were recorded, bound")
	}
	// The whole marker held as well changes neither: the cid and the appId
	// stay two values.
	aw := textBindsHeld(t, heldAt(recB, recA, recB+"/"+recA), [4]string{"body.k1", "k", liveP + "/" + liveX, recB + "/" + recA})
	bw := textBindsHeld(t, heldAt(recD, recC, recD+"/"+recC), [4]string{"body.k2", "c", liveQ + "/" + liveZ, recD + "/" + recC})
	cw := textBindsHeld(t, heldAt(recD, recC, recD+"/"+recC), [4]string{"body.k2", "c", liveQ + "/" + liveX, recD + "/" + recC})
	if !bind(aw, bw) {
		t.Errorf("two appIds beside cids, each held elsewhere and the whole marker too, a group equal by chance, did not bind")
	}
	if bind(aw, cw) {
		t.Errorf("one appId beside two cids, each held elsewhere and the whole marker too, where two were recorded, bound")
	}
}
