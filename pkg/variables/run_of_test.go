package variables

import (
	"testing"

	"go.keploy.io/server/v3/pkg/models"
)

// runVar is a run's variable's binding (models.RunVariable) of name, made
// at mock.
func runVar(name, value, mock string) Binding {
	return Binding{Name: models.RunVariablePrefix + name, Value: value, Recorded: "rec-" + name, Mock: mock, Place: "body.cmd.filter.appId#0"}
}

// RunOf is the run's bindings a named test's scope made itself: a run's
// variable a call of the test bound for the tests after it, which no
// scope's own bindings hold (Own, EndedOf). Never one the start-up bound
// before the first scope began, one a scope begun with no test named
// bound, nor another test's; none for test "". It holds while the test's
// scope runs, after it ended, and in what the set kept as it ended
// (SetBound), and NewSet forgets it.
func TestTableRunOfIsTheRunsBindingsATestsScopeMade(t *testing.T) {
	var tb Table
	tb.KeepEnded()
	tb.KeepRun()
	tb.Bind(runVar("boot", "b-live", "mock-1"))
	openTest(&tb, ".::TestGenerateTestSuitesOnClient")
	tb.Bind(runVar("appId0", "4c313756", "mock-1115"))
	tb.Bind(Binding{Name: "suiteId", Value: "s-live", Recorded: "s-rec", Mock: "mock-1116", Place: "body.id"})
	const T = ".::TestGenerateTestSuitesOnClient"
	if got := tb.RunOf(T); len(got) != 1 || got[0].Name != models.RunVariablePrefix+"appId0" || got[0].Value != "4c313756" || got[0].Mock != "mock-1115" {
		t.Fatalf("RunOf while the test's scope runs = %+v; want its run-appId0 alone (not the start-up's run-boot, nor its scope's suiteId)", got)
	}
	for _, b := range tb.Own() {
		if models.RunVariable(b.Name) {
			t.Fatalf("a run's binding is the scope's own: %+v", b)
		}
	}
	openTest(&tb, ".::TestStopGenerationWhileExecuting")
	tb.Bind(runVar("appId1", "dbf49737", "mock-368"))
	if got := tb.RunOf(T); len(got) != 1 || got[0].Value != "4c313756" {
		t.Fatalf("RunOf once the next test began = %+v; want the ended test's run-appId0 alone", got)
	}
	if got := tb.RunOf(".::TestStopGenerationWhileExecuting"); len(got) != 1 || got[0].Name != models.RunVariablePrefix+"appId1" {
		t.Fatalf("RunOf of the running test = %+v; want its own run-appId1 alone", got)
	}
	if got := tb.RunOf(""); len(got) != 0 {
		t.Fatalf(`RunOf("") = %+v; want none: no test is named ""`, got)
	}
	tb.Start(nil)
	tb.Bind(runVar("noname", "n-live", "mock-9"))
	for _, test := range []string{T, ".::TestStopGenerationWhileExecuting"} {
		for _, b := range tb.RunOf(test) {
			if b.Name == models.RunVariablePrefix+"noname" {
				t.Fatalf("a run's binding made in a scope begun with no test named is %s's", test)
			}
		}
	}

	kept := tb.Bound()
	openTest(&tb, ".::TestLater")
	tb.Bind(runVar("later", "l-live", "mock-20"))
	if got := kept.RunOf(T); len(got) != 1 || got[0].Value != "4c313756" {
		t.Fatalf("SetBound.RunOf = %+v; want the test's run-appId0", got)
	}
	if got := kept.RunOf(".::TestLater"); len(got) != 0 {
		t.Fatalf("SetBound.RunOf reads a binding made after it was kept: %+v", got)
	}
	got := tb.RunOf(T)
	got[0].Value = "changed"
	if again := tb.RunOf(T); again[0].Value != "4c313756" {
		t.Fatal("RunOf hands out the table's own binding")
	}
	ending := tb.NewSetKeeping(nil)
	if got := ending.RunOf(T); len(got) != 1 || got[0].Value != "4c313756" {
		t.Fatalf("what the ending set kept = %+v; want the test's run-appId0", got)
	}
	if got := tb.RunOf(T); len(got) != 0 {
		t.Fatalf("RunOf after NewSet = %+v; want none", got)
	}
	var none *SetBound
	if got := none.RunOf(T); got != nil {
		t.Fatalf("a nil SetBound's RunOf = %+v; want none", got)
	}
}

// A table that does not keep its ended scopes (KeepEnded: only the one a
// background miss reads) tells no test's run's bindings, while it binds
// the run's as before; and a name the run's holds again from a later
// scope (a carried binding put once more) is the later test's alone.
func TestTableRunOfKeepsTheLatestMakerAndOnlyOnAKeepingTable(t *testing.T) {
	var plain Table
	plain.KeepRun()
	openTest(&plain, ".::TestA")
	plain.Bind(runVar("appId0", "a-live", "mock-1"))
	if got := plain.RunOf(".::TestA"); len(got) != 0 {
		t.Fatalf("a table that keeps no ended scopes tells RunOf %+v; want none", got)
	}
	if b, ok := plain.Lookup(models.RunVariablePrefix + "appId0"); !ok || b.Value != "a-live" {
		t.Fatal("the run's binding is no longer bound")
	}

	var tb Table
	tb.KeepEnded()
	tb.KeepRun()
	openTest(&tb, ".::TestA")
	tb.Bind(runVar("appId0", "a-live", "mock-1"))
	tb.StartOpeningTest(".::TestB", func() {}, nil, []Binding{runVar("appId0", "b-live", "mock-2")}, nil)
	if got := tb.RunOf(".::TestA"); len(got) != 0 {
		t.Fatalf("RunOf of the earlier maker = %+v; want none once a later test's scope put the name again", got)
	}
	if got := tb.RunOf(".::TestB"); len(got) != 1 || got[0].Value != "b-live" || got[0].Mock != "mock-2" {
		t.Fatalf("RunOf of the later maker = %+v; want its b-live", got)
	}
	if b, ok := tb.Lookup(models.RunVariablePrefix + "appId0"); !ok || b.Value != "b-live" {
		t.Fatalf("the run's binding in force = %+v; RunOf must tell what the table binds", b)
	}
	// A scope begun with no test named that puts the name once more (a
	// carried binding) makes it no test's: the earlier maker's is not in
	// force.
	tb.StartCarrying(nil, []Binding{runVar("appId0", "c-live", "mock-3")})
	if got := tb.RunOf(".::TestB"); len(got) != 0 {
		t.Fatalf("RunOf of the earlier maker = %+v; want none once a scope of no test put the name again", got)
	}
}
