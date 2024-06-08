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
		mock.TestModeInfo.SortOrder = index
		mock.TestModeInfo.ID = index
		m.filtered.insert(mock.TestModeInfo, mock)
	}
}

func (m *MockManager) SetUnFilteredMocks(mocks []*models.Mock) {
	m.unfiltered.deleteAll()
	for index, mock := range mocks {
		mock.TestModeInfo.SortOrder = index
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
			if err := m.FlagMockAsUsed(old); err != nil {
				m.logger.Error("failed to flag mock as used", zap.Error(err))
			}
		}()
	}
	return updated
}

func (m *MockManager) FlagMockAsUsed(mock *models.Mock) error {
	if mock == nil {
		return fmt.Errorf("mock is empty")
	}
	m.consumedMocks.Store(mock.Name, true)
	return nil
}

func (m *MockManager) DeleteFilteredMock(mock *models.Mock) bool {
	isDeleted := m.filtered.delete(mock.TestModeInfo)
	if isDeleted {
		go func() {
			if err := m.FlagMockAsUsed(mock); err != nil {
				m.logger.Error("failed to flag mock as used", zap.Error(err))
			}
		}()
	}
	return isDeleted
}

func (m *MockManager) DeleteUnFilteredMock(mock *models.Mock) bool {
	isDeleted := m.unfiltered.delete(mock.TestModeInfo)
	if isDeleted {
		go func() {
			if err := m.FlagMockAsUsed(mock); err != nil {
				m.logger.Error("failed to flag mock as used", zap.Error(err))
			}
		}()
	}
	return isDeleted
}

func (m *MockManager) GetConsumedMocks() []string {
	var keys []string
	m.consumedMocks.Range(func(key, _ interface{}) bool {
		if _, ok := key.(string); ok {
			keys = append(keys, key.(string))
		}
		return true
	})
	sort.Slice(keys, func(i, j int) bool {
		numI, _ := strconv.Atoi(strings.Split(keys[i], "-")[1])
		numJ, _ := strconv.Atoi(strings.Split(keys[j], "-")[1])
		return numI < numJ
	})
	m.consumedMocks = sync.Map{}
	return keys
}
