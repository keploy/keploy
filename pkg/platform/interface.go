package platform

import "go.keploy.io/server/pkg/models"

type TestCaseDB interface {
	WriteTestcase(path string, tc *models.TestCase) error
	WriteMock(path string, tc *models.Mock) error

	LastSessionIndex(path string) (string, error)

	Read(path string, options interface{}) ([]*models.TestCase, []*models.Mock, error)
}
