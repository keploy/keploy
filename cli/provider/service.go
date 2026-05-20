package provider

import (
	"context"
	"errors"
	"sync"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/platform/telemetry"
	"go.keploy.io/server/v3/pkg/service"
	"go.keploy.io/server/v3/utils"

	"go.keploy.io/server/v3/pkg/service/utgen"
	"go.uber.org/zap"
)

var TeleGlobalMap sync.Map

type ServiceProvider struct {
	logger *zap.Logger
	cfg    *config.Config
	auth   service.Auth
}

func NewServiceProvider(logger *zap.Logger, cfg *config.Config, auth service.Auth) *ServiceProvider {
	return &ServiceProvider{
		logger: logger,
		cfg:    cfg,
		auth:   auth,
	}
}

func (n *ServiceProvider) GetService(ctx context.Context, cmd string) (interface{}, error) {

	tel := telemetry.NewTelemetry(n.logger, telemetry.Options{
		Enabled:        !n.cfg.DisableTele,
		Version:        utils.Version,
		GlobalMap:      &TeleGlobalMap,
		InstallationID: n.cfg.InstallationID,
	})
	tel.Ping(ctx)

	switch cmd {
	case "gen":
		return utgen.NewUnitTestGenerator(n.cfg, tel, n.auth, n.logger)
	case "record", "test", "mock", "normalize", "rerecord", "contract", "config", "update", "login", "export", "import", "templatize", "report", "sanitize":
		return Get(ctx, cmd, n.cfg, n.logger, tel, n.auth)
	case "agent":
		return GetAgent(ctx, cmd, n.cfg, n.logger, n.auth)
	default:
		return nil, errors.New("invalid command")
	}
}
