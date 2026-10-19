package variables

import (
	"errors"
	"strconv"
	"sync"
	"testing"

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

// A value bound before the first scope began is the run's: every scope sees
// it, and a scope's own bindings go with the scope when the next begins. A
// value bound between two scopes is the ended scope's, never the run's, and a
// new set forgets the run's.
func TestTable_RunBindingsOutliveScopes(t *testing.T) {
	var tb Table
	tb.KeepRun()
	tb.Bind(Binding{Name: "session", Value: "live-1", Recorded: "rec-1"})
	tb.Reset()
	tb.Bind(Binding{Name: "order", Value: "o-1", Recorded: "o-r"})
	if b, ok := tb.Lookup("session"); !ok || b.Value != "live-1" {
		t.Fatalf("a test does not see the start-up's binding: %+v %v", b, ok)
	}
	// The test ended: the gap before the next reads and binds in its scope.
	tb.Bind(Binding{Name: "job", Value: "j-1", Recorded: "j-r"})
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
	if _, took := tb.Bind(Binding{Name: "session", Value: "live-2", Recorded: "rec-2"}); !took {
		t.Fatal("the next set's start-up could not bind the run's variable anew")
	}
	tb.Reset()
	if b, _ := tb.Lookup("session"); b.Value != "live-2" {
		t.Fatalf("the next set's test sees %q, want the new set's start-up value", b.Value)
	}
}

// A table that does not keep the run's (no KeepRun: the switch is off, or
// the set was never begun) forgets at the first scope what was bound before
// it, as before.
func TestTable_WithoutKeepRunTheFirstScopeForgetsTheStartUp(t *testing.T) {
	var tb Table
	tb.Bind(Binding{Name: "session", Value: "live-1", Recorded: "rec-1"})
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

// The start-up binds a run's name once: a second start-up Bind of it, before
// or after a scope began, leaves the first in force.
func TestTable_AStartUpBindsARunNameOnce(t *testing.T) {
	var tb Table
	tb.KeepRun()
	if _, took := tb.Bind(Binding{Name: "session", Value: "v-1", Recorded: "r"}); !took {
		t.Fatal("the start-up could not bind session")
	}
	if have, took := tb.Bind(Binding{Name: "session", Value: "v-2", Recorded: "r"}); took || have.Value != "v-1" {
		t.Fatalf("a second start-up bind of session took (%v), in force %q", took, have.Value)
	}
	tb.Reset()
	if have, took := tb.Bind(Binding{Name: "session", Value: "v-3", Recorded: "r"}); took || have.Value != "v-1" {
		t.Fatalf("a test bound the run's session again (%v), in force %q", took, have.Value)
	}
	if b, _ := tb.Lookup("session"); b.Value != "v-1" {
		t.Fatalf("session reads %q, want the start-up's v-1", b.Value)
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
	vars.Bind(Binding{Name: "session", Value: "S", Recorded: "s-1", Mock: "boot"})
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
			vars.Bind(Binding{Name: "run-" + strconv.Itoa(i), Value: "R" + strconv.Itoa(i), Recorded: "r" + strconv.Itoa(i)})
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

// BindAll is the only bind of a call: two variables recorded with two values
// never share one live value, whatever the values look like (two reads of
// one clock in one millisecond included), in one call, across calls, and
// against the run's.
func TestTable_TwoVariablesNeverShareALiveValue(t *testing.T) {
	const live = "1792000000000"
	var tb Table
	tb.KeepRun()
	tb.Bind(Binding{Name: "session", Value: "S", Recorded: "s-1"})
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
