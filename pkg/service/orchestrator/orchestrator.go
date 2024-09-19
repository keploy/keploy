
// Package orchestrator acts as a main brain for both the record and replay services
package orchestrator

import (
	"go.keploy.io/server/v2/config"
	"go.keploy.io/server/v2/pkg/service/record"
	"go.keploy.io/server/v2/pkg/service/replay"

	"go.uber.org/zap"
)

type Orchestrator struct {
	logger *zap.Logger
	record record.Service
	replay replay.Service
	config *config.Config
}

func New(logger *zap.Logger, record record.Service, replay replay.Service, config *config.Config) *Orchestrator {
	return &Orchestrator{
		logger: logger,
		record: record,
		replay: replay,
		config: config,
	}
}
