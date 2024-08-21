package consumer

import (
	"go.keploy.io/server/v2/pkg/models"
)

// Service defines the consumer service interface
type Service interface {
	ValidateSchema(testsMapping map[string]map[string]*models.OpenAPI, mocksMapping []models.MockMapping) error
}
