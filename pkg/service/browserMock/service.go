package browserMock

import (
	"context"

	"go.keploy.io/server/pkg/models"
)

type Service interface {
	Put(context.Context, models.BrowserMock) error
	Get(ctx context.Context, app string, testName string) ([]models.BrowserMock, error)
}
