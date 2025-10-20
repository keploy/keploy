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
	globalMockCh           chan *models.Mock
}

func New(logger *zap.Logger, record record.Service, tools tools.Service, replay replay.Service, config *config.Config) *Orchestrator {
	// Create global mock channel for communication between record service and correlation manager
	globalMockCh := make(chan *models.Mock, 1000) // Buffered channel to prevent blocking

	return &Orchestrator{
		logger:                 logger,
		record:                 record,
		replay:                 replay,
		tools:                  tools,
		config:                 config,
		globalMockCh:           globalMockCh,
		mockCorrelationManager: nil, // Will be initialized when needed
	}
}

// GetGlobalMockChannel returns the global mock channel for the record service to use
func (o *Orchestrator) GetGlobalMockChannel() chan<- *models.Mock {
	return o.globalMockCh
}
