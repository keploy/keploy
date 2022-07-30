package sDeps

import (
	"context"

	"go.keploy.io/server/pkg/models"
	"go.uber.org/zap"
)

func NewInrfaDepsService(c models.InfraDepsDB, log *zap.Logger) *InfraDeps {
	return &InfraDeps{
		sdb: c,
		log: log,
	}
}

type InfraDeps struct {
	sdb models.InfraDepsDB
	log *zap.Logger
}

func (s *InfraDeps) Insert(ctx context.Context, doc models.InfraDeps) error {
	if count, err := s.sdb.CountDocs(ctx, doc.AppID, doc.TestName); err == nil && count > 0 {
		return s.sdb.UpdateArr(ctx, doc.AppID, doc.TestName, doc)
	}
	return s.sdb.Insert(ctx, doc)
}

func (s *InfraDeps) Get(ctx context.Context, app string, testName string) ([]models.InfraDeps, error) {
	return s.sdb.Get(ctx, app, testName)
}
