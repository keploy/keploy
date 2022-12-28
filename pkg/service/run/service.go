package run

import (
	"context"
	"time"

	"go.keploy.io/server/pkg/models"
)

type Service interface {
	Get(ctx context.Context, summary bool, cid string, user, app, id *string, from, to *time.Time, offset *int, limit *int) ([]*models.TestRun, error)
	Put(ctx context.Context, run models.TestRun, testExport bool, testReportPath string) error
	Normalize(ctx context.Context, cid, id string) error
}
