package regression

import (
	"context"
	"net/http"

	"go.keploy.io/server/pkg/models"
)

type Service interface {
	Get(ctx context.Context, cid, appID, id string) (models.TestCase, error)
	GetAll(ctx context.Context, cid, appID string, offset *int, limit *int) ([]models.TestCase, error)
	Put(ctx context.Context, cid string, t []models.TestCase) ([]string, error)
	DeNoise(ctx context.Context, cid, id, app, body string, h http.Header, path string) error
	Test(ctx context.Context, cid, app, runID, id, path string, resp models.HttpResp) (bool, error)
	GetApps(ctx context.Context, cid string) ([]string, error)
	UpdateTC(ctx context.Context, t []models.TestCase) error
	DeleteTC(ctx context.Context, cid, id string) error
	WriteTC(ctx context.Context, t []models.Mock, path string) ([]string, error)
	ReadTCS(ctx context.Context, path string) ([]models.TestCase, error)
}
