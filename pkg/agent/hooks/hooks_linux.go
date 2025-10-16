package hooks

import (
	"go.keploy.io/server/v2/config"
	"go.keploy.io/server/v2/pkg/agent"
	"go.keploy.io/server/v2/pkg/agent/hooks/linux"
	"go.uber.org/zap"
)

func New(logger *zap.Logger, cfg *config.Config) agent.Hooks {
	return linux.NewHooks(logger, cfg)
}
