package provider

import (
	"context"
)

// Service defines the provider service interface
type Service interface {
	ProviderDrivenValidation(ctx context.Context) error
}
