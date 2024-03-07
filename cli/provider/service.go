package provider

import (
	"context"
	"errors"

	"go.keploy.io/server/v2/config"
	"go.keploy.io/server/v2/pkg/core"
	"go.keploy.io/server/v2/pkg/core/hooks"
	"go.keploy.io/server/v2/pkg/core/proxy"
	"go.keploy.io/server/v2/pkg/platform/telemetry"
	"go.keploy.io/server/v2/pkg/platform/yaml/configdb"
	mockdb "go.keploy.io/server/v2/pkg/platform/yaml/mockdb"
	reportdb "go.keploy.io/server/v2/pkg/platform/yaml/reportdb"
	testdb "go.keploy.io/server/v2/pkg/platform/yaml/testdb"

	"go.keploy.io/server/v2/pkg/service/record"
	"go.keploy.io/server/v2/pkg/service/replay"
	"go.keploy.io/server/v2/pkg/service/tools"
	"go.keploy.io/server/v2/utils"
	"go.uber.org/zap"
)

type ServiceProvider struct {
	logger   *zap.Logger
	configDb *configdb.ConfigDb
}

type CommonInternalService struct {
	YamlTestDB      *testdb.TestYaml
	YamlMockDb      *mockdb.MockYaml
	YamlReportDb    *reportdb.TestReport
	Instrumentation *core.Core
}

func NewServiceProvider(logger *zap.Logger, configDb *configdb.ConfigDb) *ServiceProvider {
	return &ServiceProvider{
		logger:   logger,
		configDb: configDb,
	}
}

func (n *ServiceProvider) GetTelemetryService(ctx context.Context, config config.Config) (*telemetry.Telemetry, error) {
	installtionID, err := n.configDb.GetInstallationId(ctx)
	if err != nil {
		return nil, errors.New("failed to get installation id")
	}
	return telemetry.NewTelemetry(n.logger, telemetry.Options{
		Enabled:        config.DisableTele,
		Version:        utils.Version,
		GlobalMap:      map[string]interface{}{},
		InstallationID: installtionID,
	},
	), nil
}

func (n *ServiceProvider) GetCommonServices(config config.Config) *CommonInternalService {
	h := hooks.NewHooks(n.logger, config)
	p := proxy.New(n.logger, h, config)
	instrumentation := core.New(n.logger, h, p)
	testDB := testdb.New(n.logger, config.Path)
	mockDB := mockdb.New(n.logger, config.Path, "")
	reportDB := reportdb.New(n.logger, config.Path+"/reports")
	return &CommonInternalService{
		Instrumentation: instrumentation,
		YamlTestDB:      testDB,
		YamlMockDb:      mockDB,
		YamlReportDb:    reportDB,
	}
}

func (n *ServiceProvider) GetService(ctx context.Context, cmd string, config config.Config) (interface{}, error) {
	tel, err := n.GetTelemetryService(ctx, config)
	if err != nil {
		return nil, err
	}
	tel.Ping(ctx)
	switch cmd {
	case "config", "update":
		return tools.NewTools(n.logger, tel), nil
	// TODO: add case for mock
	case "record", "test", "mock":
		commonServices := n.GetCommonServices(config)
		if cmd == "record" {
			return record.New(n.logger, commonServices.YamlTestDB, commonServices.YamlMockDb, tel, commonServices.Instrumentation, config), nil
		}
		if cmd == "test" {
			return replay.NewReplayer(n.logger, commonServices.YamlTestDB, commonServices.YamlMockDb, commonServices.YamlReportDb, tel, commonServices.Instrumentation, config), nil
		}
		return nil, errors.New("invalid command")
	default:
		return nil, errors.New("invalid command")
	}
}
