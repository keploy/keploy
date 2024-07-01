//go:build linux

// Package orchestrator acts as a main brain for both the record and replay services
package orchestrator

import (
	"go.keploy.io/server/v2/config"
	"go.keploy.io/server/v2/pkg/service/record"
	"go.keploy.io/server/v2/pkg/service/replay"
	"go.keploy.io/server/v2/pkg/models"
	"context"

	"go.uber.org/zap"
)

type Config interface {
	Read(ctx context.Context, testSetID string) (*models.TestSet, error)
	Write(ctx context.Context, testSetID string, testSet *models.TestSet) error
}

type Orchestrator struct {
	logger *zap.Logger
	record record.Service
	replay replay.Service
	config *config.Config
	TestSetConf Config
}

func New(logger *zap.Logger, record record.Service, replay replay.Service, config *config.Config, TestSetConf Config) *Orchestrator {
	return &Orchestrator{
		logger: logger,
		record: record,
		replay: replay,
		config: config,
		TestSetConf: TestSetConf,
	}
}
