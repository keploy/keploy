//go:build !linux

// Package Agent provides functionality for managing Agent functionalities in Keploy.
package agent

import (
	"context"
	"errors"
	"runtime"

	"go.keploy.io/server/v2/pkg/models"
	"go.uber.org/zap"
)

type Agent struct {
	logger *zap.Logger
}

var errUnsupported = errors.New("instrumentation only supported on linux. Detected OS: " + runtime.GOOS)

func New(logger *zap.Logger) *Agent {
	return &Agent{
		logger: logger,
	}
}

func (c *Agent) Setup(ctx context.Context, cmd string, opts models.SetupOptions) (uint64, error) {
	return 0, errUnsupported
}

func (c *Agent) Hook(ctx context.Context, opts models.HookOptions) error {
	return errUnsupported
}

func (c *Agent) MockOutgoing(ctx context.Context, opts models.OutgoingOptions) error {
	return errUnsupported
}

func (c *Agent) SetMocks(ctx context.Context, filtered []*models.Mock, unFiltered []*models.Mock) error {
	return errUnsupported
}

func (c *Agent) StoreMocks(ctx context.Context, filtered []*models.Mock, unFiltered []*models.Mock) error {
	return errUnsupported
}

func (c *Agent) UpdateMockParams(ctx context.Context, params models.MockFilterParams) error {
	return errUnsupported
}

func (c *Agent) GetConsumedMocks(ctx context.Context) ([]models.MockState, error) {
	return nil, errUnsupported
}

func (c *Agent) Run(ctx context.Context, _ models.RunOptions) models.AppError {
	return models.AppError{
		Err: errUnsupported,
	}
}

func (c *Agent) GetContainerIP(_ context.Context) (string, error) {
	return "", errUnsupported
}

func (c *Agent) GetErrorChannel() <-chan error {
	return nil
}
