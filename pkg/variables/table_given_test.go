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
