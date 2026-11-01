package learn

import (
	"reflect"
	"strings"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/variables"
)

// The seams as decide.go declares them: package variables are all set
// before any init runs, so these hold the defaults even where another file's
// init registers an order or a clock.
var declaredDecideBy, declaredClockInWindow, declaredClockSinceBegin = decideBy, clockInWindow, clockSinceBegin

// TestReplayAndLearnShareOneWordRule (TY3): variables.Words, the word rule
// replay reads, finds the runs learn's isWord finds.
func TestReplayAndLearnShareOneWordRule(t *testing.T) {
	runs := func(s string) [][2]int {
		var out [][2]int
		for i := 0; i < len(s); i++ {
			if !isWord(s[i]) {
				continue
			}
			j := i
			for j < len(s) && isWord(s[j]) {
				j++
			}
			out = append(out, [2]int{i, j})
			i = j
		}
		return out
	}
	for _, s := range []string{
		"order-17", "uploads/17/a.png", "Order+17", "6f1c2b9e-3d4a-4e5f-8a7b-0c1d2e3f4a5b",
		"eyJpZCI6Im8tMTcifQ_-x9", "cart_42", "", "---", "é17ü", "a.b=c,d;e f\tg",
	} {
		if got, want := variables.Words(s), runs(s); !reflect.DeepEqual(got, want) {
			t.Errorf("Words(%q) = %v; learn's isWord runs are %v", s, got, want)
		}
	}
	got := variables.Words("order-17")
	if len(got) != 2 || "order-17"[got[1][0]:got[1][1]] != "17" {
		t.Errorf(`the "17" of "order-17" is not a word: %v`, got)
	}
}

// TestDecideWithNoOrderRefusesEveryChangedWord (TY4): with no order
// registered, Decide refuses every drifted word as one keploy cannot check
// yet, a leaf whose words do not pair by position whole, and never gives a
// Clock, a Value or a fresh word.
func TestDecideWithNoOrderRefusesEveryChangedWord(t *testing.T) {
	was := decideBy
	decideBy = refuseAll
	t.Cleanup(func() { decideBy = was })
	leaves := func(kv ...string) map[string]Leaf {
		m := map[string]Leaf{}
		for i := 0; i < len(kv); i += 2 {
			l := Leaf{Path: kv[i], Type: String, Value: kv[i+1]}
			if strings.HasPrefix(kv[i], "header.date") {
				l.Type = Instant
			}
			m[kv[i]] = l
		}
		return m
	}
	in := Input{
		Test: "test-1", Mock: "mock-3",
		Rec:   Shape{Leaves: leaves("body.id", "order-17-a-5", "body.sep", "a-b", "header.date", "2026-11-01T10:00:00Z", "body.more", "a b", "body.mix", "a-17"), Body: map[string]interface{}{}},
		Live:  Shape{Leaves: leaves("body.id", "order-42-a-6", "body.sep", "a_b", "header.date", "2026-11-01T10:00:05Z", "body.more", "a b c", "body.mix", "a_42"), Body: map[string]interface{}{}},
		Drift: []string{"body.id", "body.sep", "header.date", "body.more", "body.gone", "body.mix"},
	}
	ds := Decide(in)
	type got struct {
		field, at, r, f string
		word            int
	}
	var gots []got
	for _, d := range ds {
		if d.Outcome != OutcomeRefused || d.Clock != nil || !reflect.DeepEqual(d.From, Place{}) || len(d.Echo) != 0 || d.Refusal == nil {
			t.Fatalf("Decide gave %+v; want a refusal alone", d)
		}
		r := d.Refusal
		if r.Kind != RefusalCannotCheck || r.Test != "test-1" || r.Mock != "mock-3" || r.Recorded != d.Recorded || r.Full != d.Full || len(r.Edits) != 1 || r.Edits[0].Kind != EditNoiseAtField {
			t.Errorf("refusal %+v", r)
		}
		if !strings.HasPrefix(d.Line, "test-1 mock-3 "+r.Field) || !strings.Contains(d.Line, "keploy cannot check") || !strings.Contains(d.Line, "keploy's limit") {
			t.Errorf("line %q", d.Line)
		}
		gots = append(gots, got{r.Field, d.At.String(), d.Recorded, d.Full, r.Word})
	}
	want := []got{
		{"body.id", "body.id#1", "17", "42", 1},
		{"body.id", "body.id#3", "5", "6", 3},
		{"body.sep", "body.sep", "a-b", "a_b", -1},
		{"header.date", "header.date", "2026-11-01T10:00:00Z", "2026-11-01T10:00:05Z", -1},
		{"body.more", "body.more", "a b", "a b c", -1},
		{"body.gone", "", "", "", -1},
		{"body.mix", "body.mix", "a-17", "a_42", -1},
	}
	if !reflect.DeepEqual(gots, want) {
		t.Errorf("Decide refused\n%+v\nwant\n%+v", gots, want)
	}
	if !strings.HasPrefix(ds[0].Line, `test-1 mock-3 body.id word 1: recorded "17", round F "42": `) {
		t.Errorf("line %q", ds[0].Line)
	}
}

// TestTheSeamsDefaultToMainsReadings: as declared, Decide's order refuses
// every word, and its clock seams are main's readings (offsetReading,
// sinceTestBegin); Decide asks whichever order is registered.
func TestTheSeamsDefaultToMainsReadings(t *testing.T) {
	same := func(a, b interface{}) bool { return reflect.ValueOf(a).Pointer() == reflect.ValueOf(b).Pointer() }
	if !same(declaredDecideBy, refuseAll) || !same(declaredClockInWindow, offsetReading) || !same(declaredClockSinceBegin, sinceTestBegin) {
		t.Fatal("a seam does not default to main's reading")
	}
	if ln, ok := declaredClockSinceBegin(Learned{Kind: KindNoise}); ok || ln.Kind != KindNoise {
		t.Errorf("clockSinceBegin of noise = %+v, %v", ln, ok)
	}
	if _, why, ok := declaredClockInWindow("body.at", typedInstant{}, typedInstant{}, nil, nil); ok || why != "" {
		t.Errorf("clockInWindow with no window = %q, %v", why, ok)
	}
	was := decideBy
	t.Cleanup(func() { decideBy = was })
	decideBy = func(in Input) []Decision { return []Decision{{Line: in.Mock}} }
	if ds := Decide(Input{Mock: "mock-9"}); len(ds) != 1 || ds[0].Line != "mock-9" {
		t.Errorf("Decide does not ask the registered order: %+v", ds)
	}
}

// TestInputNamesEachMocksScopeAndWhereTheCallLanded: ScopesOf reads
// mappings.yaml's places (a test's own list, startup:, worker:, any other
// section), a mock named in no place or in two is unknown, an Input with
// no lookup knows no scope, and Landed names the window in a line.
func TestInputNamesEachMocksScopeAndWhereTheCallLanded(t *testing.T) {
	m := &models.Mapping{
		TestCases: []models.MappedTestCase{{ID: "test-1", Mocks: []models.MockEntry{{Name: "mock-1"}, {Name: "mock-2"}}}},
		Startup:   []models.MockEntry{{Name: "mock-3"}},
		Worker:    []models.MockEntry{{Name: "mock-4"}, {Name: "mock-2"}},
		Shared:    []models.MockEntry{{Name: "mock-5"}},
	}
	in := Input{Scope: ScopesOf(m)}
	for mock, want := range map[string]Scope{
		"mock-1": ScopeTest("test-1"), "mock-2": ScopeUnknown, "mock-3": ScopeStartup,
		"mock-4": ScopeWorker, "mock-5": "shared:", "mock-6": ScopeUnknown,
	} {
		if got := in.ScopeOf(mock); got != want {
			t.Errorf("scope of %s = %q; want %q", mock, got, want)
		}
	}
	if got := (Input{}).ScopeOf("mock-1"); got != ScopeUnknown {
		t.Errorf("an Input with no lookup knows %s's scope: %q", "mock-1", got)
	}
	if got := ScopesOf(nil)("mock-1"); got != ScopeUnknown {
		t.Errorf("no mappings.yaml knows a scope: %q", got)
	}
	for l, want := range map[Landed]string{
		{}: "a window keploy was not told", {Where: LandedOwn}: "its own test's window",
		{Where: LandedOther, Test: "test-2"}: "test-2's window", {Where: LandedOther}: "another test's window",
		{Where: LandedBetween}: "between tests",
	} {
		if got := l.String(); got != want {
			t.Errorf("%+v names %q; want %q", l, got, want)
		}
	}
}
