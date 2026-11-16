package learn

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// refusalChecks is every check name a refusal of this package may carry
// (Refusal.Check). A name is stable: a row that sees one outside this list,
// or none, fails, so a check is renamed or added only here as well.
var refusalChecks = []string{
	"declared-clock-range", "declared-clock-no-span", "declared-clock-no-moment", "declared-clock-not-read",
	"declared-clock-only", "minted-not-one-value", "minted-form-changed", "minted-held-earlier", "minted-two-places",
	"own-instant-both-ways", "unread-earlier", "one-value-two-sources", "unread-before-call", "text-changed-shape",
	"signature-form", "word-added-or-dropped", "unpaired-unknown",
	// a conflict's first place
	"conflict", "held-both-runs", "same-token", "own-moved", "disproven",
	// an unknown word's first place
	"state-unknown", "past-budget", "glued", "given-glued", "moved", "outside-test", "late-call", "one-run",
	"set-unknown", "order-unknown", "swap-unknown",
	"another-scope", "encoded-another-scope", "encoded-unknown", "instant-unread", "instant-conflict",
	"instant-pick-state-unknown", "instant-pick", "instant-pick-recorded-after", "instant-pick-test-case",
	"instant-two-reads", "instant-late-two-sources", "instant-pick-outside-scope", "instant-another-scope",
	"instant-unknown", "instant-held-read", "instant-held-read-given", "no-clock-read", "copied-two-reads",
	"no-payload", "cannot-check", "cannot-check-all", "elsewhere-learned-nothing",
}

// checkGuard is the sink every row of the package runs under (TestMain):
// each word any row's Decide refuses must name a check of refusalChecks.
// It keeps the notes of the sink rows' own tests (Test "sink-row-..."),
// which no other row uses.
var checkGuard guardSink

type guardSink struct {
	mu    sync.Mutex
	bad   []string
	notes []RefusalNote
}

func (g *guardSink) Refused(n RefusalNote) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if !slices.Contains(refusalChecks, n.Check) && len(g.bad) < 20 {
		g.bad = append(g.bad, fmt.Sprintf("%s %s %s: check %q", n.Test, n.Mock, n.Field, n.Check))
	}
	if strings.HasPrefix(n.Test, "sink-row-") {
		g.notes = append(g.notes, n)
	}
}

// failed reports, after every row ran, the refusals that named no known check.
func (g *guardSink) failed() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, b := range g.bad {
		fmt.Fprintln(os.Stderr, "a refusal names no known check (refusalChecks):", b)
	}
	return len(g.bad) > 0
}

func (g *guardSink) of(test string) []RefusalNote {
	g.mu.Lock()
	defer g.mu.Unlock()
	var out []RefusalNote
	for _, n := range g.notes {
		if n.Test == test {
			out = append(out, n)
		}
	}
	return out
}

// A word refused for its first place names the branch that put the place
// there: one of the call's own test that held the value's whole token and
// moved (own-moved), a value the test gave, then gave another
// (held-both-runs), a whole token around the word the place kept in both
// runs (same-token). Each refused word is noted to the sink once, by that
// name.
func TestRefusalNamesTheCheckThatRefusedIt(t *testing.T) {
	const ms = time.Millisecond
	place := func(run, startUp bool, path, rec, live string) []Earlier {
		return oServed("mock-4", run, startUp, Response, oText(path, rec), live, ms)
	}
	rows := []struct {
		what      string
		earlier   []Earlier
		rec, live string
		word      [2]string
		check     string
	}{
		{"the call's own test held the short word whole and moved", place(false, false, "body.numIndexesAfter", "3", "2"), "3", "131", [2]string{"3", "131"}, "own-moved"},
		{"a short value the test gave, then gave another", []Earlier{TestGiven("test-2", "given:n", "3", "7")}, "3", "131", [2]string{"3", "131"}, "held-both-runs"},
		{"the call's test moved but kept the whole token", place(false, false, "body.tag", "blue-17 ready", "blue-17 waiting"), "blue-17", "red-42", [2]string{"17", "42"}, "same-token"},
	}
	for i, r := range rows {
		in := oInput("body.eventDetails.duration_ms", r.rec, r.live, r.earlier, nil)
		in.Test = fmt.Sprintf("sink-row-check-%d", i)
		in.PlaceOf = func(l Leaf, answer bool) (string, bool) { return VarPlace(l, true) }
		ds := Decide(in)
		d := oWord(t, ds, r.word[0], r.word[1])
		if d.Outcome != OutcomeRefused || d.Refusal == nil || d.Refusal.Kind != RefusalConflict {
			t.Errorf("%s: %s; want a conflict", r.what, oDescribe([]Decision{d}))
			continue
		}
		if d.Refusal.Check != r.check {
			t.Errorf("%s: check %q, want %q", r.what, d.Refusal.Check, r.check)
		}
		n := 0
		for _, d := range ds {
			if d.Outcome == OutcomeRefused {
				n++
			}
		}
		ns := checkGuard.of(in.Test)
		if len(ns) != n || slices.ContainsFunc(ns, func(x RefusalNote) bool { return x.Check != r.check || x.Kind != RefusalConflict }) {
			t.Errorf("%s: %d refused, noted %+v; want each noted once, a conflict by %q", r.what, n, ns, r.check)
		}
	}
}

// Decide notes each word it refuses once, as it returns, and no word it
// learns. A note holds neither value: each by its type, length, classes of
// characters and a hash keyed with this process's key, so one value hashes
// alike in two notes and two values apart, and no plain hash of the value
// stands in it. With no sink set, nothing is noted.
func TestRefusalSinkNotesEachRefusedWordOnceByShape(t *testing.T) {
	const ms = time.Millisecond
	held := oServed("mock-4", false, false, Response, oText("body.id", "A7K29QXM"), "B4N08ZRT", ms)
	run := func(test, rec, live string, earlier []Earlier) []Decision {
		in := oInput("body.items[3].id", rec, live, earlier, nil)
		in.Test = test
		in.PlaceOf = func(l Leaf, answer bool) (string, bool) { return VarPlace(l, true) }
		return Decide(in)
	}
	refused := func(ds []Decision) (n int) {
		for _, d := range ds {
			if d.Outcome == OutcomeRefused {
				n++
			}
		}
		return n
	}
	a := run("sink-row-a", "A7K29QXM", "Q8M31WPN", held)
	b := run("sink-row-b", "A7K29QXM", "Q8M31WPN", held)
	na, nb := checkGuard.of("sink-row-a"), checkGuard.of("sink-row-b")
	if refused(a) == 0 || len(na) != refused(a) || len(nb) != refused(b) {
		t.Fatalf("refused %d and %d words, noted %d and %d; want each refused word noted once", refused(a), refused(b), len(na), len(nb))
	}
	n := na[0]
	if n.Field != "body.items[].id" || n.Recorded.Type != "string" || n.Recorded.Len != 8 || n.Recorded.Classes != "digit+upper" || n.Full.Len != 8 {
		t.Errorf("note %+v; want field body.items[].id, each value a string of 8 digits and capitals", n)
	}
	if n.Recorded.Hash != nb[0].Recorded.Hash || n.Recorded.Hash == n.Full.Hash || len(n.Recorded.Hash) != 64 {
		t.Errorf("hashes %q (a) %q (b) %q (a's full); want one value alike in both notes and the two values apart", n.Recorded.Hash, nb[0].Recorded.Hash, n.Full.Hash)
	}
	plain := sha256.Sum256([]byte("A7K29QXM"))
	js, _ := json.Marshal(na)
	for _, v := range []string{"A7K29QXM", "Q8M31WPN", hex.EncodeToString(plain[:])} {
		if strings.Contains(string(js), v) {
			t.Errorf("a note holds %q: %s", v, js)
		}
	}
	// A word learned (bound to the place it followed) is no refusal.
	learned := run("sink-row-learned", "3", "131", oServed("mock-4", true, true, Response, oText("body.n", "3"), "131", ms))
	if got := checkGuard.of("sink-row-learned"); refused(learned) != 0 || len(got) != 0 {
		t.Errorf("a learned word: %s, noted %+v; want no refusal and no note", oDescribe(learned), got)
	}
	SetRefusalSink(nil)
	defer SetRefusalSink(&checkGuard)
	if refused(run("sink-row-none", "A7K29QXM", "Q8M31WPN", held)) == 0 || len(checkGuard.of("sink-row-none")) != 0 {
		t.Errorf("with no sink set, a note was taken: %+v", checkGuard.of("sink-row-none"))
	}
}

// A value's classes are read rune by rune, in their one order.
func TestRefusalValueShapeClasses(t *testing.T) {
	for v, want := range map[string]string{
		"": "", "42": "digit", "abc": "lower", "ABC": "upper", "a b": "lower+space", "x-1": "digit+lower+punct",
		"é9": "digit+other", "Ab 9.é": "digit+lower+upper+space+punct+other", "0z9Z~": "digit+lower+upper+punct",
	} {
		if got := shapeOf("string", v); got.Classes != want || got.Len != len(v) {
			t.Errorf("%q: classes %q, length %d; want %q, %d", v, got.Classes, got.Len, want, len(v))
		}
	}
	if got := pathShape("mock-3 body.a[12].b[0]"); got != "mock-3 body.a[].b[]" {
		t.Errorf("path shape %q", got)
	}
	for ty, want := range map[Type]string{String: "string", Number: "number", Bool: "bool", Null: "null", EmptyObj: "empty-object",
		EmptyArray: "empty-array", Keys: "keys", Instant: "instant", Type('?'): ""} {
		if got := typeName(ty); got != want {
			t.Errorf("type %c: %q, want %q", ty, got, want)
		}
	}
}
