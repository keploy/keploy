package provider

import (
	"context"
	"errors"

	"go.keploy.io/server/v2/config"
	"go.keploy.io/server/v2/pkg/platform/telemetry"
	"go.keploy.io/server/v2/pkg/service"

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
		Version:        n.cfg.Version,
		GlobalMap:      TeleGlobalMap,
		InstallationID: n.cfg.InstallationID,
	})
	tel.Ping()

	switch cmd {
	case "config", "update", "login":
		return tools.NewTools(n.logger, tel, n.auth), nil
	case "gen":
		return utgen.NewUnitTestGenerator(n.cfg.Gen.SourceFilePath, n.cfg.Gen.TestFilePath, n.cfg.Gen.CoverageReportPath, n.cfg.Gen.TestCommand, n.cfg.Gen.TestDir, n.cfg.Gen.CoverageFormat, n.cfg.Gen.DesiredCoverage, n.cfg.Gen.MaxIterations, n.cfg.Gen.Model, n.cfg.Gen.APIBaseURL, n.cfg.Gen.APIVersion, n.cfg.APIServerURL, n.cfg, tel, n.auth, n.logger)
	case "record", "test", "mock", "normalize", "templatize", "rerecord":
		return Get(ctx, cmd, n.cfg, n.logger, tel, n.auth)
	default:
		return nil, errors.New("invalid command")
	}
}
