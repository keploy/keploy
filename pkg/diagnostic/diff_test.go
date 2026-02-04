package diagnostic

import (
	"testing"

	"go.keploy.io/server/v3/pkg/models"
)

func TestComputeJSONDiff_ClassifyAndScore(t *testing.T) {
	exp := `{"id":1,"now":"2020-01-01T00:00:00Z","name":"old"}`
	act := `{"id":1,"now":"2026-01-22T00:00:00Z","name":"new"}`

	report, err := ComputeJSONDiff(exp, act, true)
	if err != nil {
		t.Fatalf("ComputeJSONDiff error: %v", err)
	}
	if report == nil {
		t.Fatalf("expected report, got nil")
	}
	if report.Category != models.CategoryDataUpdate {
		t.Fatalf("expected category %s, got %s", models.CategoryDataUpdate, report.Category)
	}
	if report.Confidence <= 0 || report.Confidence > 100 {
		t.Fatalf("unexpected confidence: %d", report.Confidence)
	}

	var hasDynamic bool
	var hasDataUpdate bool
	for _, e := range report.Entries {
		if e.Category == models.CategoryDynamicNoise {
			hasDynamic = true
		}
		if e.Category == models.CategoryDataUpdate {
			hasDataUpdate = true
		}
	}
	if !hasDynamic {
		t.Fatalf("expected at least one dynamic noise entry")
	}
	if !hasDataUpdate {
		t.Fatalf("expected at least one data update entry")
	}
}
