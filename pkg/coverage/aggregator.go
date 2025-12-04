// Package coverage provides mock replay coverage tracking and reporting.
// It tracks which mocks were used during test replay execution and generates
// coverage statistics, enabling users to understand which mocks were exercised
// and which were missed during testing.
//
// The package is designed to be minimally invasive: instrumentation points in
// the replay and matching code call global convenience functions (RegisterMock,
// MarkMockUsed) which delegate to the global aggregator. After test execution,
// the CLI can call coverage.Global.Compute() to generate reports.
package coverage

import (
	"fmt"
	"sort"
	"time"
)

// Aggregator combines mock usage data and generates coverage statistics.
// It wraps a MockUsageTracker to provide higher-level statistics computation.
// Thread-safe via the underlying tracker's synchronization.
type Aggregator struct {
	tracker *MockUsageTracker
}

// NewAggregator creates a new coverage aggregator with an empty tracker.
func NewAggregator() *Aggregator {
	return &Aggregator{
		tracker: NewMockUsageTracker(),
	}
}

// RegisterMock registers a mock in the aggregator with its metadata.
// Should be called once per mock when the test set is loaded.
// Mock ID is used as the unique identifier; name, method, and path are for reporting.
func (a *Aggregator) RegisterMock(mockID, name, method, path, testSetID string) {
	a.tracker.RegisterMock(mockID, name, method, path, testSetID)
}

// MarkMockUsed marks a mock as used/replayed.
// Should be called when the mock is actually matched and used during replay.
func (a *Aggregator) MarkMockUsed(mockID string) {
	a.tracker.MarkMockUsed(mockID)
}

// Compute generates coverage statistics from tracked data.
// It aggregates per-endpoint statistics and calculates overall coverage percentages.
// testRunID and testSetID are included in the report for context (may be empty).
// Returns a populated CoverageStats ready for reporting.
func (a *Aggregator) Compute(testRunID, testSetID string) *CoverageStats {
	stats := &CoverageStats{
		Endpoints:     make(map[string]EndpointStats),
		UsedMockIDs:   a.tracker.GetUsedMocks(),
		MissedMockIDs: a.tracker.GetMissedMocks(),
		Timestamp:     time.Now(),
		TestRunID:     testRunID,
		TestSetID:     testSetID,
	}

	allMocks := a.tracker.GetAllMocks()
	stats.TotalMocks = len(allMocks)
	stats.ReplayedMocks = len(stats.UsedMockIDs)
	stats.MissedMocks = len(stats.MissedMockIDs)

	if stats.TotalMocks > 0 {
		stats.CoveragePercent = float64(stats.ReplayedMocks) / float64(stats.TotalMocks) * 100
	}

	// Group mocks by endpoint
	endpointMap := make(map[string]*EndpointStats)

	for _, mockID := range allMocks {
		metadata := a.tracker.MockMetadata[mockID]
		if metadata == nil {
			continue
		}

		// Create endpoint key (method + path)
		key := fmt.Sprintf("%s %s", metadata.Method, metadata.Path)

		if _, exists := endpointMap[key]; !exists {
			endpointMap[key] = &EndpointStats{
				Method:  metadata.Method,
				Path:    metadata.Path,
				MockIDs: []string{},
			}
		}

		endpoint := endpointMap[key]
		endpoint.Total++
		endpoint.MockIDs = append(endpoint.MockIDs, mockID)

		if a.tracker.IsUsed(mockID) {
			endpoint.Replayed++
		}
	}

	// Calculate endpoint coverage percentages and sort
	for key, endpoint := range endpointMap {
		if endpoint.Total > 0 {
			endpoint.CoveragePercent = float64(endpoint.Replayed) / float64(endpoint.Total) * 100
		}
		sort.Strings(endpoint.MockIDs)
		stats.Endpoints[key] = *endpoint
	}

	return stats
}

// GetTracker returns the underlying mock usage tracker (for advanced use cases).
func (a *Aggregator) GetTracker() *MockUsageTracker {
	return a.tracker
}

// Reset clears all tracked data. Useful for testing or resetting between runs.
func (a *Aggregator) Reset() {
	a.tracker = NewMockUsageTracker()
}

// Global is a process-wide aggregator instance for convenient access during
// replay instrumentation. The CLI and coverage reporting code uses this.
var Global = NewAggregator()

// RegisterMock is a convenience function that registers a mock in the global aggregator.
// Intended to be called during test set load in the replay engine.
func RegisterMock(mockID, name, method, path, testSetID string) {
	Global.RegisterMock(mockID, name, method, path, testSetID)
}

// MarkMockUsed is a convenience function that marks a mock as used in the global aggregator.
// Intended to be called when a mock is successfully matched and used during replay.
func MarkMockUsed(mockID string) {
	Global.MarkMockUsed(mockID)
}
