package browserMock

import (
	"context"

	"go.keploy.io/server/pkg/models"
	"go.uber.org/zap"
)

func NewBrMockService(c models.BrowserMockDB, log *zap.Logger) *BrowserMock {
	return &BrowserMock{
		sdb: c,
		log: log,
	}
}

// BrowserMock is a service to read-write mocks during record and replay in Selenium-IDE only.
type BrowserMock struct {
	sdb models.BrowserMockDB
	log *zap.Logger
}

func (s *BrowserMock) Put(ctx context.Context, doc models.BrowserMock) error {
	if count, err := s.sdb.CountDocs(ctx, doc.AppID, doc.TestName); err == nil && count > 0 {
		return s.sdb.UpdateArr(ctx, doc.AppID, doc.TestName, doc)
	}
	return s.sdb.Put(ctx, doc)
}

func (s *BrowserMock) Get(ctx context.Context, app string, testName string) ([]models.BrowserMock, error) {
	return s.sdb.Get(ctx, app, testName)
}
