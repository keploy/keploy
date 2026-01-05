package mockrecord

import (
	"context"
	"errors"
	"strings"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

type recorder struct {
	logger *zap.Logger
	cfg    *config.Config
	record RecordService
}

// New creates a new mock recording service.
func New(logger *zap.Logger, cfg *config.Config, recordSvc RecordService) Service {
	if logger == nil {
		logger = zap.NewNop()
	}
	return &recorder{
		logger: logger,
		cfg:    cfg,
		record: recordSvc,
	}
}

// Record captures outgoing calls while running the provided command.
func (r *recorder) Record(ctx context.Context, opts models.RecordOptions) (*models.RecordResult, error) {
	if r.record == nil {
		return nil, errors.New("record service is not configured")
	}

	if strings.TrimSpace(opts.Command) == "" && r.cfg != nil {
		opts.Command = r.cfg.Command
	}
	if strings.TrimSpace(opts.Path) == "" && r.cfg != nil {
		opts.Path = r.cfg.Path
	}
	if opts.Duration == 0 && r.cfg != nil && r.cfg.Record.RecordTimer > 0 {
		opts.Duration = r.cfg.Record.RecordTimer
	}

	if strings.TrimSpace(opts.Command) == "" {
		return nil, errors.New("command is required")
	}

	result, err := r.record.RecordMocks(ctx, opts)
	if err != nil {
		return nil, err
	}

	if result.Metadata == nil {
		result.Metadata = ExtractMetadata(result.Mocks, opts.Command)
	}

	return result, nil
}
