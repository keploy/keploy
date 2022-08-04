package mocks

import (
	"context"

	"go.keploy.io/server/pkg/models"
)

type Service interface {
	Insert(context.Context, models.TestMock) error
	Get(ctx context.Context, app string, testName string) ([]models.TestMock, error)
}
