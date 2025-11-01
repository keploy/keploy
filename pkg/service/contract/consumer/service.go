package consumer

import "github.com/keploy/keploy-integrations-shared/pkg/models"

// Service defines the consumer service interface
type Service interface {
	ValidateSchema(testsMapping map[string]map[string]*models.OpenAPI, mocksMapping []models.MockMapping) error
}
