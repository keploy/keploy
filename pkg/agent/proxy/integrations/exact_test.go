package integrations

import "testing"

// exactStore is a MockMemDb that says whether it serves exactly.
type exactStore struct {
	MockMemDb
	exact bool
}

func (s exactStore) ExactServing() bool { return s.exact }

// unheldView is a view over a store HoldToRound returned that forwards why
// it reads nothing (UnheldTeller) and no exact serving.
type unheldView struct{ MockMemDb }

func (v unheldView) Unheld() string { return UnheldWhy(v.MockMemDb) }

// MarkMockAsUsed's false is a refusal from a store that serves exactly now,
// for a named mock; and, for any mock, from a store HoldToRound returned that
// could not hold its call to its round (UnheldWhy), or a view over one,
// though neither offers exact serving: read as bookkeeping, it would have the
// parser answer with a recording the hold never let it read.
func TestConsumeRefusedOnlyUnderExactServingForANamedMock(t *testing.T) {
	// A store that marks a call's round and cannot hold the call to it.
	unheld, _ := HoldToRound(exactStore{exact: true}, NewCallRound(1, nil))
	if UnheldWhy(unheld) == "" {
		t.Fatal("HoldToRound held a call to its round in a store that cannot hold one")
	}
	for _, tc := range []struct {
		name    string
		db      MockMemDb
		mock    string
		exact   bool
		refused bool
	}{
		{"a store that cannot serve exactly", struct{ MockMemDb }{}, "m", false, false},
		{"exact serving off", exactStore{exact: false}, "m", false, false},
		{"exact serving on", exactStore{exact: true}, "m", true, true},
		{"exact serving on, a mock with no name", exactStore{exact: true}, "", true, false},
		{"no store", nil, "m", false, false},
		{"a store held to no round", unheld, "m", false, true},
		{"a store held to no round, a mock with no name", unheld, "", false, true},
		{"a view over a store held to no round", unheldView{unheld}, "m", false, true},
		{"a view that tells nothing held", unheldView{exactStore{exact: false}}, "m", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := ExactServing(tc.db); got != tc.exact {
				t.Fatalf("ExactServing = %v, want %v", got, tc.exact)
			}
			if got := ConsumeRefused(tc.db, tc.mock); got != tc.refused {
				t.Fatalf("ConsumeRefused = %v, want %v", got, tc.refused)
			}
		})
	}
}
