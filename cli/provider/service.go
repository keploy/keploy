package provider

import (
	"context"
	"errors"
	"sync"

	"go.keploy.io/server/v2/config"
	"go.keploy.io/server/v2/pkg/platform/telemetry"
	"go.keploy.io/server/v2/pkg/service"
	"go.keploy.io/server/v2/utils"

	"go.keploy.io/server/v2/pkg/service/load"
	"go.keploy.io/server/v2/pkg/service/secure"
	"go.keploy.io/server/v2/pkg/service/testsuite"
	"go.keploy.io/server/v2/pkg/service/utgen"
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
		GlobalMap:      TeleGlobalMap,
		InstallationID: n.cfg.InstallationID,
	})
	tel.Ping()

	switch cmd {
	case "secure":
		return secure.NewSecurityChecker(n.cfg, n.logger)
	case "load":
		return load.NewLoadTester(n.cfg, n.logger)
	case "testsuite":
		return testsuite.NewTSExecutor(n.cfg, n.logger, false)
	case "gen":
		return utgen.NewUnitTestGenerator(n.cfg, tel, n.auth, n.logger)
	case "record", "test", "mock", "normalize", "rerecord", "contract", "config", "update", "login", "export", "import", "templatize", "report":
		return Get(ctx, cmd, n.cfg, n.logger, tel, n.auth)
	default:
		return nil, errors.New("invalid command")
	}
}
