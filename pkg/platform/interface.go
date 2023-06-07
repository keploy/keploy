package platform

import "go.keploy.io/server/pkg/models"

type TestCaseDB interface{
	Insert(tc *models.Mock, mocks []*models.Mock) error
	Read (options interface{}) ([]models.Mock, map[string][]models.Mock, error)
}