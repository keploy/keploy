package proxy

import (
	"fmt"
	"go.keploy.io/server/v2/pkg/models"
)

type MockManager struct {
	filtered   *treeDb
	unfiltered *treeDb
}

func NewMockManager(filtered, unfiltered *treeDb) *MockManager {
	return &MockManager{
		filtered:   filtered,
		unfiltered: unfiltered,
	}
}

// For proxy
func (m *MockManager) SetFilteredMocks(mocks []*models.Mock) {
	m.filtered.deleteAll()
	for index, mock := range mocks {
		mock.TestModeInfo.SortOrder = index
		mock.TestModeInfo.Id = index
		m.filtered.insert(mock.TestModeInfo, mock)
	}
}

// For proxy
func (m *MockManager) SetUnFilteredMocks(mocks []*models.Mock) {
	m.unfiltered.deleteAll()
	for index, mock := range mocks {
		mock.TestModeInfo.SortOrder = index
		mock.TestModeInfo.Id = index
		m.unfiltered.insert(mock.TestModeInfo, mock)
	}
}

// For integrations
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
	return updated
}

func (m *MockManager) DeleteFilteredMock(mock *models.Mock) bool {
	isDeleted := m.filtered.delete(mock.TestModeInfo)
	return isDeleted
}

func (m *MockManager) DeleteUnFilteredMock(mock *models.Mock) bool {
	isDeleted := m.unfiltered.delete(mock.TestModeInfo)
	return isDeleted
}
