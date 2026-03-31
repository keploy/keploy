package mockreplay

import (
	"context"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/service/replay"
	"go.uber.org/zap"
)

// Service defines the interface for replaying recorded mocks.
type Service interface {
	Replay(ctx context.Context, opts models.ReplayOptions) (*models.ReplayResult, error)
	// ListMockSets returns a list of available mock set names/IDs.
	ListMockSets(ctx context.Context) ([]string, error)
}

// Runtime provides access to the shared replay runtime state.
type Runtime interface {
	Logger() *zap.Logger
	Config() *config.Config
	Instrumentation() replay.Instrumentation
	MockDB() replay.MockDB
}
