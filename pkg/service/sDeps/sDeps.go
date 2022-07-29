package sDeps

import (
	"context"
	"fmt"

	"go.keploy.io/server/pkg/models"
	"go.uber.org/zap"
)

func NewSDepsService(c models.SDepsDB, log *zap.Logger) *SDeps {
	return &SDeps{
		sdb: c,
		log: log,
	}
}

type SDeps struct {
	sdb models.SDepsDB
	log *zap.Logger
}

func (s *SDeps) Insert(ctx context.Context, doc models.SeleniumDeps) error {
	if count, err := s.sdb.CountDocs(ctx, doc.AppID, doc.TestName); err == nil && count > 0 {
		fmt.Println(`Already present`, count, `
		
		`)
		return s.sdb.UpdateArr(ctx, doc.AppID, doc.TestName, doc)

	}
	fmt.Println(`
	
	
	`)
	return s.sdb.Insert(ctx, doc)
}

func (s *SDeps) Get(ctx context.Context, app string, testName string) ([]models.SeleniumDeps, error) {
	return s.sdb.Get(ctx, app, testName)
}
