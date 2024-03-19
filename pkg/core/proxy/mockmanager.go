package proxy

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"go.keploy.io/server/v2/pkg/models"
)

const (
	filteredMock   = "filtered"
	unFilteredMock = "unfiltered"
	totalMock      = "total"
)

type MockManager struct {
	filtered   *TreeDb
	unfiltered *TreeDb
	// usedMocks contains the name of the mocks as key which were used by the parsers during the test execution.
	//
	// value is an array that will contain the type of mock
	usedMocks map[string][]string
}

func NewMockManager(filtered, unfiltered *TreeDb) *MockManager {
	usedMap := make(map[string][]string)
	return &MockManager{
		filtered:   filtered,
		unfiltered: unfiltered,
		usedMocks:  usedMap,
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
	println("printing the update mocks for name: ", old.Name, "and isUpdated: ", updated)
	if updated {
		// mark the unfiltered mock as used for the current simulated test-case
		m.usedMocks[old.Name] = []string{unFilteredMock, totalMock}
	}
	return updated
}

func (m *MockManager) FlagMockAsUsed(mock *models.Mock) error {
	if mock == nil {
		return fmt.Errorf("mock is empty")
	}

	if mockType, ok := mock.Spec.Metadata["type"]; ok && mockType == "config" {
		// mark the unfiltered mock as used for the current simulated test-case
		m.usedMocks[mock.Name] = []string{unFilteredMock, totalMock}
	} else {
		// mark the filtered mock as used for the current simulated test-case
		m.usedMocks[mock.Name] = []string{filteredMock, totalMock}
	}
	return nil
}

func (m *MockManager) DeleteFilteredMock(mock *models.Mock) bool {
	isDeleted := m.filtered.delete(mock.TestModeInfo)
	if isDeleted {
		// mark the unfiltered mock as used for the current simulated test-case
		m.usedMocks[mock.Name] = []string{filteredMock, totalMock}
	}
	return isDeleted
}

func (m *MockManager) DeleteUnFilteredMock(mock *models.Mock) bool {
	isDeleted := m.unfiltered.delete(mock.TestModeInfo)
	return isDeleted
}

func (m *MockManager) GetConsumedFilteredMocks() []string {
	var allNames []string
	// Extract all names from the map
	for mockName, typeList := range m.usedMocks {
		for _, mockType := range typeList {
			// add mock name which are consumed by the parsers during the test-case simulation.
			// Since, test-case are simulated synchronously, so the order of the mock consumption is preserved.
			if mockType == filteredMock || mockType == unFilteredMock {
				allNames = append(allNames, mockName)
			}
		}
	}

	// Custom sorting function to sort names by sequence number
	sort.Slice(allNames, func(i, j int) bool {
		seqNo1, _ := strconv.Atoi(strings.Split(allNames[i], "-")[1])
		seqNo2, _ := strconv.Atoi(strings.Split(allNames[j], "-")[1])
		return seqNo1 < seqNo2
	})

	// add the consumed filtered mocks into the total consumed mocks
	for mockName, typeList := range m.usedMocks {
		for indx, mockType := range typeList {
			// reset the consumed unfiltered slice for the test-case simulation.
			if mockType == unFilteredMock || mockType == filteredMock {
				m.usedMocks[mockName] = append(typeList[:indx], typeList[indx+1:]...)
			}
		}
	}

	return allNames
}

func (m *MockManager) GetConsumedMocks() map[string][]string {
	return m.usedMocks
}
