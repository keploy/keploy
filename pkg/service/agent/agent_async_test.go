package agent

import (
	"testing"

	"go.keploy.io/server/v3/pkg/models"
)

// TestCollectAsyncMocksFiltersByMetadata proves collectAsyncMocks keeps only
// the async mocks (Spec.Async != nil), preserves their input order, and
// tolerates nil entries. This is the exact subset the agent hands the run-once
// Engine.Load, so a false negative here silently drops async mocks at replay.
func TestCollectAsyncMocksFiltersByMetadata(t *testing.T) {
	a := &models.Mock{Spec: models.MockSpec{Async: &models.AsyncMeta{Lane: "L"}}}
	b := &models.Mock{Spec: models.MockSpec{Metadata: map[string]string{}}}
	c := &models.Mock{Spec: models.MockSpec{Async: &models.AsyncMeta{Lane: "L"}}}
	got := collectAsyncMocks([]*models.Mock{a, b, c, nil})
	if len(got) != 2 || got[0] != a || got[1] != c {
		t.Fatalf("collectAsyncMocks = %v, want [a c]", got)
	}
}
