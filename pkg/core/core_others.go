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

func (c *Core) Setup(_ context.Context, _ string, _ models.SetupOptions) (uint64, error) {
	return 0, errUnsupported
}

func (c *Core) Hook(_ context.Context, _ uint64, _ models.HookOptions) error {
	return errUnsupported
}

func (c *Core) MockOutgoing(_ context.Context, _ uint64, _ models.OutgoingOptions) error {
	return errUnsupported
}

func (c *Core) SetMocks(_ context.Context, _ uint64, _ []*models.Mock, _ []*models.Mock) error {
	return errUnsupported
}

func (c *Core) GetConsumedMocks(_ context.Context, _ uint64) ([]string, error) {
	return nil, errUnsupported
}

func (c *Core) Run(_ context.Context, _ uint64, _ models.RunOptions) models.AppError {
	return models.AppError{
		Err: errUnsupported,
	}
}

func (c *Core) GetContainerIP(_ context.Context, _ uint64) (string, error) {
	return "", errUnsupported
}
