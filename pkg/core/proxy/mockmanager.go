//go:build linux

package proxy

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"

	"go.keploy.io/server/v2/pkg/models"
	"go.uber.org/zap"
)

type MockManager struct {
	filtered      *TreeDb
	unfiltered    *TreeDb
	logger        *zap.Logger
	consumedMocks sync.Map
	rev           uint64 // monotonically increasing revision of in-memory view

}

func NewMockManager(filtered, unfiltered *TreeDb, logger *zap.Logger) *MockManager {
	return &MockManager{
		filtered:      filtered,
		unfiltered:    unfiltered,
		logger:        logger,
		consumedMocks: sync.Map{},
	}
}

// Revision returns a monotonically increasing number that changes
// whenever the in-memory set or sort order of mocks is mutated.
func (m *MockManager) Revision() uint64 {
	return atomic.LoadUint64(&m.rev)
}

func (m *MockManager) bumpRevision() {
	atomic.AddUint64(&m.rev, 1)
}

func (m *MockManager) GetFilteredMocks() ([]*models.Mock, error) {
	results := make([]*models.Mock, 0, 64) // small cap; grows if needed
	m.filtered.rangeValues(func(v interface{}) bool {
		if mock, ok := v.(*models.Mock); ok && mock != nil {
			// Return pointers directly; callers must treat as read-only.
			results = append(results, mock)
		}
		return true
	})
	return results, nil
}

func (m *MockManager) GetUnFilteredMocks() ([]*models.Mock, error) {
	results := make([]*models.Mock, 0, 128)
	m.unfiltered.rangeValues(func(v interface{}) bool {
		if mock, ok := v.(*models.Mock); ok && mock != nil {
			results = append(results, mock)
		}
		return true
	})
	return results, nil
}

// GetMySQLCounts computes counts without building slices.
func (m *MockManager) GetMySQLCounts() (total, config, data int) {
	m.unfiltered.rangeValues(func(v interface{}) bool {
		mock, ok := v.(*models.Mock)
		if !ok || mock == nil || mock.Kind != models.MySQL {
			return true
		}
		total++
		if mock.Spec.Metadata["type"] == "config" {
			config++
		} else {
			data++
		}
		return true
	})
	return
}

func (m *MockManager) SetFilteredMocks(mocks []*models.Mock) {
	m.filtered.deleteAll()
	for index, mock := range mocks {
		// if the sortOrder is already set (!= 0) then we shouldn't override it,
		// as this would be a consequence of the mock being matched in previous testcases,
		// which is done to put the mock in the last when we are processing the mock list for getting a match.
		if mock.TestModeInfo.SortOrder == 0 {
			mock.TestModeInfo.SortOrder = int64(index) + 1
		}
		mock.TestModeInfo.ID = index
		m.filtered.insert(mock.TestModeInfo, mock)
		m.bumpRevision()
	}
}

func (m *MockManager) SetUnFilteredMocks(mocks []*models.Mock) {
	m.unfiltered.deleteAll()
	for index, mock := range mocks {
		// if the sortOrder is already set (!= 0) then we shouldn't override it,
		// as this would be a consequence of the mock being matched in previous testcases,
		// which is done to put the mock in the last when we are processing the mock list for getting a match.
		if mock.TestModeInfo.SortOrder == 0 {
			mock.TestModeInfo.SortOrder = int64(index) + 1
		}
		mock.TestModeInfo.ID = index
		m.unfiltered.insert(mock.TestModeInfo, mock)
		m.bumpRevision()
	}
}

func (m *MockManager) UpdateUnFilteredMock(old *models.Mock, new *models.Mock) bool {
	updated := m.unfiltered.update(old.TestModeInfo, new.TestModeInfo, new)
	if updated {
		// mark the unfiltered mock as used for the current simulated test-case
		if err := m.flagMockAsUsed(models.MockState{
			Name:       (*new).Name,
			Usage:      models.Updated,
			IsFiltered: (*new).TestModeInfo.IsFiltered,
			SortOrder:  (*new).TestModeInfo.SortOrder,
		}); err != nil {
			m.logger.Error("failed to flag mock as used", zap.Error(err))
		}
	}
	m.bumpRevision()
	return updated
}

func (m *MockManager) flagMockAsUsed(mock models.MockState) error {
	if mock.Name == "" {
		return fmt.Errorf("mock is empty")
	}
	m.consumedMocks.Store(mock.Name, mock)
	return nil
}

func (m *MockManager) DeleteFilteredMock(mock models.Mock) bool {
	isDeleted := m.filtered.delete(mock.TestModeInfo)
	if isDeleted {
		if err := m.flagMockAsUsed(models.MockState{
			Name:       mock.Name,
			Usage:      models.Deleted,
			IsFiltered: mock.TestModeInfo.IsFiltered,
			SortOrder:  mock.TestModeInfo.SortOrder,
		}); err != nil {
			m.logger.Error("failed to flag mock as used", zap.Error(err))
		}
	}
	m.bumpRevision()
	return isDeleted
}

func (m *MockManager) DeleteUnFilteredMock(mock models.Mock) bool {
	isDeleted := m.unfiltered.delete(mock.TestModeInfo)
	if isDeleted {
		if err := m.flagMockAsUsed(models.MockState{
			Name:       mock.Name,
			Usage:      models.Deleted,
			IsFiltered: mock.TestModeInfo.IsFiltered,
			SortOrder:  mock.TestModeInfo.SortOrder,
		}); err != nil {
			m.logger.Error("failed to flag mock as used", zap.Error(err))
		}
	}
	m.bumpRevision()
	return isDeleted
}

func (m *MockManager) GetConsumedMocks() []models.MockState {
	var keys []models.MockState
	m.consumedMocks.Range(func(key, val interface{}) bool {
		if _, ok := key.(string); ok {
			keys = append(keys, val.(models.MockState))
			m.consumedMocks.Delete(key)
		}
		return true
	})
	sort.Slice(keys, func(i, j int) bool {
		numI, _ := strconv.Atoi(strings.Split(keys[i].Name, "-")[1])
		numJ, _ := strconv.Atoi(strings.Split(keys[j].Name, "-")[1])
		return numI < numJ
	})
	for key := range keys {
		m.consumedMocks.Delete(key)
	}
	return keys
}
