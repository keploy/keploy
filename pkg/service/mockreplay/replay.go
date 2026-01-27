package mockreplay

import (
	"context"
	"errors"
	"strings"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

type replayer struct {
	logger  *zap.Logger
	cfg     *config.Config
	runtime Runtime
}

// New creates a new mock replay service.
func New(logger *zap.Logger, cfg *config.Config, runtime Runtime) Service {
	if logger == nil && runtime != nil {
		logger = runtime.Logger()
	}
	if logger == nil {
		logger = zap.NewNop()
	}
	if cfg == nil && runtime != nil {
		cfg = runtime.Config()
	}
	return &replayer{
		logger:  logger,
		cfg:     cfg,
		runtime: runtime,
	}
}

// ListMockSets returns a list of available mock set names/IDs.
func (r *replayer) ListMockSets(ctx context.Context) ([]string, error) {
	if r.runtime == nil {
		return nil, errors.New("replay runtime is not configured")
	}
	mockDB := r.runtime.MockDB()
	if mockDB == nil {
		return nil, errors.New("mock database is not configured")
	}

	return mockDB.GetAllMockSetIDs(ctx)
}

// Replay loads mocks and replays them while running the provided command.
func (r *replayer) Replay(ctx context.Context, opts models.ReplayOptions) (*models.ReplayResult, error) {
	if r.runtime == nil {
		return nil, errors.New("replay runtime is not configured")
	}
	if strings.TrimSpace(opts.Command) == "" {
		return nil, errors.New("command is required")
	}
	if r.cfg == nil {
		r.cfg = r.runtime.Config()
	}
	if r.logger == nil {
		r.logger = r.runtime.Logger()
	}

	return r.mockReplay(ctx, opts)
}
