package replay

import (
	"testing"

	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// Mocks mapped exclusively to UNSELECTED tests are deliberately not loaded by
// mockdb; they must not be reported as pruned/missing on selective runs.
func TestReportMissingMappedMocks_SelectionAware(t *testing.T) {
	r := &Replayer{logger: zap.NewNop(), mockMismatchFailures: NewTestFailureStore()}
	mappings := map[string][]models.MockEntry{
		"test-1": {{Name: "mock-1"}}, // selected, loaded → no row
		"test-2": {{Name: "mock-2"}}, // selected, missing → row
		"test-3": {{Name: "mock-3"}}, // NOT selected, not loaded → no row
	}
	loaded := []*models.Mock{{Name: "mock-1"}}
	selected := map[string]bool{"test-1": true, "test-2": true}

	r.reportMissingMappedMocks("ts-1", mappings, selected, loaded, nil)

	fails := r.mockMismatchFailures.GetFailures()
	if len(fails) != 1 {
		t.Fatalf("expected exactly one missing-mock row, got %d: %+v", len(fails), fails)
	}
	if fails[0].TestID != "test-2" || len(fails[0].ExpectedMocks) != 1 || fails[0].ExpectedMocks[0] != "mock-2" {
		t.Errorf("unexpected row: %+v", fails[0])
	}
}

// With no selection, every mapped test participates.
func TestReportMissingMappedMocks_FullRun(t *testing.T) {
	r := &Replayer{logger: zap.NewNop(), mockMismatchFailures: NewTestFailureStore()}
	mappings := map[string][]models.MockEntry{
		"test-1": {{Name: "mock-1"}},
		"test-2": {{Name: "mock-2"}},
	}
	r.reportMissingMappedMocks("ts-1", mappings, nil, []*models.Mock{{Name: "mock-1"}}, nil)
	fails := r.mockMismatchFailures.GetFailures()
	if len(fails) != 1 || fails[0].TestID != "test-2" {
		t.Fatalf("expected one row for test-2, got %+v", fails)
	}
}
