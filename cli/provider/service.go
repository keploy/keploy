package provider

import (
	"context"
	"errors"

	"go.keploy.io/server/v2/config"
	"go.keploy.io/server/v2/pkg/platform/telemetry"
	"go.keploy.io/server/v2/pkg/service"
	"go.keploy.io/server/v2/utils"

	"go.keploy.io/server/v2/pkg/service/tools"
	"go.keploy.io/server/v2/pkg/service/utgen"
	"go.uber.org/zap"
)

var TeleGlobalMap = make(map[string]interface{})

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
		GlobalMap:      TeleGlobalMap,
		InstallationID: n.cfg.InstallationID,
	})
	tel.Ping()

	switch cmd {
	case "config", "update", "login":
		return tools.NewTools(n.logger, tel, n.auth), nil
	case "gen":
		return utgen.NewUnitTestGenerator(n.cfg, tel, n.auth, n.logger)
	case "record", "test", "mock", "normalize", "templatize", "rerecord", "contract":
		return Get(ctx, cmd, n.cfg, n.logger, tel, n.auth)
	case "diff":
		return utils.NewDiffService(n.logger), nil
	default:
		return nil, errors.New("invalid command")
	}
}
