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

func (c *Agent) Hook(ctx context.Context, id uint64, opts models.HookOptions) error {
	return errUnsupported
}

func (c *Agent) GetHookUnloadDone(id uint64) <-chan struct{} {
	ch := make(chan struct{})
	close(ch) // Immediately close since no actual hooks are loaded
	return ch
}

func (c *Agent) MockOutgoing(ctx context.Context, id uint64, opts models.OutgoingOptions) error {
	return errUnsupported
}

func (c *Agent) SetMocks(ctx context.Context, id uint64, filtered []*models.Mock, unFiltered []*models.Mock) error {
	return errUnsupported
}

func (c *Agent) GetConsumedMocks(ctx context.Context, id uint64) ([]models.MockState, error) {
	return nil, errUnsupported
}

func (c *Agent) Run(ctx context.Context, id uint64, _ models.RunOptions) models.AppError {
	return models.AppError{
		Err: errUnsupported,
	}
}

func (c *Agent) GetContainerIP(_ context.Context, id uint64) (string, error) {
	return "", errUnsupported
}
