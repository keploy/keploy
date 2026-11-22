package proxy

import (
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// The user-visible contract is not "the mock is in some slice" but "the startup
// TIER is populated": GetStartupMocks is what the tier-aware dispatcher and the
// mongo startup rescue actually read. A bootstrap mock handed to
// SetMocksWithWindow in the FILTERED slice must land in the startup tree, and
// must not remain in the unfiltered pool.
//
// This is the half the filter-level test cannot see. It also documents the
// accessor shift the fix causes: pre-first-window per-test mocks used to reach
// consumers via GetUnFilteredMocks in lax mode and now reach them via
// GetStartupMocks.
func TestSetMocksWithWindowRoutesPreFirstWindowMocksIntoTheStartupTier(t *testing.T) {
	first := time.Date(2026, 9, 1, 12, 0, 10, 0, time.UTC)
	end := first.Add(2 * time.Second)

	boot := &models.Mock{
		Version: "api.keploy.io/v1beta1",
		Name:    "bootstrap-find-schema",
		Kind:    models.Mongo,
		Spec: models.MockSpec{
			ReqTimestampMock: first.Add(-3 * time.Second), // recorded during app boot
			ResTimestampMock: first.Add(-3*time.Second + time.Millisecond),
		},
		TestModeInfo: models.TestModeInfo{Lifetime: models.LifetimePerTest, IsFiltered: true},
	}

	m := NewMockManager(NewTreeDb(customComparator), NewTreeDb(customComparator), zap.NewNop())
	m.SetMocksWithWindow([]*models.Mock{boot}, nil, first, end)

	startup, err := m.GetStartupMocks()
	if err != nil {
		t.Fatalf("GetStartupMocks: %v", err)
	}
	if len(startup) != 1 {
		t.Fatalf("startup tier holds %d mocks, want 1 — an empty tier is what makes a bootstrap "+
			"query miss with candidates:0 while its mock sits on disk", len(startup))
	}
	if startup[0].Name != boot.Name {
		t.Fatalf("startup tier holds %q, want %q", startup[0].Name, boot.Name)
	}
}
