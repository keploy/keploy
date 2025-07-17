//go:build linux

package proxy

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"

	"go.keploy.io/server/v2/pkg/models"
	"go.uber.org/zap"
)

type MockManager struct {
	filtered      *TreeDb
	unfiltered    *TreeDb
	logger        *zap.Logger
	consumedMocks sync.Map
	mu            sync.RWMutex

	// A map to store unfiltered mocks by their command type for fast lookups.
	unfilteredByCommandType map[string][]*models.Mock
	// A dedicated slice for config mocks for fast handshake simulation.
	unfilteredConfigMocks []*models.Mock
}

func NewMockManager(filtered, unfiltered *TreeDb, logger *zap.Logger) *MockManager {
	return &MockManager{
		filtered:                filtered,
		unfiltered:              unfiltered,
		logger:                  logger,
		consumedMocks:           sync.Map{},
		unfilteredByCommandType: make(map[string][]*models.Mock),
		unfilteredConfigMocks:   make([]*models.Mock, 0),
	}
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
	}
}

func (m *MockManager) SetUnFilteredMocks(mocks []*models.Mock) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.unfiltered.deleteAll()
	// Clear the old caches before repopulating
	m.unfilteredByCommandType = make(map[string][]*models.Mock)
	m.unfilteredConfigMocks = make([]*models.Mock, 0)

	for index, mock := range mocks {
		// if the sortOrder is already set (!= 0) then we shouldn't override it,
		// as this would be a consequence of the mock being matched in previous testcases,
		// which is done to put the mock in the last when we are processing the mock list for getting a match.
		if mock.TestModeInfo.SortOrder == 0 {
			mock.TestModeInfo.SortOrder = int64(index) + 1
		}
		mock.TestModeInfo.ID = index
		m.unfiltered.insert(mock.TestModeInfo, mock)

		if mock.Kind != "MySQL" {
			continue
		}

		// Categorize the mock for fast retrieval later
		if mock.Spec.Metadata["type"] == "config" {
			m.unfilteredConfigMocks = append(m.unfilteredConfigMocks, mock)
		} else if len(mock.Spec.MySQLRequests) > 0 {
			cmdType := mock.Spec.MySQLRequests[0].Header.Type
			m.unfilteredByCommandType[cmdType] = append(m.unfilteredByCommandType[cmdType], mock)
		}
	}
}

func (m *MockManager) GetUnFilteredConfigMocks() ([]*models.Mock, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	result := make([]*models.Mock, len(m.unfilteredConfigMocks))
	copy(result, m.unfilteredConfigMocks)
	return result, nil
}

func (m *MockManager) GetUnFilteredMocksByType(commandType string) ([]*models.Mock, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	mocks := m.unfilteredByCommandType[commandType]
	result := make([]*models.Mock, len(mocks))
	for i, mock := range mocks {
		mockCopy := *mock
		result[i] = &mockCopy
	}
	return result, nil
}

func (m *MockManager) GetFilteredMocks() ([]*models.Mock, error) {
	rawItems := m.filtered.getAll()

	result := make([]*models.Mock, 0, len(rawItems))

	for _, item := range rawItems {
		originalMock, ok := item.(*models.Mock)
		if !ok || originalMock == nil {
			continue
		}

		mockCopy := *originalMock
		result = append(result, &mockCopy)
	}

	return result, nil
}

func (m *MockManager) GetUnFilteredMocks() ([]*models.Mock, error) {
	rawItems := m.unfiltered.getAll()

	result := make([]*models.Mock, 0, len(rawItems))

	for _, item := range rawItems {
		originalMock, ok := item.(*models.Mock)
		if !ok || originalMock == nil {
			continue
		}

		mockCopy := *originalMock
		result = append(result, &mockCopy)
	}

	return result, nil
}

func (m *MockManager) UpdateUnFilteredMock(old *models.Mock, new *models.Mock) bool {
	m.mu.Lock()
	defer m.mu.Unlock()

	updated := m.unfiltered.update(old.TestModeInfo, new.TestModeInfo, new)
	if updated {
		if old.Kind == "MySQL" {
			if old.Spec.Metadata["type"] == "config" {
				for i, cacheMock := range m.unfilteredConfigMocks {
					if cacheMock.TestModeInfo.ID == old.TestModeInfo.ID {
						m.unfilteredConfigMocks[i] = new
						break
					}
				}
			} else if len(old.Spec.MySQLRequests) > 0 {
				cmdType := old.Spec.MySQLRequests[0].Header.Type
				if mocks, ok := m.unfilteredByCommandType[cmdType]; ok {
					for i, cacheMock := range mocks {
						if cacheMock.TestModeInfo.ID == old.TestModeInfo.ID {
							mocks[i] = new
							break
						}
					}
				}
			}
		}

		if err := m.flagMockAsUsed(models.MockState{
			Name:       (*new).Name,
			Usage:      models.Updated,
			IsFiltered: (*new).TestModeInfo.IsFiltered,
			SortOrder:  (*new).TestModeInfo.SortOrder,
		}); err != nil {
			m.logger.Error("failed to flag mock as used", zap.Error(err))
		}
	}
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
	return isDeleted
}

func (m *MockManager) DeleteUnFilteredMock(mock models.Mock) bool {
	m.mu.Lock()
	defer m.mu.Unlock()

	isDeleted := m.unfiltered.delete(mock.TestModeInfo)
	if isDeleted {
		if mock.Kind == "MySQL" {
			if mock.Spec.Metadata["type"] == "config" {
				// This was a config mock, find and remove it from the config slice.
				for i, cacheMock := range m.unfilteredConfigMocks {
					if cacheMock.TestModeInfo.ID == mock.TestModeInfo.ID {
						m.unfilteredConfigMocks = append(m.unfilteredConfigMocks[:i], m.unfilteredConfigMocks[i+1:]...)
						break
					}
				}
			} else if len(mock.Spec.MySQLRequests) > 0 {
				// This was a command mock, find and remove it from the command map.
				cmdType := mock.Spec.MySQLRequests[0].Header.Type
				if mocks, ok := m.unfilteredByCommandType[cmdType]; ok {
					for i, cacheMock := range mocks {
						if cacheMock.TestModeInfo.ID == mock.TestModeInfo.ID {
							m.unfilteredByCommandType[cmdType] = append(mocks[:i], mocks[i+1:]...)
							break
						}
					}
				}
			}
		}

		if err := m.flagMockAsUsed(models.MockState{
			Name:       mock.Name,
			Usage:      models.Deleted,
			IsFiltered: mock.TestModeInfo.IsFiltered,
			SortOrder:  mock.TestModeInfo.SortOrder,
		}); err != nil {
			m.logger.Error("failed to flag mock as used", zap.Error(err))
		}
	}
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
