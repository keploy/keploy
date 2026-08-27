package models

import (
	"encoding/json"
	"testing"

	"gopkg.in/yaml.v3"
)

// TestMappingStartupSectionRoundTrips pins the two properties the startup section
// must have: it survives YAML and JSON round-trips, and its absence leaves an
// existing mapping byte-identical (omitempty), so older files and older readers are
// unaffected.
func TestMappingStartupSectionRoundTrips(t *testing.T) {
	t.Run("absent stays absent", func(t *testing.T) {
		m := Mapping{Version: "v1", Kind: MappingKind, TestSetID: "test-set-0",
			TestCases: []MappedTestCase{{ID: "t-1", Mocks: []MockEntry{{Name: "mock-0", Kind: "Http"}}}}}
		y, err := yaml.Marshal(&m)
		if err != nil {
			t.Fatal(err)
		}
		if got := string(y); contains(got, "startup") {
			t.Fatalf("empty startup section must be omitted, got:\n%s", got)
		}
		j, err := json.Marshal(&m)
		if err != nil {
			t.Fatal(err)
		}
		if contains(string(j), "startup") {
			t.Fatalf("empty startup section must be omitted from JSON, got: %s", j)
		}
	})

	t.Run("present round-trips through yaml", func(t *testing.T) {
		m := Mapping{Version: "v1", Kind: MappingKind, TestSetID: "test-set-0",
			Startup: []MockEntry{{Name: "mock-boot-0", Kind: "Postgres"}}}
		y, err := yaml.Marshal(&m)
		if err != nil {
			t.Fatal(err)
		}
		var back Mapping
		if err := yaml.Unmarshal(y, &back); err != nil {
			t.Fatal(err)
		}
		if len(back.Startup) != 1 || back.Startup[0].Name != "mock-boot-0" {
			t.Fatalf("startup did not round-trip: %+v\nyaml:\n%s", back.Startup, y)
		}
	})

	t.Run("StartupMockNames is nil-safe and skips blanks", func(t *testing.T) {
		var nilM *Mapping
		if got := nilM.StartupMockNames(); got != nil {
			t.Fatalf("nil mapping must yield nil, got %v", got)
		}
		m := &Mapping{Startup: []MockEntry{{Name: "a"}, {Name: ""}, {Name: "b"}}}
		got := m.StartupMockNames()
		if len(got) != 2 || got[0] != "a" || got[1] != "b" {
			t.Fatalf("StartupMockNames() = %v, want [a b]", got)
		}
	})
}

func contains(hay, needle string) bool {
	for i := 0; i+len(needle) <= len(hay); i++ {
		if hay[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}

func TestMergeStartupMockNames(t *testing.T) {
	t.Run("per-test first, then startup", func(t *testing.T) {
		got := MergeStartupMockNames(
			[]MockEntry{{Name: "t1"}, {Name: "t2"}},
			[]string{"s1", "s2"},
		)
		want := []string{"t1", "t2", "s1", "s2"}
		if len(got) != len(want) {
			t.Fatalf("got %v want %v", got, want)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("got %v want %v", got, want)
			}
		}
	})

	// Session/connection-tier mocks are consumed at boot AND kept in the
	// per-test list by upsertActualTestMockMapping's always-keep carve-out, so
	// the overlap is normal and must not produce a duplicate name.
	t.Run("a mock in both sections appears once", func(t *testing.T) {
		got := MergeStartupMockNames(
			[]MockEntry{{Name: "shared"}, {Name: "t1"}},
			[]string{"shared", "s1"},
		)
		want := []string{"shared", "t1", "s1"}
		if len(got) != len(want) {
			t.Fatalf("got %v want %v", got, want)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("got %v want %v", got, want)
			}
		}
	})

	t.Run("no startup mocks leaves the per-test list intact", func(t *testing.T) {
		got := MergeStartupMockNames([]MockEntry{{Name: "t1"}}, nil)
		if len(got) != 1 || got[0] != "t1" {
			t.Fatalf("got %v", got)
		}
	})

	// A test with NO per-test entry must stay empty even when a startup section
	// exists. A non-empty list flips the agent into mapping mode, which would
	// shrink that test's pool to the boot mocks alone; an empty list keeps it on
	// the strict-window / lax-all path it has always relied on.
	t.Run("no per-test entry stays empty, even with startup", func(t *testing.T) {
		if got := MergeStartupMockNames(nil, []string{"s1"}); len(got) != 0 {
			t.Fatalf("expected empty so the agent falls through to the wide pool, got %v", got)
		}
		if got := MergeStartupMockNames([]MockEntry{}, []string{"s1"}); len(got) != 0 {
			t.Fatalf("expected empty for an empty (not nil) entry list, got %v", got)
		}
	})

	// Both empty returns nil. Only emptiness matters to the caller — the agent
	// branches on len(MockMapping) > 0 — so nil and an empty slice are equivalent
	// there, and nil is what the no-per-test-entry guard returns.
	t.Run("both empty yields nothing", func(t *testing.T) {
		if got := MergeStartupMockNames(nil, nil); len(got) != 0 {
			t.Fatalf("got %v", got)
		}
	})
}
