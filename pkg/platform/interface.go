package platform

import "go.keploy.io/server/pkg/models"

type TestCaseDB interface {
	WriteTestcase(tc *models.TestCase) error
	WriteMock(tc *models.Mock) error

	ReadTestcase(path string, options interface{}) ([]*models.TestCase, error)
	ReadMocks(path string) ([]*models.Mock, []*models.Mock, error)
}
