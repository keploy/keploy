package app

import (
	"context"

	"go.keploy.io/server/v3/pkg/platform/docker"
	"go.uber.org/zap"
)

// AppHooks defines extension points used during app lifecycle.
type AppHooks interface {
	BeforeDockerComposeSetup(ctx context.Context, compose *docker.Compose, serviceName string) (bool, error)
	BeforeDockerSetup(ctx context.Context, cmd string) (string, error)
}

var HookImpl AppHooks

type Hooks struct {
	logger *zap.Logger
}

func NewHooks(logger *zap.Logger) Hooks {
	return Hooks{logger: logger}
}

func (Hooks) BeforeDockerComposeSetup(ctx context.Context, _ *docker.Compose, _ string) (bool, error) {
	// no-op
	return false, nil
}

func (h Hooks) BeforeDockerSetup(ctx context.Context, cmd string) (string, error) {
	h.logger.Debug("running before docker setup hook - oss", zap.String("cmd", cmd))
	// no-op
	return cmd, nil
}
