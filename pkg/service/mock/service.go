package mock

import (
	"context"

	"go.keploy.io/server/pkg/models"
)

type Service interface {
	Put(context.Context, models.Mock) error
	Get(ctx context.Context, app string, testName string) ([]models.Mock, error)
}
