package platform

import "go.keploy.io/server/pkg/models"

type TestCaseDB interface {
	WriteTestcase(path string, tc *models.TestCase) error
	WriteMock(path string, tc *models.Mock) error

	NewSessionIndex(path string) (string, error)
	ReadSessionIndices(path string) ([]string, error)

	ReadTestcase(path string, options interface{}) ([]*models.TestCase, error)
	ReadMocks(path string) ([]*models.Mock, []*models.Mock, error)
}
