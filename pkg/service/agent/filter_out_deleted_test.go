package agent

import (
	"testing"

	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// filterOutDeleted must not write the mocks it is given. With no test window
// the filters pass the stored mocks through, and a stored mock staged by an
// earlier call is in the proxy's pools, where matchers copy it concurrently.
// It returns a copy that carries the recorded consumption state instead.
func TestFilterOutDeletedDoesNotWriteItsInput(t *testing.T) {
	a := &Agent{logger: zap.NewNop()}
	updated := &models.Mock{Name: "updated", TestModeInfo: models.TestModeInfo{IsFiltered: true, SortOrder: 3}}
	same := &models.Mock{Name: "same", TestModeInfo: models.TestModeInfo{IsFiltered: true, SortOrder: 4}}
	untouched := &models.Mock{Name: "untouched", TestModeInfo: models.TestModeInfo{IsFiltered: true, SortOrder: 5}}
	deleted := &models.Mock{Name: "deleted", TestModeInfo: models.TestModeInfo{IsFiltered: true, SortOrder: 6}}
	consumed := map[string]models.MockState{
		"updated": {Name: "updated", Usage: models.Updated, IsFiltered: false, SortOrder: 99},
		"same":    {Name: "same", Usage: models.Updated, IsFiltered: true, SortOrder: 4},
		"deleted": {Name: "deleted", Usage: models.Deleted},
	}

	out := a.filterOutDeleted([]*models.Mock{updated, same, untouched, deleted}, consumed)

	if updated.TestModeInfo.IsFiltered != true || updated.TestModeInfo.SortOrder != 3 {
		t.Fatalf("filterOutDeleted wrote its input: %+v", updated.TestModeInfo)
	}
	if len(out) != 3 {
		t.Fatalf("got %d mocks, want 3 (the deleted one dropped)", len(out))
	}
	if out[0] == updated || out[0].Name != "updated" ||
		out[0].TestModeInfo.IsFiltered || out[0].TestModeInfo.SortOrder != 99 {
		t.Fatalf("the updated mock should come back as a copy carrying its consumption state, got %p (input %p) %+v",
			out[0], updated, out[0].TestModeInfo)
	}
	if out[1] != same || out[2] != untouched {
		t.Fatal("a mock whose state does not change should be passed through, not copied")
	}
}
