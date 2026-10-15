package integrations

import "testing"

// exactStore is a MockMemDb that says whether it serves exactly.
type exactStore struct {
	MockMemDb
	exact bool
}

func (s exactStore) ExactServing() bool { return s.exact }

// MarkMockAsUsed's false is a refusal only from a store that serves exactly
// now, and only for a named mock.
func TestConsumeRefusedOnlyUnderExactServingForANamedMock(t *testing.T) {
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
