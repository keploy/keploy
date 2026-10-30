package variables

import (
	"errors"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/templates"
)

func TestParse_ReadsEveryPartAndWritesItBack(t *testing.T) {
	cases := []struct {
		in   string
		want Place
		out  string // "" = in
	}{
		{in: "body.id", want: Place{Part: PartBody, Path: []templates.PathSeg{{Key: "id"}}}},
		{in: "body.items[0].id", want: Place{Part: PartBody, Path: []templates.PathSeg{{Key: "items"}, {Index: 0, IsIndex: true}, {Key: "id"}}}},
		{in: "body[3]", want: Place{Part: PartBody, Path: []templates.PathSeg{{Index: 3, IsIndex: true}}}},
		{in: `body["a.b"].c`, want: Place{Part: PartBody, Path: []templates.PathSeg{{Key: "a.b"}, {Key: "c"}}}},
		{in: `body["x[1]"]`, want: Place{Part: PartBody, Path: []templates.PathSeg{{Key: "x[1]"}}}},
		{in: `body["q\"t"]`, want: Place{Part: PartBody, Path: []templates.PathSeg{{Key: `q"t`}}}},
		{in: `body["plain"]`, want: Place{Part: PartBody, Path: []templates.PathSeg{{Key: "plain"}}}, out: "body.plain"},
		{in: "url.path[0]", want: Place{Part: PartPath, Index: 0}},
		{in: "url.path[12]", want: Place{Part: PartPath, Index: 12}},
		{in: "url.query.page", want: Place{Part: PartQuery, Name: "page"}},
		{in: "url.query.filter[id]", want: Place{Part: PartQuery, Name: "filter[id]"}},
		{in: "header.X-Request-Id", want: Place{Part: PartHeader, Name: "X-Request-Id"}},
	}
	for _, c := range cases {
		got, err := Parse(c.in)
		if err != nil {
			t.Fatalf("Parse(%q): %v", c.in, err)
		}
		if got.Part != c.want.Part || got.Index != c.want.Index || got.Name != c.want.Name || !samePath(got.Path, c.want.Path) {
			t.Fatalf("Parse(%q) = %+v, want %+v", c.in, got, c.want)
		}
		out := c.out
		if out == "" {
			out = c.in
		}
		if s := got.String(); s != out {
			t.Fatalf("Parse(%q).String() = %q, want %q", c.in, s, out)
		}
	}
}

func TestParse_RefusesWhatIsNotAPlace(t *testing.T) {
	for _, in := range []string{
		"", "body", "body.", "body..a", "body.a]", "body[x]", "body[-1]", "body[01]", "body[1",
		`body["a"`, `body["a"x]`, "url.path[]", "url.path[a]", "url.path[-1]", "url.path[01]",
		"url.query.", "header.", "query.page", "path[0]", "BODY.id",
	} {
		if p, err := Parse(in); err == nil {
			t.Fatalf("Parse(%q) = %+v, want an error", in, p)
		} else if !errors.Is(err, ErrPlace) {
			t.Fatalf("Parse(%q): %v does not wrap ErrPlace", in, err)
		}
	}
}

func TestSameJSON_IsExactOutsideNoise(t *testing.T) {
	cases := []struct {
		name      string
		rec, live string
		noise     []string
		want      bool
	}{
		{"same", `{"a":1,"b":"x"}`, `{"b":"x","a":1}`, nil, true},
		{"number by value", `{"a":1.50}`, `{"a":1.5}`, nil, true},
		{"value differs", `{"a":1,"b":"x"}`, `{"a":1,"b":"y"}`, nil, false},
		{"a field added", `{"a":1}`, `{"a":1,"b":2}`, nil, false},
		{"a field removed", `{"a":1,"b":2}`, `{"a":1}`, nil, false},
		{"empty object is not empty array", `{"a":{}}`, `{"a":[]}`, nil, false},
		{"array order counts", `{"a":[1,2]}`, `{"a":[2,1]}`, nil, false},
		{"array length counts", `{"a":[1]}`, `{"a":[1,1]}`, nil, false},
		{"a string is not a number", `{"a":"1"}`, `{"a":1}`, nil, false},
		{"noise lets the value change", `{"ts":1,"a":1}`, `{"ts":2,"a":1}`, []string{"body.ts"}, true},
		{"noise keeps the type", `{"ts":1}`, `{"ts":"1"}`, []string{"ts"}, false},
		{"noise covers its subtree", `{"m":{"x":1,"y":[1]}}`, `{"m":{"x":2,"y":[3]}}`, []string{"m"}, true},
		{"noise inside arrays", `{"l":[{"t":1,"k":1},{"t":2,"k":2}]}`, `{"l":[{"t":9,"k":1},{"t":8,"k":2}]}`, []string{"l[].t"}, true},
		{"array steps need not be written", `{"l":[{"t":1,"k":1}]}`, `{"l":[{"t":9,"k":1}]}`, []string{"l.t"}, true},
		{"noise in an array leaves the rest compared", `{"l":[{"t":1,"k":1}]}`, `{"l":[{"t":9,"k":2}]}`, []string{"l.t"}, false},
		{"a whole array as noise", `{"l":[1,2]}`, `{"l":[3]}`, []string{"l"}, true},
		{"noise is no substring", `{"userid":1}`, `{"userid":2}`, []string{"id"}, false},
		{"noise is read in lower case", `{"TS":1}`, `{"TS":2}`, []string{"body.ts"}, true},
		{"trailing text is not one document", `{"a":1}`, `{"a":1} x`, nil, false},
		{"not json", `{"a":1}`, `a=1`, nil, false},
	}
	for _, c := range cases {
		if got := SameJSON(c.rec, c.live, c.noise); got != c.want {
			t.Fatalf("%s: SameJSON(%s, %s, %v) = %v, want %v", c.name, c.rec, c.live, c.noise, got, c.want)
		}
	}
}

func TestTable_BindsOnce(t *testing.T) {
	var tb Table
	if _, ok := tb.Lookup("id"); ok {
		t.Fatal("an empty table holds id")
	}
	got, took := tb.Bind(Binding{Name: "id", Value: "a", Mock: "mock-1"})
	if !took || got.Value != "a" {
		t.Fatalf("first bind = %+v, %v", got, took)
	}
	got, took = tb.Bind(Binding{Name: "id", Value: "b", Mock: "mock-2"})
	if took || got.Value != "a" || got.Mock != "mock-1" {
		t.Fatalf("second bind = %+v, %v; want the first binding kept", got, took)
	}
	tb.Reset()
	if _, ok := tb.Lookup("id"); ok {
		t.Fatal("Reset kept id")
	}
}

func TestTable_ConcurrentBindsLeaveOneWinner(t *testing.T) {
	var tb Table
	var wg sync.WaitGroup
	wins := make(chan string, 16)
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(v string) {
			defer wg.Done()
			if _, took := tb.Bind(Binding{Name: "id", Value: v}); took {
				wins <- v
			}
		}(string(rune('a' + i)))
	}
	wg.Wait()
	close(wins)
	var n int
	var won string
	for v := range wins {
		n++
		won = v
	}
	if n != 1 {
		t.Fatalf("%d binds took, want 1", n)
	}
	if b, _ := tb.Lookup("id"); b.Value != won {
		t.Fatalf("the table holds %q, the bind that took was %q", b.Value, won)
	}
}

func TestSameJSONUnder_PatternsAndRedactedValues(t *testing.T) {
	var n Noise
	n.Add("body.ts", []string{`^\d{4}-`})
	if !SameJSONUnder(`{"ts":"2026-01-01"}`, `{"ts":"2027-02-02"}`, n) {
		t.Fatal("two values the pattern describes should be noise")
	}
	if SameJSONUnder(`{"ts":"2026-01-01"}`, `{"ts":"yesterday"}`, n) {
		t.Fatal("a live value the pattern does not describe is compared")
	}
	n = Noise{RecordedValueIsNoise: func(v string) bool { return v == "REDACTED" }}
	if !SameJSONUnder(`{"k":"REDACTED","a":1}`, `{"k":"sk_live_1","a":1}`, n) {
		t.Fatal("a redacted recorded value should stand for any live value")
	}
	if SameJSONUnder(`{"k":"REDACTED","a":1}`, `{"k":"sk_live_1","a":2}`, n) {
		t.Fatal("the rest of the body is still compared")
	}
	if SameJSONUnder(`{"k":"REDACTED"}`, `{"k":7}`, n) {
		t.Fatal("a redacted value keeps its type")
	}
}

func TestAnswerEditAndApplyEdits(t *testing.T) {
	body := `{"a":{"id":"r1"},"n":7,"s":"7"}`
	e1, why := AnswerEdit(body, []templates.PathSeg{{Key: "a"}, {Key: "id"}}, "r1", "l\"1")
	if why != "" {
		t.Fatal(why)
	}
	e2, why := AnswerEdit(body, []templates.PathSeg{{Key: "n"}}, "7", "8.5")
	if why != "" {
		t.Fatal(why)
	}
	out, ok := ApplyEdits(body, []Edit{e1, e2})
	if !ok || out != `{"a":{"id":"l\"1"},"n":8.5,"s":"7"}` {
		t.Fatalf("ApplyEdits = %s, %v", out, ok)
	}
	if _, why := AnswerEdit(body, []templates.PathSeg{{Key: "n"}}, "7", "x"); why == "" {
		t.Fatal("a value that is no number cannot stand at a number")
	}
	if _, why := AnswerEdit(body, []templates.PathSeg{{Key: "s"}}, "8", "9"); why == "" {
		t.Fatal("a place that does not hold the recorded value cannot answer")
	}
	if _, ok := ApplyEdits(body, []Edit{{Start: 1, End: 5}, {Start: 3, End: 8}}); ok {
		t.Fatal("overlapping edits were applied")
	}
}

// A place that holds another value is said by each value's length, the
// recording's decoded as the variable's is: two values of one length read as
// one length, and neither value is written. The recording's length was its
// raw JSON span's, quotes and escapes counted, and an equal length read as a
// changed one.
func TestAnswerEdit_AnotherValueIsSaidByDecodedLengths(t *testing.T) {
	body := `{"id":"vw\"yz","n":12345}`
	for _, tc := range []struct {
		key, recorded, want string
	}{
		{"id", "abcde", "another string there (5 bytes), not the variable's recorded value (5 bytes)"},
		{"n", "54321", "another number there (5 bytes), not the variable's recorded value (5 bytes)"},
	} {
		_, why := AnswerEdit(body, []templates.PathSeg{{Key: tc.key}}, tc.recorded, "x")
		if !strings.Contains(why, tc.want) {
			t.Errorf("%s: why = %q, want it to say %q", tc.key, why, tc.want)
		}
		for _, v := range []string{"vw", "yz", "12345", tc.recorded} {
			if strings.Contains(why, v) {
				t.Errorf("%s: why = %q writes the value %q", tc.key, why, v)
			}
		}
	}
}

// BindAll commits all or none: a name keeps its value, two variables recorded
// as two values never share a live value, and a refused batch leaves nothing.
func TestTable_BindAll(t *testing.T) {
	var tb Table
	if _, _, _, ok := tb.BindAll(tb.Gen(), []Binding{{Name: "a", Value: "1", Recorded: "r1"}, {Name: "b", Value: "2", Recorded: "r2"}}); !ok {
		t.Fatal("two fresh variables should bind")
	}
	for name, batch := range map[string][]Binding{
		"a name to another value":          {{Name: "c", Value: "3", Recorded: "r3"}, {Name: "a", Value: "9", Recorded: "r1"}},
		"another variable's value":         {{Name: "c", Value: "3", Recorded: "r3"}, {Name: "d", Value: "1", Recorded: "r4"}},
		"two values of one name in a call": {{Name: "c", Value: "3", Recorded: "r3"}, {Name: "c", Value: "4", Recorded: "r3"}},
		"one value of two names in a call": {{Name: "c", Value: "3", Recorded: "r3"}, {Name: "e", Value: "3", Recorded: "r5"}},
	} {
		if bad, with, _, ok := tb.BindAll(tb.Gen(), batch); ok {
			t.Fatalf("%s: bound", name)
		} else if bad.Name == "" || with.Name == "" {
			t.Fatalf("%s: no evidence (%+v, %+v)", name, bad, with)
		}
		if _, bound := tb.Lookup("c"); bound {
			t.Fatalf("%s: a refused batch left c bound", name)
		}
	}
	if _, _, _, ok := tb.BindAll(tb.Gen(), []Binding{{Name: "a", Value: "1", Recorded: "r1"}, {Name: "f", Value: "1", Recorded: "r1"}}); !ok {
		t.Fatal("a bound name at its value, and a name recorded with the same value, should bind")
	}
	// A call checked before a new scope began binds nothing in it.
	gen := tb.Gen()
	tb.Reset()
	if _, _, stale, ok := tb.BindAll(gen, []Binding{{Name: "g", Value: "7", Recorded: "r7"}}); ok || !stale {
		t.Fatalf("a call of the previous scope bound: ok=%v stale=%v", ok, stale)
	}
	if _, bound := tb.Lookup("g"); bound {
		t.Fatal("a stale call left g bound")
	}
}

func TestTable_ConcurrentBindAllKeepsValuesApart(t *testing.T) {
	for round := 0; round < 200; round++ {
		var tb Table
		var wg sync.WaitGroup
		took := make(chan string, 2)
		for _, name := range []string{"userId", "orderId"} {
			wg.Add(1)
			go func(name string) {
				defer wg.Done()
				if _, _, _, ok := tb.BindAll(tb.Gen(), []Binding{{Name: name, Value: "U", Recorded: "rec-" + name}}); ok {
					took <- name
				}
			}(name)
		}
		wg.Wait()
		close(took)
		if n := len(took); n != 1 {
			t.Fatalf("round %d: %d variables took one value, want 1", round, n)
		}
	}
}

// Start begins a scope with the values a test case gave bound, marked given;
// a call of the previous scope is stale in it.
func TestTable_Start(t *testing.T) {
	var tb Table
	tb.Bind(Binding{Name: "old", Value: "x"})
	gen := tb.Gen()
	tb.Start([]Binding{{Name: "id", Value: "u1"}, {Name: "id", Value: "u2"}})
	got := tb.Snapshot()
	if len(got) != 1 || got[0].Value != "u1" || !got[0].Given {
		t.Fatalf("started with %+v", got)
	}
	if _, _, stale, _ := tb.BindAll(gen, []Binding{{Name: "z", Value: "9"}}); !stale {
		t.Fatal("a call of the previous scope was not stale")
	}
	if _, _, _, ok := tb.BindAll(tb.Gen(), []Binding{{Name: "order", Value: "u1", Recorded: "r"}}); !ok {
		t.Fatal("a given value blocked another variable from taking it")
	}
}

// A scope opened in one step (StartOpening) begins in the step that makes its
// mocks live: open runs under the table's lock, so no call binds between the
// two, and a call that read the scope before it opened binds stale. The scope
// begins as StartCarrying begins one: a given value bound and marked, winning
// over a carried one of its name, the carried as made, the run's kept; and
// what the scope then binds is its own, never the run's.
func TestTable_AScopeBeginsInTheStepThatOpensIt(t *testing.T) {
	var tb Table
	tb.KeepRun()
	if _, _, _, ok := tb.BindAll(tb.Gen(), []Binding{{Name: "session", Value: "s-1", Recorded: "s-0"}}); !ok {
		t.Fatal("the start-up could not bind the run's session")
	}
	before := tb.Gen()
	opened := false
	tb.StartOpening(func() {
		if tb.mu.TryLock() {
			tb.mu.Unlock()
			t.Error("the scope's mocks went live outside the table's lock: a call could bind between them and the scope")
		}
		opened = true
	}, []Binding{{Name: "id", Value: "u1"}}, []Binding{{Name: "id", Value: "c-1"}, {Name: "order", Value: "o-1", Recorded: "o-0"}})
	if !opened {
		t.Fatal("the scope opened without making its mocks live")
	}
	if _, _, stale, ok := tb.BindAll(before, []Binding{{Name: "own", Value: "x-1", Recorded: "k-1"}}); ok || !stale {
		t.Fatalf("a call that read the scope before it opened bound: ok %v stale %v", ok, stale)
	}
	if b, _ := tb.Lookup("id"); b.Value != "u1" || !b.Given {
		t.Fatalf("id is %+v; want the given u1, marked given", b)
	}
	if b, _ := tb.Lookup("order"); b.Value != "o-1" || b.Given {
		t.Fatalf("order is %+v; want o-1, carried as made", b)
	}
	if b, _ := tb.Lookup("session"); b.Value != "s-1" {
		t.Fatalf("the run's session is %+v in the opened scope", b)
	}
	if _, _, _, ok := tb.BindAll(tb.Gen(), []Binding{{Name: "own", Value: "x-1", Recorded: "k-1"}}); !ok {
		t.Fatal("the opened scope's call could not bind")
	}
	tb.Reset()
	if _, ok := tb.Lookup("own"); ok {
		t.Fatal("what the opened scope bound outlived it: it was bound in the run's")
	}
}

// A value bound before the first scope began is the run's: every scope sees
// it, and a scope's own bindings go with the scope when the next begins. A
// value bound between two scopes is the ended scope's, never the run's, and a
// new set forgets the run's.
func TestTable_RunBindingsOutliveScopes(t *testing.T) {
	var tb Table
	tb.KeepRun()
	bind := func(b Binding) bool {
		_, _, _, ok := tb.BindAll(tb.Gen(), []Binding{b})
		return ok
	}
	bind(Binding{Name: "session", Value: "live-1", Recorded: "rec-1"})
	tb.Reset()
	bind(Binding{Name: "order", Value: "o-1", Recorded: "o-r"})
	if b, ok := tb.Lookup("session"); !ok || b.Value != "live-1" {
		t.Fatalf("a test does not see the start-up's binding: %+v %v", b, ok)
	}
	// The test ended: the gap before the next reads and binds in its scope.
	bind(Binding{Name: "job", Value: "j-1", Recorded: "j-r"})
	if _, ok := tb.Lookup("order"); !ok {
		t.Fatal("the gap after a test does not read the test's binding")
	}
	tb.Reset()
	if _, ok := tb.Lookup("session"); !ok {
		t.Fatal("session, bound before the first test, is gone in the next test")
	}
	if _, ok := tb.Lookup("order"); ok {
		t.Fatal("a test's binding outlived the next test's start")
	}
	if _, ok := tb.Lookup("job"); ok {
		t.Fatal("job, bound between two tests, became the run's")
	}
	if _, _, _, ok := tb.BindAll(tb.Gen(), []Binding{{Name: "other", Value: "live-1", Recorded: "x"}}); ok {
		t.Fatal("a test bound the run's value to another variable")
	}
	if got := len(tb.Snapshot()); got != 1 {
		t.Fatalf("the test's snapshot holds %d bindings, want the run's one", got)
	}
	tb.NewSet()
	if _, ok := tb.Lookup("session"); ok {
		t.Fatal("the run's binding outlived its set")
	}
	if !bind(Binding{Name: "session", Value: "live-2", Recorded: "rec-2"}) {
		t.Fatal("the next set's start-up could not bind the run's variable anew")
	}
	tb.Reset()
	if b, _ := tb.Lookup("session"); b.Value != "live-2" {
		t.Fatalf("the next set's test sees %q, want the new set's start-up value", b.Value)
	}
}

// KeepNoRun undoes KeepRun: what the run's held, and what is bound after,
// before the first scope began, is that scope's to forget as it begins; and
// KeepRun keeps none again until AllowRun.
func TestTable_KeepNoRunMakesTheStartUpsTheFirstScopesToForget(t *testing.T) {
	var tb Table
	tb.KeepRun()
	tb.BindAll(tb.Gen(), []Binding{{Name: "session", Value: "s-1", Recorded: "s-0"}})
	tb.KeepNoRun()
	if b, ok := tb.Lookup("session"); !ok || b.Value != "s-1" {
		t.Fatalf("KeepNoRun dropped what the start-up bound before the first scope: %+v %v", b, ok)
	}
	tb.BindAll(tb.Gen(), []Binding{{Name: "order", Value: "o-1", Recorded: "o-0"}})
	if _, _, _, ok := tb.BindAll(tb.Gen(), []Binding{{Name: "other", Value: "s-1", Recorded: "x-0"}}); ok {
		t.Fatal("a bind took the live value another variable holds")
	}
	tb.Start(nil)
	for _, name := range []string{"session", "order"} {
		if b, ok := tb.Lookup(name); ok {
			t.Fatalf("%s %+v outlived the first scope's begin: kept as the run's", name, b)
		}
	}
	// KeepRun keeps none until AllowRun, a new set's too; AllowRun keeps
	// none itself.
	keeps := func() bool {
		tb.BindAll(tb.Gen(), []Binding{{Name: "boot", Value: "b-1", Recorded: "b-0"}})
		tb.Start(nil)
		_, kept := tb.Lookup("boot")
		return kept
	}
	tb.NewSet()
	tb.KeepRun()
	if keeps() {
		t.Fatal("KeepRun kept the run's after KeepNoRun, before AllowRun")
	}
	tb.NewSet()
	tb.AllowRun()
	if keeps() {
		t.Fatal("AllowRun kept the run's by itself")
	}
	tb.NewSet()
	tb.KeepRun()
	if !keeps() {
		t.Fatal("KeepRun after AllowRun kept no run's")
	}
}

// A table that does not keep the run's (no KeepRun: the switch is off, or
// the set was never begun) forgets at the first scope what was bound before
// it, as before.
func TestTable_WithoutKeepRunTheFirstScopeForgetsTheStartUp(t *testing.T) {
	var tb Table
	tb.BindAll(tb.Gen(), []Binding{{Name: "session", Value: "live-1", Recorded: "rec-1"}})
	tb.Start(nil)
	if _, ok := tb.Lookup("session"); ok {
		t.Fatal("a table that keeps no run's kept a start-up binding into the first scope")
	}
}

// The gap after a scope is that scope's (the proxy's gap rule): its bindings
// stay in force until the next scope begins, a call checked in it still
// binds there once it ended, and what the gap binds is forgotten with it.
func TestTable_TheGapIsTheEndedScopes(t *testing.T) {
	for _, keepRun := range []bool{false, true} {
		var tb Table
		if keepRun {
			tb.KeepRun()
		}
		tb.Start(nil)
		gen := tb.Gen()
		if _, _, _, ok := tb.BindAll(gen, []Binding{{Name: "key", Value: "x-1", Recorded: "k-1"}}); !ok {
			t.Fatalf("keepRun %v: test 1 could not bind key", keepRun)
		}
		// Test 1 ended; nothing ends its scope until test 2 begins.
		if b, ok := tb.Lookup("key"); !ok || b.Value != "x-1" {
			t.Fatalf("keepRun %v: the gap reads key as %+v, %v; want test 1's x-1", keepRun, b, ok)
		}
		if _, _, stale, ok := tb.BindAll(gen, []Binding{{Name: "late", Value: "l-1", Recorded: "l-r"}}); stale || !ok {
			t.Fatalf("keepRun %v: a call checked in test 1, answered in the gap, did not bind (stale %v)", keepRun, stale)
		}
		tb.Start(nil)
		if _, ok := tb.Lookup("key"); ok {
			t.Fatalf("keepRun %v: test 2 reads test 1's key", keepRun)
		}
		if _, ok := tb.Lookup("late"); ok {
			t.Fatalf("keepRun %v: test 2 reads what test 1's late call bound in the gap", keepRun)
		}
		if _, _, stale, _ := tb.BindAll(gen, []Binding{{Name: "late2", Value: "l-2", Recorded: "l-r2"}}); !stale {
			t.Fatalf("keepRun %v: a call checked in test 1 bound in test 2", keepRun)
		}
	}
}

// The start-up binds a run's name once, with BindAll as every call: a second
// start-up call that binds it to another value, before or after a scope
// began, is refused with the first in force, and so is Bind.
func TestTable_AStartUpBindsARunNameOnce(t *testing.T) {
	var tb Table
	tb.KeepRun()
	if _, _, _, ok := tb.BindAll(tb.Gen(), []Binding{{Name: "session", Value: "v-1", Recorded: "r"}}); !ok {
		t.Fatal("the start-up could not bind session")
	}
	if _, with, _, ok := tb.BindAll(tb.Gen(), []Binding{{Name: "session", Value: "v-2", Recorded: "r"}}); ok || with.Value != "v-1" {
		t.Fatalf("a second start-up bind of session took (%v), in force %q", ok, with.Value)
	}
	tb.Reset()
	if _, with, _, ok := tb.BindAll(tb.Gen(), []Binding{{Name: "session", Value: "v-3", Recorded: "r"}}); ok || with.Value != "v-1" {
		t.Fatalf("a test bound the run's session again (%v), in force %q", ok, with.Value)
	}
	if have, took := tb.Bind(Binding{Name: "session", Value: "v-3", Recorded: "r"}); took || have.Value != "v-1" {
		t.Fatalf("Bind bound the run's session again (%v), in force %q", took, have.Value)
	}
	if b, _ := tb.Lookup("session"); b.Value != "v-1" {
		t.Fatalf("session reads %q, want the start-up's v-1", b.Value)
	}
}

// Bind keeps BindAll's rule that two variables recorded with two values never
// share one live value, the run's included: a start-up's value is a call's.
func TestTable_BindKeepsTwoVariablesApart(t *testing.T) {
	var tb Table
	tb.KeepRun()
	if _, took := tb.Bind(Binding{Name: "run-session", Value: "S", Recorded: "s-1"}); !took {
		t.Fatal("run-session could not bind")
	}
	if with, took := tb.Bind(Binding{Name: "run-token", Value: "S", Recorded: "t-1"}); took || with.Name != "run-session" {
		t.Fatalf("Bind took run-token with run-session's live value, though recorded as two values (%v, evidence %+v)", took, with)
	}
	tb.Reset()
	if with, took := tb.Bind(Binding{Name: "order", Value: "S", Recorded: "o-1"}); took || with.Name != "run-session" {
		t.Fatalf("Bind took order with the run's live value (%v, evidence %+v)", took, with)
	}
	if _, took := tb.Bind(Binding{Name: "again", Value: "S", Recorded: "s-1"}); !took {
		t.Fatal("a variable recorded with run-session's value could not take its live value")
	}
	var given Table
	given.Start([]Binding{{Name: "id", Value: "u1"}})
	if _, took := given.Bind(Binding{Name: "order", Value: "u1", Recorded: "r"}); !took {
		t.Fatal("a given value blocked another variable from taking it")
	}
}

// A scope begun early (the next test's calls in flight, answered in the gap
// before it) reads the run's bindings of the table it reads them of, whenever
// the start-up bound them: it is refused a value of theirs for another
// variable, as the test is. What it binds is its scope's, never the run's,
// and a new set forgets the run's for it too.
func TestTable_AScopeBegunEarlyReadsTheRuns(t *testing.T) {
	var vars, ahead Table
	vars.KeepRun()
	ahead.ReadRunOf(&vars)
	ahead.ReadRunOf(&ahead) // a table never reads its own run's of itself
	ahead.Start(nil)
	vars.BindAll(vars.Gen(), []Binding{{Name: "session", Value: "S", Recorded: "s-1", Mock: "boot"}})
	if b, ok := ahead.Lookup("session"); !ok || b.Value != "S" {
		t.Fatalf("the scope begun early reads session as %+v, %v; want the run's S", b, ok)
	}
	if _, _, _, ok := ahead.BindAll(ahead.Gen(), []Binding{{Name: "order", Value: "S", Recorded: "o-1"}}); ok {
		t.Fatal("a call in flight bound the run's value to another variable")
	}
	if _, _, _, ok := ahead.BindAll(ahead.Gen(), []Binding{{Name: "poll", Value: "P", Recorded: "p-1", Mock: "fetch"}}); !ok {
		t.Fatal("a call in flight could not bind its own variable")
	}
	if _, ok := vars.Lookup("poll"); ok {
		t.Fatal("what a call in flight bound became the run's")
	}
	if got := len(ahead.Snapshot()); got != 2 {
		t.Fatalf("the scope begun early holds %d bindings in force, want its own and the run's", got)
	}
	c := ahead.Clone()
	vars.NewSet()
	if _, ok := ahead.Lookup("session"); ok {
		t.Fatal("the scope begun early reads the previous set's run's")
	}
	if _, ok := c.Lookup("session"); !ok {
		t.Fatal("a clone does not hold the run's it read when it was made")
	}
}

// Calls of the scope begun early and of the start-up bind at once: one lock
// at a time, so none waits on the other.
func TestTable_AScopeBegunEarlyAndTheRunBindConcurrently(t *testing.T) {
	var vars, ahead Table
	vars.KeepRun()
	ahead.ReadRunOf(&vars)
	ahead.Start(nil)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(2)
		go func(i int) {
			defer wg.Done()
			vars.BindAll(vars.Gen(), []Binding{{Name: "run-" + strconv.Itoa(i), Value: "R" + strconv.Itoa(i), Recorded: "r" + strconv.Itoa(i)}})
		}(i)
		go func(i int) {
			defer wg.Done()
			ahead.BindAll(ahead.Gen(), []Binding{{Name: "own-" + strconv.Itoa(i), Value: "O" + strconv.Itoa(i), Recorded: "o" + strconv.Itoa(i)}})
			ahead.Snapshot()
		}(i)
	}
	wg.Wait()
	if got := len(ahead.Snapshot()); got != 16 {
		t.Fatalf("the scope begun early holds %d bindings, want 8 of its own and 8 of the run's", got)
	}
}

// Before the set's first test, the first test's calls in flight bind in a
// scope begun early (ReadRunOf) while the start-up still binds the run's.
// Whichever binds first, the other is refused that value for another
// variable recorded with another value: the first test, which carries the
// early scope's bindings in beside the run's (StartCarrying), never holds two
// variables at one live value. What a call's check reads before it binds
// (Beside) holds the early scope's too while the table binds the run's, and
// no longer once a scope began: two tests' variables may share a value.
func TestTable_TheRunAndAScopeBegunEarlyKeepTheirValuesApart(t *testing.T) {
	var vars, ahead Table
	vars.KeepRun()
	ahead.ReadRunOf(&vars)
	ahead.Start(nil)
	order := Binding{Name: "order", Value: "V", Recorded: "o-rec", Mock: "fetch", Place: "body.id"}
	if _, _, _, ok := ahead.BindAll(ahead.Gen(), []Binding{order}); !ok {
		t.Fatal("the call in flight could not bind order")
	}
	if got := vars.Beside(); len(got) != 1 || got[0].Name != "order" || got[0].Value != "V" {
		t.Fatalf("before the first test, a start-up call's check reads %+v; want the call in flight's order", got)
	}
	session := Binding{Name: "run-session", Value: "V", Recorded: "s-rec", Mock: "boot", Place: "body.key"}
	if bad, with, stale, ok := vars.BindAll(vars.Gen(), []Binding{session}); ok || stale || bad.Name != "run-session" || with.Name != "order" {
		t.Fatalf("the start-up bound run-session to order's live value: ok %v stale %v, evidence %+v beside %+v", ok, stale, bad, with)
	}
	if with, took := vars.Bind(session); took || with.Name != "order" {
		t.Fatalf("Bind took run-session at order's live value (%v, evidence %+v)", took, with)
	}
	if _, ok := vars.Lookup("run-session"); ok {
		t.Fatal("a refused bind left run-session bound")
	}
	// Recorded with order's value, a variable of the run's is that value.
	if _, _, _, ok := vars.BindAll(vars.Gen(), []Binding{{Name: "run-ref", Value: "V", Recorded: "o-rec", Mock: "boot"}}); !ok {
		t.Fatal("a run's variable recorded with order's recorded value could not take its live value")
	}
	session.Value = "S"
	if _, _, _, ok := vars.BindAll(vars.Gen(), []Binding{session}); !ok {
		t.Fatal("run-session could not bind a value of its own")
	}
	vars.StartCarrying(nil, ahead.Snapshot())
	first := vars.Snapshot()
	for _, a := range first {
		for _, b := range first {
			if TwoValues(a, b) {
				t.Fatalf("the first test holds %s and %s, recorded as two values, at one live value %q", a.Name, b.Name, b.Value)
			}
		}
	}
	if len(first) != 3 {
		t.Fatalf("the first test holds %+v, want order, run-ref and run-session", first)
	}

	// Once a scope began, the early scope is the next test's: its values are
	// no longer checked beside this test's.
	ahead.Reset()
	if _, _, _, ok := ahead.BindAll(ahead.Gen(), []Binding{{Name: "poll", Value: "P", Recorded: "p-rec", Mock: "fetch-2"}}); !ok {
		t.Fatal("the next test's call in flight could not bind poll")
	}
	if got := len(vars.Beside()); got != 3 {
		t.Fatalf("the first test's check reads %d bindings, want its own 3 and none of the next test's", got)
	}
	if _, _, _, ok := vars.BindAll(vars.Gen(), []Binding{{Name: "late", Value: "P", Recorded: "l-rec", Mock: "use-1"}}); !ok {
		t.Fatal("the first test was refused a value the next test's call in flight bound")
	}

	// The opposite order, as before: the run's first, and the call in flight
	// is refused.
	var vars2, ahead2 Table
	vars2.KeepRun()
	ahead2.ReadRunOf(&vars2)
	ahead2.Start(nil)
	if _, _, _, ok := vars2.BindAll(vars2.Gen(), []Binding{{Name: "run-session", Value: "V", Recorded: "s-rec", Mock: "boot"}}); !ok {
		t.Fatal("the start-up could not bind run-session")
	}
	if _, _, _, ok := ahead2.BindAll(ahead2.Gen(), []Binding{order}); ok {
		t.Fatal("the call in flight bound order to run-session's live value")
	}
}

// The start-up and the first test's call in flight bind two variables, recorded
// with two values, at one live value at once: exactly one of them takes it,
// whichever goes first.
func TestTable_TheRunAndAScopeBegunEarlyBindOneValueOnce(t *testing.T) {
	for i := 0; i < 400; i++ {
		var vars, ahead Table
		vars.KeepRun()
		ahead.ReadRunOf(&vars)
		ahead.Start(nil)
		var wg sync.WaitGroup
		var inFlight, run bool
		start := make(chan struct{})
		binds := []func(){
			func() {
				_, _, _, inFlight = ahead.BindAll(ahead.Gen(), []Binding{{Name: "order", Value: "V", Recorded: "o-rec", Mock: "fetch"}})
			},
			func() {
				_, _, _, run = vars.BindAll(vars.Gen(), []Binding{{Name: "run-session", Value: "V", Recorded: "s-rec", Mock: "boot"}})
			},
		}
		if i%2 == 1 {
			binds[0], binds[1] = binds[1], binds[0]
		}
		for _, bind := range binds {
			wg.Add(1)
			go func(bind func()) {
				defer wg.Done()
				<-start
				bind()
			}(bind)
		}
		close(start)
		wg.Wait()
		if inFlight == run {
			t.Fatalf("round %d: the call in flight bound %v and the start-up %v; want exactly one", i, inFlight, run)
		}
	}
}

// A scope begun early is checked beside the run it reads (ReadRunOf), and
// that run's binds beside it: once it reads another table's run's, the
// first table's binds no longer see its scope, nor it the first's run.
func TestTable_AScopeBegunEarlyIsBesideTheRunItReadsOnly(t *testing.T) {
	var old, cur, ahead Table
	old.KeepRun()
	cur.KeepRun()
	ahead.ReadRunOf(&old)
	ahead.ReadRunOf(&cur)
	ahead.ReadRunOf(&cur)
	ahead.Start(nil)
	if _, _, _, ok := ahead.BindAll(ahead.Gen(), []Binding{{Name: "order", Value: "V", Recorded: "o-rec", Mock: "fetch"}}); !ok {
		t.Fatal("the call in flight could not bind order")
	}
	if got := old.Beside(); len(got) != 0 {
		t.Fatalf("a run the early scope no longer reads is checked beside %+v", got)
	}
	session := Binding{Name: "run-session", Value: "V", Recorded: "s-rec", Mock: "boot"}
	if _, _, _, ok := old.BindAll(old.Gen(), []Binding{session}); !ok {
		t.Fatal("a run the early scope no longer reads was refused order's live value")
	}
	if _, ok := ahead.Lookup("run-session"); ok {
		t.Fatal("the early scope reads the run of a table it no longer reads")
	}
	if got := cur.Beside(); len(got) != 1 || got[0].Name != "order" {
		t.Fatalf("the run the early scope reads is checked beside %+v; want order once", got)
	}
	if _, _, _, ok := cur.BindAll(cur.Gen(), []Binding{session}); ok {
		t.Fatal("the run the early scope reads bound run-session to order's live value")
	}
}

// BindAll is the only bind of a call: two variables recorded with two values
// never share one live value, whatever the values look like (two reads of
// one clock in one millisecond included), in one call, across calls, and
// against the run's.
func TestTable_TwoVariablesNeverShareALiveValue(t *testing.T) {
	const live = "1792000000000"
	var tb Table
	tb.KeepRun()
	tb.BindAll(tb.Gen(), []Binding{{Name: "session", Value: "S", Recorded: "s-1"}})
	tb.Reset()
	if _, _, _, ok := tb.BindAll(tb.Gen(), []Binding{{Name: "created", Value: live, Recorded: "1"}, {Name: "polled", Value: live, Recorded: "2"}}); ok {
		t.Fatal("two variables recorded with two values bound one live value in one call")
	}
	if _, _, _, ok := tb.BindAll(tb.Gen(), []Binding{{Name: "created", Value: live, Recorded: "1"}}); !ok {
		t.Fatal("created could not bind")
	}
	if bad, with, _, ok := tb.BindAll(tb.Gen(), []Binding{{Name: "polled", Value: live, Recorded: "2"}}); ok || bad.Name != "polled" || with.Name != "created" {
		t.Fatalf("a later call bound polled to created's live value: ok %v, evidence %+v %+v", ok, bad, with)
	}
	if bad, with, _, ok := tb.BindAll(tb.Gen(), []Binding{{Name: "order", Value: "S", Recorded: "o-1"}}); ok || with.Name != "session" {
		t.Fatalf("a call bound the run's live value as another variable: ok %v, evidence %+v %+v", ok, bad, with)
	}
}

// A call checked before the next set began binds nothing in it: a set's
// boundary ends the scope, as the next scope's start does.
func TestTable_NewSetMakesAnEarlierCallStale(t *testing.T) {
	var tb Table
	tb.KeepRun()
	tb.Start(nil)
	gen := tb.Gen()
	tb.NewSet()
	if _, _, stale, ok := tb.BindAll(gen, []Binding{{Name: "late", Value: "l-1", Recorded: "l-r"}}); ok || !stale {
		t.Fatalf("a call of the previous set bound in the next: ok %v stale %v", ok, stale)
	}
	if _, bound := tb.Lookup("late"); bound {
		t.Fatal("a stale call of the previous set left late bound")
	}
}

// clockBinding is a binding of name to live, recorded as recorded (an RFC
// 3339 instant to the second), with the clock read the learn step reads in
// it when marked (models.MockVariable.Clock).
func clockBinding(t *testing.T, name, live, recorded string, marked bool) Binding {
	t.Helper()
	b := Binding{Name: name, Value: live, Recorded: recorded}
	if marked {
		at, err := time.Parse(time.RFC3339, recorded)
		if err != nil {
			t.Fatal(err)
		}
		b.Clock = []models.ClockRead{{At: at, Res: time.Second}}
	}
	return b
}

// Two clock reads the learn step marked (Binding.Clock) may share one live
// value though recorded with two values only when they were made in one
// second, as their recordings say (FD-3: two reads of one clock in one
// second), against the run's, in one call and across calls. Two marked reads
// further apart (a creation time and an expiry an hour later) may not: a
// regression that writes one as the other is refused. Nor may a marked read
// and an unmarked variable, whichever binds first and whatever the values
// look like.
func TestTable_TwoClockReadsTheLearnStepMarkedMayShareALiveValue(t *testing.T) {
	const live = "2026-10-19T08:00:00Z"
	b := func(name, recorded string, marked bool) Binding { return clockBinding(t, name, live, recorded, marked) }
	created := b("created", "2026-10-14T10:00:00Z", true)
	polled := b("polled", "2026-10-14T10:00:01Z", true)
	expires := b("expires", "2026-10-14T11:00:00Z", true)
	later := b("later", "2026-10-14T10:00:02Z", true)
	order := b("order", "o-1", false)
	stamp := b("stamp", "2026-10-14T10:00:01Z", false)
	booted := b("booted", "2026-10-14T09:59:59Z", true)
	session := b("session", "s-1", false)
	type step struct {
		run bool // bound by the start-up, before the first test
		bs  []Binding
	}
	for _, c := range []struct {
		name  string
		steps []step
		want  bool // the last step binds
	}{
		{"the run's read and a test's, a second apart", []step{{true, []Binding{booted}}, {false, []Binding{created}}}, true},
		{"two in one call, a second apart", []step{{false, []Binding{created, polled}}}, true},
		{"two across calls, a second apart", []step{{false, []Binding{created}}, {false, []Binding{polled}}}, true},
		{"the later one first", []step{{false, []Binding{polled}}, {false, []Binding{created}}}, true},
		{"an expiry an hour later, across calls", []step{{false, []Binding{created}}, {false, []Binding{expires}}}, false},
		{"an expiry an hour later, in one call", []step{{false, []Binding{created, expires}}}, false},
		{"an expiry an hour after the run's read", []step{{true, []Binding{created}}, {false, []Binding{expires}}}, false},
		{"two seconds apart", []step{{false, []Binding{created}}, {false, []Binding{later}}}, false},
		{"a marked read after an unmarked variable, across calls", []step{{false, []Binding{order}}, {false, []Binding{created}}}, false},
		{"a marked read after an unmarked variable, in one call", []step{{false, []Binding{order, created}}}, false},
		{"an unmarked variable after a marked read, across calls", []step{{false, []Binding{created}}, {false, []Binding{order}}}, false},
		{"an unmarked variable after a marked read, in one call", []step{{false, []Binding{created, order}}}, false},
		{"an unmarked one recorded as a time", []step{{false, []Binding{created}}, {false, []Binding{stamp}}}, false},
		{"a marked read after an unmarked one recorded as a time", []step{{false, []Binding{stamp}}, {false, []Binding{created}}}, false},
		{"an unmarked variable after the run's marked read", []step{{true, []Binding{booted}}, {false, []Binding{order}}}, false},
		{"a marked read after the run's unmarked variable", []step{{true, []Binding{session}}, {false, []Binding{created}}}, false},
	} {
		var tb Table
		tb.KeepRun()
		began := false
		for i, st := range c.steps {
			if !st.run && !began {
				tb.Reset()
				began = true
			}
			bad, with, _, ok := tb.BindAll(tb.Gen(), st.bs)
			if i < len(c.steps)-1 {
				if !ok {
					t.Fatalf("%s: step %d could not bind: %+v with %+v", c.name, i+1, bad, with)
				}
				continue
			}
			if ok != c.want {
				t.Errorf("%s: bound %v, want %v (evidence %+v with %+v)", c.name, ok, c.want, bad, with)
			}
		}
	}
}
