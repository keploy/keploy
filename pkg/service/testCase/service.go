package testCase

import (
	"context"

	"go.keploy.io/server/pkg/models"
)

type Service interface {
	Get(ctx context.Context, cid, appID, id string) (models.TestCase, error)
	GetAll(ctx context.Context, cid, appID string, offset *int, limit *int, testCasePath, mockPath string) ([]models.TestCase, error)
	GetApps(ctx context.Context, cid string) ([]string, error)
	Update(ctx context.Context, t []models.TestCase) error
	Delete(ctx context.Context, cid, id string) error
	InsertToDB(ctx context.Context, cid string, t []models.TestCase) ([]string, error)
	WriteToYaml(ctx context.Context, t []models.Mock, testCasePath, mockPath string) ([]string, error)
}
