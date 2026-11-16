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

// checkGuard sees every word any row's Decide refuses (TestMain registers
// the order guarded): each must name a check of refusalChecks. It keeps
// the notes of the guard row's own tests (Test "sink-row-..."), which no
// other row uses. It is no sink: Decide notes nothing to one (NoteDecided).
var checkGuard guardSink

// guarded is order with each word it refuses noted to checkGuard.
func guarded(order func(Input) []Decision) func(Input) []Decision {
	return func(in Input) []Decision {
		ds := order(in)
		for _, d := range ds {
			if d.Refusal != nil {
				checkGuard.Refused(refusalNote(in.Rec, d, d.Refusal))
			}
		}
		return ds
	}
}

// noted is what NoteDecided(rec, ds) notes to a sink of its own; the sink
// is unset again after.
func noted(rec Shape, ds []Decision) []RefusalNote {
	var sink noteSink
	SetRefusalSink(&sink)
	defer SetRefusalSink(nil)
	NoteDecided(rec, ds)
	return sink.all()
}

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

// noteSink is a sink of a row's own, which keeps every note it is told of.
type noteSink struct {
	mu    sync.Mutex
	notes []RefusalNote
}

func (s *noteSink) Refused(n RefusalNote) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.notes = append(s.notes, n)
}

func (s *noteSink) all() []RefusalNote {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.notes)
}

func (s *noteSink) of(test string) []RefusalNote {
	var out []RefusalNote
	for _, n := range s.all() {
		if n.Test == test {
			out = append(out, n)
		}
	}
	return out
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
// runs (same-token). NoteDecided notes each refused word to the sink once,
// by that name.
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
		ns := noted(in.Rec, ds)
		if len(ns) != n || slices.ContainsFunc(ns, func(x RefusalNote) bool { return x.Check != r.check || x.Kind != RefusalConflict }) {
			t.Errorf("%s: %d refused, noted %+v; want each noted once, a conflict by %q", r.what, n, ns, r.check)
		}
	}
}

// Decide notes nothing to the sink; NoteDecided notes each word of a
// decision Decide refused, once, and no word it learned. A note holds
// neither value: each by its type, length, classes of characters and a hash
// keyed with this process's key, so one value hashes alike in two notes and
// two values apart, and no plain hash of the value stands in it. With no
// sink set, nothing is noted and nothing panics.
func TestNoteDecidedNotesEachRefusedWordOnceByShape(t *testing.T) {
	const ms = time.Millisecond
	held := oServed("mock-4", false, false, Response, oText("body.id", "A7K29QXM"), "B4N08ZRT", ms)
	in := func(test, rec, live string, earlier []Earlier) Input {
		in := oInput("body.items[3].id", rec, live, earlier, nil)
		in.Test = test
		in.PlaceOf = func(l Leaf, answer bool) (string, bool) { return VarPlace(l, true) }
		return in
	}
	refused := func(ds []Decision) (n int) {
		for _, d := range ds {
			if d.Outcome == OutcomeRefused {
				n++
			}
		}
		return n
	}
	ina, inb := in("sink-row-a", "A7K29QXM", "Q8M31WPN", held), in("sink-row-b", "A7K29QXM", "Q8M31WPN", held)
	var sink noteSink
	SetRefusalSink(&sink)
	a, b := Decide(ina), Decide(inb)
	SetRefusalSink(nil)
	if refused(a) == 0 || len(sink.all()) != 0 {
		t.Fatalf("Decide refused %d words and noted %+v; want refusals, none noted by Decide itself", refused(a), sink.all())
	}
	na, nb := noted(ina.Rec, a), noted(inb.Rec, b)
	if len(na) != refused(a) || len(nb) != refused(b) {
		t.Fatalf("refused %d and %d words, noted %d and %d; want each refused word noted once", refused(a), refused(b), len(na), len(nb))
	}
	n := na[0]
	if n.Test != "sink-row-a" || n.Field != "body.items[].id" || n.Recorded.Type != "string" || n.Recorded.Len != 8 || n.Recorded.Classes != "digit+upper" || n.Full.Len != 8 {
		t.Errorf("note %+v; want test sink-row-a, field body.items[].id, each value a string of 8 digits and capitals", n)
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
	inl := in("sink-row-learned", "3", "131", oServed("mock-4", true, true, Response, oText("body.n", "3"), "131", ms))
	learned := Decide(inl)
	if got := noted(inl.Rec, learned); refused(learned) != 0 || len(learned) == 0 || len(got) != 0 {
		t.Fatalf("a learned word: %s, noted %+v; want no refusal and no note", oDescribe(learned), got)
	}
	// Each refused word of one decision, in its order, and none it learned.
	ds := []Decision{a[0], learned[0], {Recorded: "B4N08ZRT", Full: "Z1X22CVB", Outcome: OutcomeRefused,
		Refusal: &Refusal{Kind: RefusalUnknown, Check: "state-unknown", Test: "sink-row-two", Field: "body.items[3].id"}}}
	ds[0].Refusal = &Refusal{Kind: RefusalConflict, Check: "own-moved", Test: "sink-row-two", Field: "body.items[3].id"}
	if got := noted(ina.Rec, ds); len(got) != 2 || got[0].Check != "own-moved" || got[1].Check != "state-unknown" || got[1].Full != shapeOf("string", "Z1X22CVB") {
		t.Errorf("a decision with two refused words and a learned one: noted %+v; want own-moved then state-unknown, each once", got)
	}
	func() {
		defer func() {
			if p := recover(); p != nil {
				t.Errorf("with no sink set, NoteDecided panicked: %v", p)
			}
		}()
		NoteDecided(ina.Rec, a)
	}()
	if got := sink.all(); len(got) != 0 {
		t.Errorf("with no sink set, a note was taken: %+v", got)
	}
}

// An adapter that decides one call more than once and keeps one outcome
// (tied candidates tried in turn) notes only the decision it keeps: a try
// that refused, then a kept one that learned, notes nothing; a call every
// try refused notes the kept try's refusals once, never the others'.
func TestNoteDecidedNotesOnlyTheDecisionKept(t *testing.T) {
	const ms = time.Millisecond
	try := func(test, live string, earlier []Earlier) (Input, []Decision) {
		in := oInput("body.items[3].id", "A7K29QXM", live, earlier, nil)
		in.Test = test
		in.PlaceOf = func(l Leaf, answer bool) (string, bool) { return VarPlace(l, true) }
		return in, Decide(in)
	}
	var sink noteSink
	SetRefusalSink(&sink)
	defer SetRefusalSink(nil)
	held := oServed("mock-4", false, false, Response, oText("body.id", "A7K29QXM"), "B4N08ZRT", ms)
	_, first := try("sink-row-tied", "Q8M31WPN", held)
	kept, second := try("sink-row-tied", "Q8M31WPN", oServed("mock-5", true, true, Response, oText("body.id", "A7K29QXM"), "Q8M31WPN", ms))
	if !slices.ContainsFunc(first, func(d Decision) bool { return d.Refusal != nil }) || slices.ContainsFunc(second, func(d Decision) bool { return d.Refusal != nil }) {
		t.Fatalf("tries: %s then %s; want the first refused and the second learned", oDescribe(first), oDescribe(second))
	}
	NoteDecided(kept.Rec, second)
	if got := sink.all(); len(got) != 0 {
		t.Errorf("a refused try set aside for a tied one that learned: noted %+v; want nothing", got)
	}
	_, other := try("sink-row-every", "Q8M31WPN", held)
	last, reported := try("sink-row-every", "R2D44TYU", held)
	NoteDecided(last.Rec, reported)
	got := sink.all()
	if n := slices.IndexFunc(reported, func(d Decision) bool { return d.Refusal != nil }); n < 0 || len(other) == 0 || len(got) != len(slices.DeleteFunc(slices.Clone(reported), func(d Decision) bool { return d.Refusal == nil })) || slices.ContainsFunc(got, func(n RefusalNote) bool { return n.Full != shapeOf("string", "R2D44TYU") }) {
		t.Errorf("a call every try refused: noted %+v; want the kept try's refusals once, none of the other's", got)
	}
}

// Every row's Decide runs under checkGuard (TestMain), which sees each word
// it refuses, so a refusal that names no stable check fails the run.
func TestEveryRowsRefusalsReachTheCheckGuard(t *testing.T) {
	in := oInput("body.items[3].id", "A7K29QXM", "Q8M31WPN", oServed("mock-4", false, false, Response, oText("body.id", "A7K29QXM"), "B4N08ZRT", time.Millisecond), nil)
	in.Test = "sink-row-guard"
	in.PlaceOf = func(l Leaf, answer bool) (string, bool) { return VarPlace(l, true) }
	n := 0
	for _, d := range Decide(in) {
		if d.Refusal != nil {
			n++
		}
	}
	if got := checkGuard.of("sink-row-guard"); n == 0 || len(got) != n {
		t.Errorf("Decide refused %d words; the guard saw %+v; want each one", n, got)
	}
}

// An adapter that refuses on its own side a word Decide learned notes it
// once (NoteRefusal), by the adapter's check and the word's shapes: the
// leaf's type from the recorded shape, each value's length, classes and
// keyed hash taken from the decision, as the run held it, never from the
// refusal, which hides a secret's values as "***". The hash is the one
// Decide's own note gives the value. With no sink set, nothing is noted.
func TestNoteRefusalNotesAnAdaptersRefusalByShape(t *testing.T) {
	const field = "body.items[3].token"
	rec := Shape{Leaves: map[string]Leaf{field: oText(field, "A7K29QXM")}}
	d := Decision{Recorded: "A7K29QXM", Full: "Q8M31WPN", Outcome: OutcomeValue}
	rf := Refusal{Kind: RefusalCannotCheck, Check: "adapter-branch", Test: "sink-row-adapter", Mock: "mock-9", Field: field,
		Recorded: "***", Full: "***", Why: `it holds "***"`, Places: []string{"mock-2 body.items[0].id"}}
	var sink noteSink
	SetRefusalSink(&sink)
	defer SetRefusalSink(nil)
	NoteRefusal(rec, d, rf)
	ns := sink.of("sink-row-adapter")
	if len(ns) != 1 {
		t.Fatalf("noted %+v; want the adapter's refusal noted once", ns)
	}
	n := ns[0]
	if n.Check != "adapter-branch" || n.Kind != RefusalCannotCheck || n.Mock != "mock-9" || n.Field != "body.items[].token" ||
		!slices.Equal(n.Places, []string{"mock-2 body.items[].id"}) {
		t.Errorf("note %+v; want the adapter's check and kind, its field and places by shape", n)
	}
	for _, c := range []struct {
		got  ValueShape
		want string
	}{{n.Recorded, "A7K29QXM"}, {n.Full, "Q8M31WPN"}} {
		if c.got != shapeOf("string", c.want) || c.got.Len != 8 || c.got.Classes != "digit+upper" {
			t.Errorf("value shape %+v; want %+v, of the decision's value, not the refusal's ***", c.got, shapeOf("string", c.want))
		}
	}
	js, _ := json.Marshal(n)
	for _, v := range []string{"A7K29QXM", "Q8M31WPN", "***"} {
		if strings.Contains(string(js), v) {
			t.Errorf("the note holds %q: %s", v, js)
		}
	}
	SetRefusalSink(nil)
	func() {
		defer func() {
			if p := recover(); p != nil {
				t.Errorf("with no sink set, NoteRefusal panicked: %v", p)
			}
		}()
		NoteRefusal(rec, d, rf)
	}()
	SetRefusalSink(&sink)
	if got := sink.of("sink-row-adapter"); len(got) != 1 {
		t.Errorf("with no sink set, a note was taken: %+v", got)
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
