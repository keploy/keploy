package sDeps

import (
	"context"

	"go.keploy.io/server/pkg/models"
)

type Service interface {
	Insert(context.Context, models.SeleniumDeps) error
	Get(ctx context.Context, app string, testName string) ([]models.SeleniumDeps, error)
}
