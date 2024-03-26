package proxy

import (
	"fmt"
	"sync"

	"go.keploy.io/server/v2/pkg/models"
	"go.uber.org/zap"
)

type MockManager struct {
	filtered                *TreeDb
	unfiltered              *TreeDb
	logger                  *zap.Logger
	utilizedFilteredMocks   sync.Map
	utilizedUnFilteredMocks sync.Map
}

func NewMockManager(filtered, unfiltered *TreeDb, logger *zap.Logger) *MockManager {
	return &MockManager{
		filtered:                filtered,
		unfiltered:              unfiltered,
		logger:                  logger,
		utilizedFilteredMocks:   sync.Map{},
		utilizedUnFilteredMocks: sync.Map{},
	}
}

func (m *MockManager) SetFilteredMocks(mocks []*models.Mock) {
	m.filtered.deleteAll()
	m.utilizedFilteredMocks = sync.Map{}
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
	for _, m := range mocks {
		if mock, ok := m.(*models.Mock); ok {
			tcsMocks = append(tcsMocks, mock)
		} else {
			return nil, fmt.Errorf("expected mock instance, got %v", m)
		}
	}
	return tcsMocks, nil
}

func (m *MockManager) GetUnFilteredMocks() ([]*models.Mock, error) {
	var configMocks []*models.Mock
	mocks := m.unfiltered.getAll()
	for _, m := range mocks {
		if mock, ok := m.(*models.Mock); ok {
			configMocks = append(configMocks, mock)
		} else {
			return nil, fmt.Errorf("expected mock instance, got %v", m)
		}
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
	if mockType, ok := mock.Spec.Metadata["type"]; ok && mockType == "config" {
		// mark the unfiltered mock as used for the current simulated test-case
		m.utilizedUnFilteredMocks.Store(mock.Name, true)
	} else {
		m.utilizedFilteredMocks.Store(mock.Name, true)
	}
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

func (m *MockManager) GetConsumedFilteredMocks() []string {
	var keys []string
	m.utilizedFilteredMocks.Range(func(key, _ interface{}) bool {
		if _, ok := key.(string); ok {
			keys = append(keys, key.(string))
		}
		return true
	})
	return keys
}

func (m *MockManager) GetConsumedUnFilteredMocks() []string {
	var keys []string
	m.utilizedUnFilteredMocks.Range(func(key, _ interface{}) bool {
		if _, ok := key.(string); ok {
			keys = append(keys, key.(string))
		}
		return true
	})
	return keys
}
