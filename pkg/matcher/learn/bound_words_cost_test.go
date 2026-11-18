package learn

import (
	"fmt"
	"reflect"
	"slices"
	"testing"
)

// A call asks the bound and used words of every earlier request leaf of
// its test (BoundWords, UsedWords), but splits into words only a leaf at a
// place a variable of the test, of the run or of a learn step's use is held
// at a word of: at 4 and 400 other earlier leaves none is split and none
// names a word, and the leaves at those places name the same words as
// before, each of their two texts split once. Counted, not timed: splitting
// every earlier leaf in each call grew a call's work with its test's
// length.
func TestOutcomeCostBoundWordsSplitOnlyLeavesAtAWordPlace(t *testing.T) {
	splits, split := 0, boundWordsOf
	t.Cleanup(func() { boundWordsOf = split })
	boundWordsOf = func(s string) [][2]int { splits++; return split(s) }
	const place = "body.cmd.filter.name"
	bound := Seen{Side: Request, Leaf: oText(place, "[E2E] app-crud-1793224420036884439"), Live: "[E2E] app-crud-1793311234567890123", Place: place}
	e, vars := usedWordsCall()
	for _, others := range []int{4, 400} {
		tr, n := servedUsed(e, vars)
		name, ok := tr.Name(n, "mock-201", place+"#3", nil)
		run, runOK := tr.RunName(n, "mock-301", place+"#3", nil)
		whole, wholeOK := tr.Name(n, "mock-401", place, nil)
		if !ok || !runOK || !wholeOK || whole == "" {
			t.Fatal("the tracker named no variable")
		}
		splits = 0
		for i := range others {
			p := fmt.Sprintf("body.other%d", i)
			other := Seen{Side: Request, Leaf: oText(p, "word one two"), Live: "word one three", Place: p}
			for _, mock := range []string{"mock-201", "mock-301", "mock-856"} {
				if got := tr.BoundWords(n, mock, other); got != nil {
					t.Errorf("%d other leaves: %s at %s is bound at %+v; want none", others, mock, p, got)
				}
				if got := tr.UsedWords(n, mock, other); got != nil {
					t.Errorf("%d other leaves: %s at %s is used at %+v; want none", others, mock, p, got)
				}
			}
		}
		for what, got := range map[string][]BoundWord{
			"the bound leaf of another exchange":            tr.BoundWords(n, "mock-202", bound),
			"a leaf whose variable is bound whole":          tr.BoundWords(n, "mock-401", bound),
			"the used leaf as a bound one":                  tr.BoundWords(n, "mock-856", e.Seen[1]),
			"the bound leaf as a used one":                  tr.UsedWords(n, "mock-201", bound),
			"the used leaf at another place of its request": tr.UsedWords(n, "mock-856", Seen{Side: Request, Leaf: e.Seen[1].Leaf, Live: e.Seen[1].Live, Place: "body.other"}),
		} {
			if got != nil {
				t.Errorf("%d other leaves: %s names %+v; want none", others, what, got)
			}
		}
		if splits != 0 {
			t.Errorf("%d other leaves: split %d texts into words; want none", others, splits)
		}
		splits = 0
		if got, want := tr.BoundWords(n, "mock-201", bound), []BoundWord{{Name: name, Word: 3, Recorded: "1793224420036884439", Live: "1793311234567890123", Place: place}}; !reflect.DeepEqual(got, want) {
			t.Errorf("%d other leaves: the test's bound leaf names %+v; want %+v", others, got, want)
		}
		if got, want := tr.BoundWords(n, "mock-301", bound), []BoundWord{{Name: run, Word: 3, Recorded: "1793224420036884439", Live: "1793311234567890123", Place: place}}; !reflect.DeepEqual(got, want) {
			t.Errorf("%d other leaves: the run's bound leaf names %+v; want %+v", others, got, want)
		}
		if got := tr.UsedWords(n, "mock-856", e.Seen[1]); !slices.Equal(got, []BoundWord{{Name: "name1a", Word: 1, Recorded: "alpha", Live: "beta", Place: "body.name"}}) {
			t.Errorf("%d other leaves: the used leaf names %+v; want the step's variable at word 1", others, got)
		}
		if splits != 6 {
			t.Errorf("%d other leaves: the three leaves at word places split %d texts; want their two texts each, 6", others, splits)
		}
	}
	// A place holding a '#' of its own is read at its words too.
	tr, n := servedUsed(e, vars)
	const hashed = "body.tags#a"
	name, _ := tr.Name(n, "mock-501", hashed+"#3", nil)
	at := Seen{Side: Request, Leaf: oText(hashed, bound.Leaf.Value), Live: bound.Live, Place: hashed}
	if got, want := tr.BoundWords(n, "mock-501", at), []BoundWord{{Name: name, Word: 3, Recorded: "1793224420036884439", Live: "1793311234567890123", Place: hashed}}; !reflect.DeepEqual(got, want) {
		t.Errorf("a place holding a '#': the bound leaf names %+v; want %+v", got, want)
	}
	// A later test holds none of the earlier test's places, but the run's;
	// a later set holds none.
	run, _ := tr.RunName(n, "mock-301", place+"#3", nil)
	tr.Name(n, "mock-201", place+"#3", nil)
	tr.End()
	tr.Begin()
	later, _, _ := tr.State()
	splits = 0
	if got := tr.BoundWords(later, "mock-201", bound); got != nil {
		t.Errorf("a later test: the earlier test's bound leaf names %+v; want none", got)
	}
	if got := tr.UsedWords(later, "mock-856", e.Seen[1]); got != nil {
		t.Errorf("a later test: the earlier test's used leaf names %+v; want none", got)
	}
	if got, want := tr.BoundWords(later, "mock-301", bound), []BoundWord{{Name: run, Word: 3, Recorded: "1793224420036884439", Live: "1793311234567890123", Place: place}}; !reflect.DeepEqual(got, want) {
		t.Errorf("a later test: the run's bound leaf names %+v; want %+v", got, want)
	}
	if splits != 2 {
		t.Errorf("a later test split %d texts; want the run's leaf's two alone", splits)
	}
	tr.End()
	tr.NewSet()
	tr.Begin()
	next, _, _ := tr.State()
	splits = 0
	if got := tr.BoundWords(next, "mock-301", bound); got != nil || splits != 0 {
		t.Errorf("a later set: the run's bound leaf names %+v, %d texts split; want none, none split", got, splits)
	}
}
