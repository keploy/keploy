package provider

import (
	"context"
	"errors"
	"strings"
	"sync"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/platform/telemetry"
	userDb "go.keploy.io/server/v3/pkg/platform/yaml/configdb/user"
	"go.keploy.io/server/v3/utils"

	"go.uber.org/zap"
)

var TeleGlobalMap sync.Map

type ServiceProvider struct {
	logger *zap.Logger
	cfg    *config.Config
}

func NewServiceProvider(logger *zap.Logger, cfg *config.Config) *ServiceProvider {
	return &ServiceProvider{
		logger: logger,
		cfg:    cfg,
	}
}

func (n *ServiceProvider) ensureInstallationID(ctx context.Context) {
	if n.cfg.DisableTele || strings.TrimSpace(n.cfg.InstallationID) != "" {
		return
	}
	id, err := userDb.New(n.logger, n.cfg).GetInstallationID(ctx)
	if err != nil {
		utils.LogError(n.logger, err, "failed to get installation id")
		return
	}
	n.cfg.InstallationID = id
}

func (n *ServiceProvider) GetService(ctx context.Context, cmd string) (interface{}, error) {
	n.ensureInstallationID(ctx)

	tel := telemetry.NewTelemetry(n.logger, telemetry.Options{
		Enabled:        !n.cfg.DisableTele,
		Version:        utils.Version,
		GlobalMap:      &TeleGlobalMap,
		InstallationID: n.cfg.InstallationID,
	})
	tel.Ping(ctx)

	switch cmd {
	case "record", "test", "normalize", "contract", "config", "update", "export", "import", "templatize", "report", "sanitize", "diff", "mock-record", "mock-replay":
		return Get(ctx, cmd, n.cfg, n.logger, tel)
	case "agent":
		return GetAgent(ctx, cmd, n.cfg, n.logger)
	default:
		return nil, errors.New("invalid command")
	}
}
