package proxyservice

import (
	"context"

	"go.keploy.io/server/v2/pkg/models"
)

type MockDB interface {
	InsertMock(ctx context.Context, mock *models.Mock, testSetID string) error
}
