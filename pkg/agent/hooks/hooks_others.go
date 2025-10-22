//go:build !linux

package hooks

import (
	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/agent"
	"go.keploy.io/server/v3/pkg/agent/hooks/others"
	"go.uber.org/zap"
)

func New(logger *zap.Logger, cfg *config.Config) agent.Hooks {
	return others.NewHooks(logger, cfg)
}
