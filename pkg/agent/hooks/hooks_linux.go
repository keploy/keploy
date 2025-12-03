package hooks

import (
	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/agent"
	"go.keploy.io/server/v3/pkg/agent/hooks/linux"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

func New(logger *zap.Logger, cfg *config.Config) agent.Hooks {
	return linux.NewHooks(logger.Named(models.HooksService), cfg)
}
