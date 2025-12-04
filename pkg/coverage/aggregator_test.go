package coverage

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestMockUsageTracker(t *testing.T) {
	tests := []struct {
		name string
		fn   func(t *testing.T)
	}{
		{
			name: "RegisterMock and MarkUsed",
			fn: func(t *testing.T) {
				tracker := NewMockUsageTracker()
				tracker.RegisterMock("mock1", "API Call", "GET", "/users", "testset1")
				tracker.MarkMockUsed("mock1")

				if !tracker.IsUsed("mock1") {
					t.Error("expected mock1 to be marked as used")
				}
			},
		},
		{
			name: "GetAllMocks",
			fn: func(t *testing.T) {
				tracker := NewMockUsageTracker()
				tracker.RegisterMock("mock1", "API 1", "GET", "/users", "testset1")
				tracker.RegisterMock("mock2", "API 2", "POST", "/posts", "testset1")

				mocks := tracker.GetAllMocks()
				if len(mocks) != 2 {
					t.Errorf("expected 2 mocks, got %d", len(mocks))
				}
			},
		},
		{
			name: "GetUsedMocks",
			fn: func(t *testing.T) {
				tracker := NewMockUsageTracker()
				tracker.RegisterMock("mock1", "API 1", "GET", "/users", "testset1")
				tracker.RegisterMock("mock2", "API 2", "POST", "/posts", "testset1")
				tracker.MarkMockUsed("mock1")

				used := tracker.GetUsedMocks()
				if len(used) != 1 || used[0] != "mock1" {
					t.Errorf("expected 1 used mock (mock1), got %v", used)
				}
			},
		},
		{
			name: "GetMissedMocks",
			fn: func(t *testing.T) {
				tracker := NewMockUsageTracker()
				tracker.RegisterMock("mock1", "API 1", "GET", "/users", "testset1")
				tracker.RegisterMock("mock2", "API 2", "POST", "/posts", "testset1")
				tracker.MarkMockUsed("mock1")

				missed := tracker.GetMissedMocks()
				if len(missed) != 1 || missed[0] != "mock2" {
					t.Errorf("expected 1 missed mock (mock2), got %v", missed)
				}
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, tc.fn)
	}
}

func TestAggregator(t *testing.T) {
	tests := []struct {
		name string
		fn   func(t *testing.T)
	}{
		{
			name: "Compute with 100% coverage",
			fn: func(t *testing.T) {
				agg := NewAggregator()
				agg.RegisterMock("mock1", "API 1", "GET", "/users", "testset1")
				agg.RegisterMock("mock2", "API 2", "POST", "/posts", "testset1")
				agg.MarkMockUsed("mock1")
				agg.MarkMockUsed("mock2")

				stats := agg.Compute("test1", "testset1")
				if stats.TotalMocks != 2 {
					t.Errorf("expected 2 total mocks, got %d", stats.TotalMocks)
				}
				if stats.ReplayedMocks != 2 {
					t.Errorf("expected 2 replayed mocks, got %d", stats.ReplayedMocks)
				}
				if stats.CoveragePercent != 100 {
					t.Errorf("expected 100%% coverage, got %.2f%%", stats.CoveragePercent)
				}
			},
		},
		{
			name: "Compute with 50% coverage",
			fn: func(t *testing.T) {
				agg := NewAggregator()
				agg.RegisterMock("mock1", "API 1", "GET", "/users", "testset1")
				agg.RegisterMock("mock2", "API 2", "POST", "/posts", "testset1")
				agg.MarkMockUsed("mock1")

				stats := agg.Compute("test1", "testset1")
				if stats.CoveragePercent != 50 {
					t.Errorf("expected 50%% coverage, got %.2f%%", stats.CoveragePercent)
				}
				if stats.MissedMocks != 1 {
					t.Errorf("expected 1 missed mock, got %d", stats.MissedMocks)
				}
			},
		},
		{
			name: "Group mocks by endpoint",
			fn: func(t *testing.T) {
				agg := NewAggregator()
				agg.RegisterMock("mock1", "API 1", "GET", "/users", "testset1")
				agg.RegisterMock("mock2", "API 2", "GET", "/users", "testset1")
				agg.RegisterMock("mock3", "API 3", "POST", "/posts", "testset1")
				agg.MarkMockUsed("mock1")
				agg.MarkMockUsed("mock3")

				stats := agg.Compute("test1", "testset1")
				if len(stats.Endpoints) != 2 {
					t.Errorf("expected 2 unique endpoints, got %d", len(stats.Endpoints))
				}

				// Check GET /users endpoint
				if endpoint, ok := stats.Endpoints["GET /users"]; ok {
					if endpoint.Total != 2 {
						t.Errorf("expected GET /users to have 2 mocks, got %d", endpoint.Total)
					}
					if endpoint.Replayed != 1 {
						t.Errorf("expected GET /users to have 1 replayed mock, got %d", endpoint.Replayed)
					}
				} else {
					t.Error("expected GET /users endpoint to exist")
				}
			},
		},
		{
			name: "Compute with zero mocks",
			fn: func(t *testing.T) {
				agg := NewAggregator()
				stats := agg.Compute("test1", "testset1")
				if stats.TotalMocks != 0 {
					t.Errorf("expected 0 total mocks, got %d", stats.TotalMocks)
				}
				if stats.CoveragePercent != 0 {
					t.Errorf("expected 0%% coverage, got %.2f%%", stats.CoveragePercent)
				}
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, tc.fn)
	}
}

func TestReporter(t *testing.T) {
	tests := []struct {
		name string
		fn   func(t *testing.T)
	}{
		{
			name: "ToJSON generates valid JSON",
			fn: func(t *testing.T) {
				agg := NewAggregator()
				agg.RegisterMock("mock1", "API 1", "GET", "/users", "testset1")
				agg.MarkMockUsed("mock1")
				stats := agg.Compute("test1", "testset1")
				reporter := NewReporter(stats)

				jsonStr, err := reporter.ToJSON()
				if err != nil {
					t.Fatalf("failed to generate JSON: %v", err)
				}

				// Verify it's valid JSON
				var result CoverageStats
				if err := json.Unmarshal([]byte(jsonStr), &result); err != nil {
					t.Fatalf("invalid JSON generated: %v", err)
				}

				if result.TotalMocks != 1 {
					t.Errorf("expected 1 total mock in JSON, got %d", result.TotalMocks)
				}
				if result.ReplayedMocks != 1 {
					t.Errorf("expected 1 replayed mock in JSON, got %d", result.ReplayedMocks)
				}
			},
		},
		{
			name: "ToText generates human-readable report",
			fn: func(t *testing.T) {
				agg := NewAggregator()
				agg.RegisterMock("mock1", "API 1", "GET", "/users", "testset1")
				agg.RegisterMock("mock2", "API 2", "POST", "/posts", "testset1")
				agg.MarkMockUsed("mock1")
				stats := agg.Compute("test1", "testset1")
				reporter := NewReporter(stats)

				text := reporter.ToText()

				// Check for key sections
				if !strings.Contains(text, "MOCK REPLAY COVERAGE REPORT") {
					t.Error("expected report title in text output")
				}
				if !strings.Contains(text, "Overall Coverage") {
					t.Error("expected coverage summary in text output")
				}
				if !strings.Contains(text, "Endpoint Coverage Breakdown") {
					t.Error("expected endpoint breakdown in text output")
				}
				if !strings.Contains(text, "Missed Mocks") {
					t.Error("expected missed mocks section in text output")
				}
				if !strings.Contains(text, "50.0%") {
					t.Error("expected 50% coverage in text output")
				}
			},
		},
		{
			name: "ToHTML generates valid HTML",
			fn: func(t *testing.T) {
				agg := NewAggregator()
				agg.RegisterMock("mock1", "API 1", "GET", "/users", "testset1")
				agg.MarkMockUsed("mock1")
				stats := agg.Compute("test1", "testset1")
				reporter := NewReporter(stats)

				html := reporter.ToHTML()

				// Check for key HTML elements
				if !strings.Contains(html, "<!DOCTYPE html>") {
					t.Error("expected DOCTYPE in HTML output")
				}
				if !strings.Contains(html, "<title>Mock Replay Coverage Report</title>") {
					t.Error("expected title tag in HTML output")
				}
				if !strings.Contains(html, "Overall Coverage") {
					t.Error("expected coverage summary in HTML output")
				}
				if !strings.Contains(html, "100.00%") {
					t.Error("expected 100% coverage in HTML output")
				}
			},
		},
		{
			name: "ToText includes all metrics",
			fn: func(t *testing.T) {
				agg := NewAggregator()
				agg.RegisterMock("mock1", "API 1", "GET", "/users", "testset1")
				agg.RegisterMock("mock2", "API 2", "POST", "/posts", "testset1")
				agg.MarkMockUsed("mock1")
				stats := agg.Compute("test-run-123", "testset1")
				reporter := NewReporter(stats)

				text := reporter.ToText()

				// Check metrics
				if !strings.Contains(text, "Used Mocks: 1") {
					t.Error("expected used mocks count in text")
				}
				if !strings.Contains(text, "Missed Mocks: 1") {
					t.Error("expected missed mocks count in text")
				}
				if !strings.Contains(text, "test-run-123") {
					t.Error("expected test run ID in text")
				}
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, tc.fn)
	}
}

func BenchmarkAggregatorCompute(b *testing.B) {
	agg := NewAggregator()
	// Register 1000 mocks
	for i := 0; i < 1000; i++ {
		agg.RegisterMock("mock"+string(rune(i)), "API", "GET", "/endpoint", "testset")
	}
	// Mark half as used
	for i := 0; i < 500; i++ {
		agg.MarkMockUsed("mock" + string(rune(i)))
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		agg.Compute("test", "testset")
	}
}
