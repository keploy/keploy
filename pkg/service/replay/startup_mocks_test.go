package replay

import (
	"testing"

	"go.keploy.io/server/v3/pkg/models"
)

func TestSetStartupMocks(t *testing.T) {
	t.Run("records boot traffic, deduped and order-stable", func(t *testing.T) {
		m := &models.Mapping{TestSetID: "test-set-0"}
		setStartupMocks(m, []models.MockState{
			{Name: "mock-0", Kind: models.Postgres, ReqTimestampMock: "2026-08-24T10:00:00Z"},
			{Name: "mock-1", Kind: models.REDIS},
			{Name: "mock-0", Kind: models.Postgres}, // duplicate consumption
		})
		if len(m.Startup) != 2 {
			t.Fatalf("expected 2 deduped entries, got %+v", m.Startup)
		}
		if m.Startup[0].Name != "mock-0" || m.Startup[1].Name != "mock-1" {
			t.Fatalf("order not stable: %+v", m.Startup)
		}
		if m.Startup[0].Kind != string(models.Postgres) {
			t.Fatalf("kind lost: %+v", m.Startup[0])
		}
		// Timestamp derived from the RFC3339 record time.
		if m.Startup[0].Timestamp == 0 {
			t.Fatalf("timestamp not derived from ReqTimestampMock: %+v", m.Startup[0])
		}
	})

	t.Run("no consumed mocks leaves the section unset so omitempty holds", func(t *testing.T) {
		m := &models.Mapping{TestSetID: "test-set-0"}
		setStartupMocks(m, nil)
		if m.Startup != nil {
			t.Fatalf("expected nil, got %+v", m.Startup)
		}
	})

	t.Run("nil mapping is a no-op", func(t *testing.T) {
		setStartupMocks(nil, []models.MockState{{Name: "m"}})
	})

	t.Run("unnamed mocks are skipped", func(t *testing.T) {
		m := &models.Mapping{}
		setStartupMocks(m, []models.MockState{{Name: ""}, {Name: "real"}})
		if len(m.Startup) != 1 || m.Startup[0].Name != "real" {
			t.Fatalf("got %+v", m.Startup)
		}
	})
}

func TestMergeStartupMockNames(t *testing.T) {
	t.Run("per-test first, then startup", func(t *testing.T) {
		got := mergeStartupMockNames(
			[]models.MockEntry{{Name: "t1"}, {Name: "t2"}},
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
		got := mergeStartupMockNames(
			[]models.MockEntry{{Name: "shared"}, {Name: "t1"}},
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
		got := mergeStartupMockNames([]models.MockEntry{{Name: "t1"}}, nil)
		if len(got) != 1 || got[0] != "t1" {
			t.Fatalf("got %v", got)
		}
	})

	// A test with no mocks of its own still needs the app's boot mocks.
	t.Run("startup only", func(t *testing.T) {
		got := mergeStartupMockNames(nil, []string{"s1"})
		if len(got) != 1 || got[0] != "s1" {
			t.Fatalf("got %v", got)
		}
	})

	t.Run("both empty yields an empty non-nil slice", func(t *testing.T) {
		got := mergeStartupMockNames(nil, nil)
		if got == nil || len(got) != 0 {
			t.Fatalf("got %v", got)
		}
	})
}
