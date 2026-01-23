//go:build windows && amd64

package hooks

import (
	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/agent"
	"go.keploy.io/server/v3/pkg/agent/hooks/windows"
	"go.uber.org/zap"
)

func New(logger *zap.Logger, cfg *config.Config) agent.Hooks {
	return windows.NewHooks(logger, cfg)
}
