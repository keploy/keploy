//go:build linux

// Package orchestrator acts as a main brain for both the record and replay services
package orchestrator

import (
	"go.keploy.io/server/v2/config"
	"go.keploy.io/server/v2/pkg/service/record"
	"go.keploy.io/server/v2/pkg/service/replay"
	"go.keploy.io/server/v2/pkg/service/tools"

	"go.uber.org/zap"
)

type Orchestrator struct {
	logger *zap.Logger
	record record.Service
	replay replay.Service
	tools  tools.Service
	config *config.Config
}

func New(logger *zap.Logger, record record.Service, tools tools.Service, replay replay.Service, config *config.Config) *Orchestrator {
	return &Orchestrator{
		logger: logger,
		record: record,
		replay: replay,
		tools:  tools,
		config: config,
	}
}
