// Package orchestrator acts as a main brain for both the record and replay services
package orchestrator

import (
	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/service/record"
	"go.keploy.io/server/v3/pkg/service/replay"
	"go.keploy.io/server/v3/pkg/service/tools"

	"go.uber.org/zap"
)

type Orchestrator struct {
	logger                 *zap.Logger
	record                 record.Service
	replay                 replay.Service
	tools                  tools.Service
	config                 *config.Config
	mockCorrelationManager *MockCorrelationManager
	globalMockQueue        *models.MockQueue
}

func New(logger *zap.Logger, record record.Service, tools tools.Service, replay replay.Service, config *config.Config) *Orchestrator {
	// Create unbounded queue for mocks to prevent dropping during correlation
	globalMockQueue := models.NewMockQueue()

	return &Orchestrator{
		logger:                 logger,
		record:                 record,
		replay:                 replay,
		tools:                  tools,
		config:                 config,
		globalMockQueue:        globalMockQueue,
		mockCorrelationManager: nil, // Will be initialized when needed
	}
}

// GetGlobalMockQueue returns the global mock queue for the record service to use
func (o *Orchestrator) GetGlobalMockQueue() *models.MockQueue {
	return o.globalMockQueue
}
