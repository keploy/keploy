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
