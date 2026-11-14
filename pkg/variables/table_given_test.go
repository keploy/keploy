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

// givenNames is the given values of tb's scope in force, as name=value,
// sorted.
func givenNames(tb *Table) []string {
	var names []string
	for _, b := range tb.Given() {
		names = append(names, b.Name+"="+b.Value)
	}
	sort.Strings(names)
	return names
}

// A value a test worker gave while none of its tests was open (a test
// file's setup) is in force for the calls of the start-up or the gap from
// then, as a given and never as the run's, until the next scope begins,
// which carries it in itself. One name is given between tests once.
func TestTableAGivenBetweenTestsHoldsUntilTheNextScope(t *testing.T) {
	var tb Table
	tb.KeepRun()
	b, ok := tb.GiveBetween(Binding{Name: "given:suite", Value: "s-9", Recorded: "s-1", Place: "a.spec.ts:9"})
	if !ok || !b.Given {
		t.Fatalf("the setup's given did not bind: %+v", b)
	}
	if got, ok := tb.Lookup("given:suite"); !ok || got.Value != "s-9" || got.Recorded != "s-1" {
		t.Fatalf("the start-up's calls read %+v, %v", got, ok)
	}
	if got := givenNames(&tb); len(got) != 1 || got[0] != "given:suite=s-9" {
		t.Fatalf("Given is %v", got)
	}
	if snap := tb.Snapshot(); len(snap) != 1 || snap[0].Name != "given:suite" {
		t.Fatalf("Snapshot is %+v", snap)
	}
	if have, ok := tb.GiveBetween(Binding{Name: "given:suite", Value: "s-8"}); ok || have.Value != "s-9" {
		t.Fatalf("a second given of one name bound: %+v, %v", have, ok)
	}
	tb.Start(nil)
	if got, ok := tb.Lookup("given:suite"); ok {
		t.Fatalf("the next scope reads the gap's given %+v", got)
	}
	if got := tb.Given(); got != nil {
		t.Fatalf("the next scope's givens are %v", got)
	}
}

// In the gap after a test, a value given between tests is in force over
// the ended test's given of its name. An end (a test file's) hides from the
// gap's calls every given in force there, the ended test's too, and keeps
// the ended scope's other bindings in force (the gap rule); a later
// file's setup then gives its own.
func TestTableAnEndHidesTheGapsGivensAndKeepsItsOtherBindings(t *testing.T) {
	var tb Table
	tb.Start([]Binding{{Name: "given:suite", Value: "a-9", Recorded: "a-1"}, {Name: "userId", Value: "u-9", Recorded: "u-1"}})
	if _, ok := tb.Bind(Binding{Name: "orderId", Value: "o-9", Recorded: "o-1", Mock: "mock-1", Place: "body.id"}); !ok {
		t.Fatal("the call's variable did not bind")
	}
	if _, ok := tb.GiveBetween(Binding{Name: "given:suite", Value: "a-8", Recorded: "a-2"}); !ok {
		t.Fatal("the gap's given did not bind")
	}
	if got, _ := tb.Lookup("given:suite"); got.Value != "a-8" {
		t.Fatalf("the gap reads given:suite = %q, want the setup's a-8", got.Value)
	}
	if got := givenNames(&tb); len(got) != 2 || got[0] != "given:suite=a-8" || got[1] != "userId=u-9" {
		t.Fatalf("Given is %v", got)
	}
	tb.EndBetween()
	if got, ok := tb.Lookup("given:suite"); ok {
		t.Fatalf("after the end the gap reads %+v", got)
	}
	for _, name := range []string{"orderId", "userId"} {
		if _, ok := tb.Lookup(name); !ok {
			t.Fatalf("the end hid %s, which no test gave keploy", name)
		}
	}
	if got := givenNames(&tb); len(got) != 1 || got[0] != "userId=u-9" {
		t.Fatalf("Given after the end is %v", got)
	}
	for _, b := range tb.Snapshot() {
		if b.Name == "given:suite" {
			t.Fatalf("Snapshot after the end holds %+v", b)
		}
	}
	c := tb.Clone()
	if _, ok := c.Lookup("given:suite"); ok {
		t.Fatal("a clone reads a given the end hid")
	}
	if _, ok := tb.GiveBetween(Binding{Name: "given:suite", Value: "b-9", Recorded: "b-1"}); !ok {
		t.Fatal("the next file's given did not bind")
	}
	if got, _ := tb.Lookup("given:suite"); got.Value != "b-9" {
		t.Fatalf("the next file's setup reads given:suite = %q", got.Value)
	}
	if got, _ := tb.Clone().Lookup("given:suite"); got.Value != "b-9" {
		t.Fatalf("a clone reads given:suite = %q", got.Value)
	}
	tb.Reset()
	if _, ok := tb.Lookup("given:suite"); ok {
		t.Fatal("a reset scope reads the gap's given")
	}
	tb.GiveBetween(Binding{Name: "given:suite", Value: "c-9"})
	tb.NewSet()
	if got, ok := tb.Lookup("given:suite"); ok {
		t.Fatalf("a new set's start-up reads the last set's gap given %+v", got)
	}
}
