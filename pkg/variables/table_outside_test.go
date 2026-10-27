package variables

import "testing"

// The claim key's scope (Binding.Outside): what a call outside every test
// binds is read by every test of the key and survives each test's begin; a
// test's bindings stay the test's; the two are looked up apart, and a new
// claim drops both.
func TestTable_TheClaimKeysScope(t *testing.T) {
	var tb Table
	role := Binding{Name: "name", Value: "e2e-role-live", Recorded: "e2e-role-rec", Mock: "pool-0", Place: "body.name", Outside: true}
	if _, _, _, ok := tb.BindAll(tb.Gen(), []Binding{role}); !ok {
		t.Fatal("a call outside every test binds the key's variable")
	}
	if _, ok := tb.Lookup("name"); ok {
		t.Fatal("the key's variable is not the test's")
	}
	tb.Reset() // a test begins
	tb.Start(nil)
	if b, ok := tb.LookupOutside("name"); !ok || b.Value != "e2e-role-live" {
		t.Fatalf("a test begun reads the key's variable: %+v %v", b, ok)
	}
	// A test's own variable of the same name is another one, in its scope.
	own := Binding{Name: "name", Value: "branch-live", Recorded: "branch-rec", Mock: "mock-1", Place: "body.input.name"}
	if _, _, _, ok := tb.BindAll(tb.Gen(), []Binding{own}); !ok {
		t.Fatal("the test binds its own")
	}
	if b, _ := tb.Lookup("name"); b.Value != "branch-live" {
		t.Fatalf("the test's: %+v", b)
	}
	if b, _ := tb.LookupOutside("name"); b.Value != "e2e-role-live" {
		t.Fatalf("the key's: %+v", b)
	}
	// Two variables are two values, across the scopes.
	clash := Binding{Name: "other", Value: "e2e-role-live", Recorded: "o-rec", Mock: "mock-2", Place: "body.o"}
	if bad, with, _, ok := tb.BindAll(tb.Gen(), []Binding{clash}); ok || bad.Name != "other" || with.Name != "name" || !with.Outside {
		t.Fatalf("a test's variable took the key's value: ok %v bad %+v with %+v", ok, bad, with)
	}
	// The key's variable is bound once.
	again := role
	again.Value = "e2e-role-other"
	if _, with, _, ok := tb.BindAll(tb.Gen(), []Binding{again}); ok || with.Value != "e2e-role-live" {
		t.Fatalf("the key's variable bound twice: ok %v with %+v", ok, with)
	}
	if got := tb.OutsideSnapshot(); len(got) != 1 || got[0].Name != "name" {
		t.Fatalf("outside snapshot %+v", got)
	}
	if got := tb.Snapshot(); len(got) != 1 || got[0].Value != "branch-live" {
		t.Fatalf("the test's snapshot holds its own only: %+v", got)
	}
	c := tb.Clone()
	if b, ok := c.LookupOutside("name"); !ok || b.Value != "e2e-role-live" {
		t.Fatal("a clone keeps the key's scope")
	}
	gen := tb.Gen()
	tb.StartOutside() // a new claim
	if _, ok := tb.LookupOutside("name"); ok {
		t.Fatal("a new claim drops the key's variables")
	}
	if _, ok := tb.Lookup("name"); ok {
		t.Fatal("and the previous claim's last test's")
	}
	if _, _, stale, _ := tb.BindAll(gen, []Binding{role}); !stale {
		t.Fatal("a call checked before the claim binds nothing after it")
	}
	if b, ok := c.LookupOutside("name"); !ok || b.Value != "e2e-role-live" {
		t.Fatal("the clone shares nothing")
	}
}
