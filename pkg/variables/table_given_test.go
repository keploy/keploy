package variables

import (
	"sort"
	"testing"
)

// Given is the open scope's given values alone, those Start's test case gave
// and those its test gave since (Give), never a value a call bound; a scope
// that holds none answers nil and allocates nothing, as every call's serve
// asks it.
func TestTableGivenIsTheScopesGivenValues(t *testing.T) {
	var tb Table
	tb.Start([]Binding{{Name: "userId", Value: "u-9", Recorded: "u-1"}})
	if _, ok := tb.Give(Binding{Name: "given:app", Value: "app-9", Recorded: "app-1"}); !ok {
		t.Fatal("the test's given did not bind")
	}
	if _, ok := tb.Bind(Binding{Name: "orderId", Value: "o-9", Recorded: "o-1", Mock: "mock-1", Place: "body.id"}); !ok {
		t.Fatal("the call's variable did not bind")
	}
	var names []string
	for _, b := range tb.Given() {
		if !b.Given {
			t.Fatalf("Given holds %s, which is no given", b.Name)
		}
		names = append(names, b.Name+"="+b.Value)
	}
	sort.Strings(names)
	if len(names) != 2 || names[0] != "given:app=app-9" || names[1] != "userId=u-9" {
		t.Fatalf("Given is %v; want given:app and userId", names)
	}

	var none Table
	none.Start(nil)
	if _, ok := none.Bind(Binding{Name: "orderId", Value: "o-9", Recorded: "o-1", Mock: "mock-1", Place: "body.id"}); !ok {
		t.Fatal("the call's variable did not bind")
	}
	if got := none.Given(); got != nil {
		t.Fatalf("a scope with no given has %v", got)
	}
	if n := testing.AllocsPerRun(100, func() { _ = none.Given() }); n != 0 {
		t.Fatalf("Given of a scope with no given allocated %v times", n)
	}
}

// A given, and a note on why a name is unbound, are taken only in the scope
// begun for the test that gave them (StartOpeningTest): never before any
// scope began, in a scope begun with no test named, for a test named "",
// nor in another test's scope, whose bindings are that test's once it ends
// (EndedOf). A refusal for the scope returns a binding with no name; the
// test's own scope takes it, and refuses a second value of the name.
func TestTableGivesAndNotesOnlyInTheTestsOwnScope(t *testing.T) {
	g := Binding{Name: "given:app", Value: "app-2", Recorded: "app-1", Mock: "test case post-apps-2", Place: "request body.name"}
	var tb Table
	tb.KeepEnded()
	if have, ok := tb.GiveTest("test-2", g); ok || have.Name != "" {
		t.Fatalf("a given before any scope began: %+v %v", have, ok)
	}
	if tb.NoteTest("test-2", "given:app", "why") {
		t.Fatal("a note before any scope began was taken")
	}
	tb.Start(nil)
	for _, test := range []string{"", "test-2"} {
		if have, ok := tb.GiveTest(test, g); ok || have.Name != "" {
			t.Fatalf("a given of test %q in a scope begun for no test: %+v %v", test, have, ok)
		}
		if tb.NoteTest(test, "given:app", "why") {
			t.Fatalf("a note of test %q in a scope begun for no test was taken", test)
		}
	}
	tb.StartOpeningTest("test-1", func() {}, nil, nil, nil)
	if have, ok := tb.GiveTest("test-2", g); ok || have.Name != "" {
		t.Fatalf("test-2's given in test-1's scope: %+v %v", have, ok)
	}
	if tb.NoteTest("test-2", "given:app", "why") || tb.Unbound("given:app") != "" {
		t.Fatal("test-2's note was taken in test-1's scope")
	}
	tb.StartOpeningTest("test-2", func() {}, nil, nil, nil)
	if got := tb.EndedOf("test-1"); len(got) != 0 {
		t.Fatalf("test-1's ended scope holds %+v", got)
	}
	if !tb.NoteTest("test-2", "given:other", "why") || tb.Unbound("given:other") != "why" {
		t.Fatal("test-2's note was not taken in its own scope")
	}
	if _, ok := tb.GiveTest("test-2", g); !ok {
		t.Fatal("test-2's given was refused in its own scope")
	}
	if b, ended, ok := tb.LookupOf("test-2", "given:app"); !ok || ended || !b.Given || b.Value != "app-2" {
		t.Fatalf("test-2's scope holds %+v (ended %v, ok %v)", b, ended, ok)
	}
	again := g
	again.Value = "app-3"
	if have, ok := tb.GiveTest("test-2", again); ok || have.Value != "app-2" {
		t.Fatalf("a second given of one name: %+v %v", have, ok)
	}
}
