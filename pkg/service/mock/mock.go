package mock

import (
	"context"

	"go.keploy.io/server/pkg/models"
	"go.uber.org/zap"
)

func NewMockService(c models.MockDB, log *zap.Logger) *Mock {
	return &Mock{
		sdb: c,
		log: log,
	}
}

type Mock struct {
	sdb models.MockDB
	log *zap.Logger
}

func (s *Mock) Put(ctx context.Context, doc models.Mock) error {
	if count, err := s.sdb.CountDocs(ctx, doc.AppID, doc.TestName); err == nil && count > 0 {
		return s.sdb.UpdateArr(ctx, doc.AppID, doc.TestName, doc)
	}
	return s.sdb.Put(ctx, doc)
}

func (s *Mock) Get(ctx context.Context, app string, testName string) ([]models.Mock, error) {
	return s.sdb.Get(ctx, app, testName)
}
