// Package coverage provides mock replay coverage tracking and reporting.
package coverage

import (
	"time"
)

// CoverageStats represents statistics for mock coverage.
type CoverageStats struct {
	TotalMocks       int                      `json:"totalMocks"`
	ReplayedMocks    int                      `json:"usedMocks"`
	MissedMocks      int                      `json:"missedMocks"`
	CoveragePercent  float64                  `json:"coveragePercent"`
	Endpoints        map[string]EndpointStats `json:"endpoints"`
	UsedMockIDs      []string                 `json:"usedMockIds"`
	MissedMockIDs    []string                 `json:"missedMockIds"`
	Timestamp        time.Time                `json:"timestamp"`
	TestRunID        string                   `json:"testRunId,omitempty"`
	TestSetID        string                   `json:"testSetId,omitempty"`
}

// EndpointStats represents coverage statistics for a specific endpoint.
type EndpointStats struct {
	Method          string  `json:"method"`
	Path            string  `json:"path"`
	Total           int     `json:"total"`
	Replayed        int     `json:"replayed"`
	CoveragePercent float64 `json:"coveragePercent"`
	MockIDs         []string `json:"mockIds,omitempty"`
}

// MockUsageTracker tracks which mocks were used during replay.
type MockUsageTracker struct {
	UsedMocks  map[string]bool // mockID -> used
	MockMetadata map[string]*MockMetadata // mockID -> metadata
}

// MockMetadata stores information about a mock for coverage reporting.
type MockMetadata struct {
	ID       string
	Name     string
	Method   string
	Path     string
	TestSetID string
}

// NewMockUsageTracker creates a new usage tracker.
func NewMockUsageTracker() *MockUsageTracker {
	return &MockUsageTracker{
		UsedMocks:    make(map[string]bool),
		MockMetadata: make(map[string]*MockMetadata),
	}
}

// MarkMockUsed marks a mock as used/replayed.
func (m *MockUsageTracker) MarkMockUsed(mockID string) {
	m.UsedMocks[mockID] = true
}

// RegisterMock registers a mock for coverage tracking.
func (m *MockUsageTracker) RegisterMock(mockID, name, method, path, testSetID string) {
	m.MockMetadata[mockID] = &MockMetadata{
		ID:        mockID,
		Name:      name,
		Method:    method,
		Path:      path,
		TestSetID: testSetID,
	}
	// Ensure mock is marked as unused initially
	if _, exists := m.UsedMocks[mockID]; !exists {
		m.UsedMocks[mockID] = false
	}
}

// IsUsed returns whether a mock was used.
func (m *MockUsageTracker) IsUsed(mockID string) bool {
	return m.UsedMocks[mockID]
}

// GetAllMocks returns all registered mock IDs.
func (m *MockUsageTracker) GetAllMocks() []string {
	var mocks []string
	for mockID := range m.MockMetadata {
		mocks = append(mocks, mockID)
	}
	return mocks
}

// GetUsedMocks returns all used mock IDs.
func (m *MockUsageTracker) GetUsedMocks() []string {
	var used []string
	for mockID := range m.UsedMocks {
		if m.UsedMocks[mockID] {
			used = append(used, mockID)
		}
	}
	return used
}

// GetMissedMocks returns all unused mock IDs.
func (m *MockUsageTracker) GetMissedMocks() []string {
	var missed []string
	for mockID := range m.UsedMocks {
		if !m.UsedMocks[mockID] {
			missed = append(missed, mockID)
		}
	}
	return missed
}
