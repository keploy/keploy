package platform

import "go.keploy.io/server/pkg/models"

type TestCaseDB interface{
	Insert(tc *models.Mock, mocks []*models.Mock) error
}