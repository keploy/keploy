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
}

func NewMockManager(filtered, unfiltered *TreeDb, logger *zap.Logger) *MockManager {
	return &MockManager{
		filtered:      filtered,
		unfiltered:    unfiltered,
		logger:        logger,
		consumedMocks: sync.Map{},
	}
}

func (m *MockManager) SetFilteredMocks(mocks []*models.Mock) {
	m.filtered.deleteAll()
	for index, mock := range mocks {
		if mock.TestModeInfo.SortOrder == 0 {
			mock.TestModeInfo.SortOrder = index
		}
		mock.TestModeInfo.ID = index
		m.filtered.insert(mock.TestModeInfo, mock)
	}
}

func (m *MockManager) SetUnFilteredMocks(mocks []*models.Mock) {
	m.unfiltered.deleteAll()
	for index, mock := range mocks {
		if mock.TestModeInfo.SortOrder == 0 {
			mock.TestModeInfo.SortOrder = index
		}
		mock.TestModeInfo.ID = index
		m.unfiltered.insert(mock.TestModeInfo, mock)
	}
}

func (m *MockManager) GetFilteredMocks() ([]*models.Mock, error) {
	var tcsMocks []*models.Mock
	mocks := m.filtered.getAll()
	//sending copy of mocks instead of actual mocks
	mockCopy, err := localMock(mocks)
	if err != nil {
		return nil, fmt.Errorf("expected mock instance, got %v", m)
	}
	for _, m := range mockCopy {
		tcsMocks = append(tcsMocks, &m)
	}
	return tcsMocks, nil
}

func (m *MockManager) GetUnFilteredMocks() ([]*models.Mock, error) {
	var configMocks []*models.Mock
	mocks := m.unfiltered.getAll()
	//sending copy of mocks instead of actual mocks
	mockCopy, err := localMock(mocks)
	if err != nil {
		return nil, fmt.Errorf("expected mock instance, got %v", m)
	}
	for _, m := range mockCopy {
		configMocks = append(configMocks, &m)
	}
	return configMocks, nil
}

func (m *MockManager) UpdateUnFilteredMock(old *models.Mock, new *models.Mock) bool {
	updated := m.unfiltered.update(old.TestModeInfo, new.TestModeInfo, new)
	if updated {
		// mark the unfiltered mock as used for the current simulated test-case
		go func() {
			if err := m.FlagMockAsUsed(models.MockState{
				Name:       (*new).Name,
				Usage:      models.Updated,
				IsFiltered: (*new).TestModeInfo.IsFiltered,
				SortOrder:  (*new).TestModeInfo.SortOrder,
			}); err != nil {
				m.logger.Error("failed to flag mock as used", zap.Error(err))
			}
		}()
	}
	return updated
}

func (m *MockManager) FlagMockAsUsed(mock models.MockState) error {
	if mock.Name == "" {
		return fmt.Errorf("mock is empty")
	}
	m.consumedMocks.Store(mock.Name, mock)
	return nil
}

func (m *MockManager) DeleteFilteredMock(mock models.Mock) bool {
	isDeleted := m.filtered.delete(mock.TestModeInfo)
	if isDeleted {
		go func() {
			if err := m.FlagMockAsUsed(models.MockState{
				Name:       mock.Name,
				Usage:      models.Deleted,
				IsFiltered: mock.TestModeInfo.IsFiltered,
				SortOrder:  mock.TestModeInfo.SortOrder,
			}); err != nil {
				m.logger.Error("failed to flag mock as used", zap.Error(err))
			}
		}()
	}
	return isDeleted
}

func (m *MockManager) DeleteUnFilteredMock(mock models.Mock) bool {
	isDeleted := m.unfiltered.delete(mock.TestModeInfo)
	if isDeleted {
		go func() {
			if err := m.FlagMockAsUsed(models.MockState{
				Name:       mock.Name,
				Usage:      models.Deleted,
				IsFiltered: mock.TestModeInfo.IsFiltered,
				SortOrder:  mock.TestModeInfo.SortOrder,
			}); err != nil {
				m.logger.Error("failed to flag mock as used", zap.Error(err))
			}
		}()
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
