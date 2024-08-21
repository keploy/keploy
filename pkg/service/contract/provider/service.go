package provider

import (
	"context"
)

// Service defines the provider service interface
type Service interface {
	ValidateSchema(ctx context.Context) error
}
