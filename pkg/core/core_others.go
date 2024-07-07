//go:build !linux

// Package core provides functionality for managing core functionalities in Keploy.
package core

import (
	"context"
	"errors"
	"runtime"

	"go.keploy.io/server/v2/pkg/models"
	"go.uber.org/zap"
)

type Core struct {
	logger *zap.Logger
}

var errUnsupported = errors.New("instrumentation only supported on linux. Detected OS: " + runtime.GOOS)

func New(logger *zap.Logger) *Core {
	return &Core{
		logger: logger,
	}
}

func (c *Core) Setup(ctx context.Context, cmd string, opts models.SetupOptions) (uint64, error) {
	return 0, errUnsupported
}

func (c *Core) Hook(ctx context.Context, id uint64, opts models.HookOptions) error {
	return errUnsupported
}

func (c *Core) MockOutgoing(ctx context.Context, id uint64, opts models.OutgoingOptions) error {
	return errUnsupported
}

func (c *Core) SetMocks(ctx context.Context, id uint64, filtered []*models.Mock, unFiltered []*models.Mock) error {
	return errUnsupported
}

func (c *Core) GetConsumedMocks(ctx context.Context, id uint64) ([]string, error) {
	return nil, errUnsupported
}

func (c *Core) Run(ctx context.Context, id uint64, _ models.RunOptions) models.AppError {
	return models.AppError{
		Err: errUnsupported,
	}
}

func (c *Core) GetContainerIP(_ context.Context, id uint64) (string, error) {
	return "", errUnsupported
}
