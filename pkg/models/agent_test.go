package models

import (
	"testing"
	"time"
)

// Under the proven switch a mock filter begins a test's variables only when
// it opens a test: a test case's filter, exact serving's or not. A test
// set's staging (from BaseTime, before any test runs, with no test's
// mapping) begins none, with exact serving or without: what the set's
// start-up binds after it is the run's (MockManager.NewVariableSet), which
// the set's first test must not forget. Exact serving's pool call begins none
// either.
func TestOnlyATestsFilterBeginsItsVariables(t *testing.T) {
	at := time.Date(2026, 10, 14, 10, 0, 0, 0, time.UTC)
	for _, c := range []struct {
		name   string
		params MockFilterParams
		want   bool
		stages bool
	}{
		{"exact serving's staging", MockFilterParams{AfterTime: BaseTime, BeforeTime: at, Exact: &ExactServing{Stage: true, Pool: []string{"mock-9"}}}, false, true},
		{"a test set's staging without exact serving", MockFilterParams{AfterTime: BaseTime, BeforeTime: at}, false, true},
		{"exact serving's open of a test case", MockFilterParams{Exact: &ExactServing{Test: "test-1", Owned: []string{"mock-1"}}}, true, false},
		{"exact serving's pool call", MockFilterParams{Exact: &ExactServing{}}, false, false},
		{"exact serving's pool call from BaseTime", MockFilterParams{AfterTime: BaseTime, BeforeTime: at, Exact: &ExactServing{}}, false, false},
		{"a test case's filter without exact serving", MockFilterParams{AfterTime: at, BeforeTime: at.Add(time.Second), MockMapping: []string{"mock-1"}}, true, false},
		{"a test case's filter by its window alone", MockFilterParams{AfterTime: at, BeforeTime: at.Add(time.Second)}, true, false},
		{"a test case recorded without times", MockFilterParams{}, true, false},
		{"a filter from BaseTime that names a test's mocks", MockFilterParams{AfterTime: BaseTime, BeforeTime: at, MockMapping: []string{"mock-1"}}, true, false},
	} {
		if got := c.params.BeginsTestVariables(); got != c.want {
			t.Errorf("%s: BeginsTestVariables %v, want %v", c.name, got, c.want)
		}
		// StagesSet says which filter is a set's staging: exact serving's
		// says so itself (Stage), whatever its window.
		if got := c.params.StagesSet(); got != c.stages {
			t.Errorf("%s: StagesSet %v, want %v", c.name, got, c.stages)
		}
	}
}

// A MockWindowHeader value reads back as the mode it was written from; one
// this keploy cannot read, or an absent header, says nothing. Only a load by
// mapping or by the strict window runs the carry-over lookahead, and a set's
// calls fold to the mode that holds more.
func TestMockWindowModeReadsBackWhatTheAgentSaid(t *testing.T) {
	for _, m := range []MockWindowMode{
		{Strict: true, Load: MockLoadStrictWindow},
		{Strict: false, Load: MockLoadMapping},
		{Strict: false, Load: MockLoadLaxAll},
		{Strict: true, Load: MockLoadMapping},
	} {
		got, ok := ParseMockWindowMode(m.String())
		if !ok || got != m {
			t.Errorf("ParseMockWindowMode(%q) = %+v, %v; want %+v", m.String(), got, ok, m)
		}
	}
	for _, v := range []string{"", "strict=on", "strict=maybe;load=mapping", "strict=on;load=all", "load=mapping;strict=on"} {
		if got, ok := ParseMockWindowMode(v); ok {
			t.Errorf("ParseMockWindowMode(%q) read %+v", v, got)
		}
	}
	for load, want := range map[string]bool{MockLoadMapping: true, MockLoadStrictWindow: true, MockLoadLaxAll: false} {
		if got := (MockWindowMode{Load: load}).Lookahead(); got != want {
			t.Errorf("a %s load runs the lookahead: %v, want %v", load, got, want)
		}
	}
	var s MockWindowsSaid
	s.Add(MockWindowMode{Strict: false, Load: MockLoadLaxAll})
	if s != (MockWindowsSaid{}) {
		t.Fatalf("a lax call folds to %+v", s)
	}
	s.Add(MockWindowMode{Strict: false, Load: MockLoadMapping})
	s.Add(MockWindowMode{Strict: true, Load: MockLoadLaxAll})
	if s != (MockWindowsSaid{Strict: true, Lookahead: true}) {
		t.Fatalf("a set's calls fold to %+v, want both", s)
	}
	// A later call that says less takes nothing back: the fold is the mode
	// that holds the most of any call, not the last call's.
	s.Add(MockWindowMode{Strict: false, Load: MockLoadLaxAll})
	if s != (MockWindowsSaid{Strict: true, Lookahead: true}) {
		t.Fatalf("a lax call after the others folds the set's calls to %+v, want both kept", s)
	}
}
