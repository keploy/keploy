package testset

import (
	"context"

	"go.keploy.io/server/v2/pkg/models"
	"go.uber.org/zap"
)

type Db struct {
	g *Generic[*models.TestSet]
}

func New(logger *zap.Logger, path string) *Db {
	return &Db{
		g: NewGeneric[*models.TestSet](logger, path),
	}
}

func (db *Db) ReadConfig(ctx context.Context, testSetID string) (*models.TestSet, error) {
	return db.g.ReadConfig(ctx, testSetID)
}

func (db *Db) WriteConfig(ctx context.Context, testSetID string, testSet *models.TestSet) error {
	return db.g.WriteConfig(ctx, testSetID, testSet)
}
