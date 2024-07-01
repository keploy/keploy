package provider

import (
	"context"
	"errors"

	"go.keploy.io/server/v2/config"
	"go.keploy.io/server/v2/pkg/platform/telemetry"
	"go.keploy.io/server/v2/pkg/platform/yaml/configdb/user"

	"go.keploy.io/server/v2/pkg/service/contract"
	"go.keploy.io/server/v2/pkg/service/tools"
	"go.keploy.io/server/v2/pkg/service/utgen"
	"go.keploy.io/server/v2/utils"
	"go.uber.org/zap"
)

var TeleGlobalMap = make(map[string]interface{})

type ServiceProvider struct {
	logger *zap.Logger
	userDb *user.Db
	cfg    *config.Config
}

func NewServiceProvider(logger *zap.Logger, userDb *user.Db, cfg *config.Config) *ServiceProvider {
	return &ServiceProvider{
		logger: logger,
		userDb: userDb,
		cfg:    cfg,
	}
}

func (n *ServiceProvider) GetTelemetryService(ctx context.Context, config *config.Config) (*telemetry.Telemetry, error) {
	installationID, err := n.userDb.GetInstallationID(ctx)
	if err != nil {
		return nil, errors.New("failed to get installation id")
	}
	return telemetry.NewTelemetry(n.logger, telemetry.Options{
		Enabled:        !config.DisableTele,
		Version:        utils.Version,
		GlobalMap:      TeleGlobalMap,
		InstallationID: installationID,
	},
	), nil
}

func (n *ServiceProvider) GetService(ctx context.Context, cmd string) (interface{}, error) {
	tel, err := n.GetTelemetryService(ctx, n.cfg)
	if err != nil {
		return nil, err
	}
	tel.Ping()
	switch cmd {
	case "config", "update":
		return tools.NewTools(n.logger, tel), nil
	case "gen":
		return utgen.NewUnitTestGenerator(n.cfg.Gen.SourceFilePath, n.cfg.Gen.TestFilePath, n.cfg.Gen.CoverageReportPath, n.cfg.Gen.TestCommand, n.cfg.Gen.TestDir, n.cfg.Gen.CoverageFormat, n.cfg.Gen.DesiredCoverage, n.cfg.Gen.MaxIterations, n.cfg.Gen.Model, n.cfg.Gen.APIBaseURL, n.cfg.Gen.APIVersion, n.cfg, tel, n.logger)
	case "generate", "download", "validate":
		return contract.New(n.logger, n.cfg), nil
	case "record", "test", "mock", "normalize", "rerecord":
		return Get(ctx, cmd, n.cfg, n.logger, tel)
	default:
		return nil, errors.New("invalid command")
	}
}
