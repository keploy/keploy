package contract

import (
	"context"
)

// Service defines the contract service interface
type Service interface {
	Generate(ctx context.Context, flag bool) error
	Download(ctx context.Context) error
}
