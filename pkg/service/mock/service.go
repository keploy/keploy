package mock

import (
	"context"

	"go.keploy.io/server/pkg/models"
)

type Service interface {
	Put(ctx context.Context, path string, doc models.Mock) error
	GetAll(ctx context.Context, path string, name string) ([]models.Mock, error)
}
