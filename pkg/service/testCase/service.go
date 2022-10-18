package testCase

import (
	"context"

	"go.keploy.io/server/pkg/models"
)

type Service interface {
	Get(ctx context.Context, cid, appID, id string) (models.TestCase, error)
	GetAll(ctx context.Context, cid, appID string, offset *int, limit *int) ([]models.TestCase, error)
	Put(ctx context.Context, cid string, t []models.TestCase) ([]string, error)
	GetApps(ctx context.Context, cid string) ([]string, error)
	UpdateTC(ctx context.Context, t []models.TestCase) error
	DeleteTC(ctx context.Context, cid, id string) error
	WriteTC(ctx context.Context, t []models.Mock, testCasePath, mockPath string) ([]string, error)
	ReadTCS(ctx context.Context, testCasePath, mockPath string) ([]models.TestCase, error)
}
