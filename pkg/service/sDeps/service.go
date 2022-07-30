package sDeps

import (
	"context"

	"go.keploy.io/server/pkg/models"
)

type Service interface {
	Insert(context.Context, models.InfraDeps) error
	Get(ctx context.Context, app string, testName string) ([]models.InfraDeps, error)
}
