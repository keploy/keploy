package mock

import (
	"context"
	// proto "go.keploy.io/server/grpc/regression"

	"go.keploy.io/server/pkg/models"
)

type Service interface {
	Put(ctx context.Context, path string, doc models.Mock, meta interface{}) error
	GetAll(ctx context.Context, path string, name string) ([]models.Mock, error)
}
