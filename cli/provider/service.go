package provider

import (
	"context"
	"errors"
	"strings"

	"go.keploy.io/server/v2/config"
	"go.keploy.io/server/v2/pkg/core"
	"go.keploy.io/server/v2/pkg/core/hooks"
	"go.keploy.io/server/v2/pkg/core/proxy"
	"go.keploy.io/server/v2/pkg/core/tester"
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
	"gopkg.in/yaml.v3"
)

type ServiceProvider struct {
	logger   *zap.Logger
	configDb *configdb.ConfigDb
	cfg      *config.Config
}

type CommonInternalService struct {
	YamlTestDB      *testdb.TestYaml
	YamlMockDb      *mockdb.MockYaml
	YamlReportDb    *reportdb.TestReport
	Instrumentation *core.Core
}

func NewServiceProvider(logger *zap.Logger, configDb *configdb.ConfigDb, cfg *config.Config) *ServiceProvider {
	return &ServiceProvider{
		logger:   logger,
		configDb: configDb,
		cfg:      cfg,
	}
}

func (n *ServiceProvider) GetTelemetryService(ctx context.Context, config config.Config) (*telemetry.Telemetry, error) {
	installationID, err := n.configDb.GetInstallationID(ctx)
	if err != nil {
		return nil, errors.New("failed to get installation id")
	}
	return telemetry.NewTelemetry(n.logger, telemetry.Options{
		Enabled:        !config.DisableTele,
		Version:        utils.Version,
		GlobalMap:      map[string]interface{}{},
		InstallationID: installationID,
	},
	), nil
}

func (n *ServiceProvider) GetCommonServices(config config.Config) *CommonInternalService {
	h := hooks.NewHooks(n.logger, config)
	p := proxy.New(n.logger, h, config)
	t := tester.New(n.logger, h) //for keploy test bench
	instrumentation := core.New(n.logger, h, p, t)
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

func (n *ServiceProvider) GetService(ctx context.Context, cmd string) (interface{}, error) {
	tel, err := n.GetTelemetryService(ctx, *n.cfg)
	if err != nil {
		return nil, err
	}
	tel.Ping()
	switch cmd {
	case "config", "update":
		return tools.NewTools(n.logger, tel), nil
	// TODO: add case for mock
	case "record", "test", "mock", "normalize":
		// Check if the config file exists on the path or not and if it does not, we create it.
		if !utils.CheckFileExists("keploy.yml") {
			toolsService := tools.NewTools(n.logger, tel)
			config := n.cfg
			config.Path = strings.TrimSuffix(config.Path, "/keploy")
			yamlData, err := yaml.Marshal(config)
			if err != nil {
				n.logger.Debug("failed to marshal the config")
			}
			err = toolsService.CreateConfig(ctx, "keploy.yml", string(yamlData))
			if err != nil {
				n.logger.Debug("failed to create the config file", zap.Error(err))
			}
		}
		commonServices := n.GetCommonServices(*n.cfg)
		if cmd == "record" {
			return record.New(n.logger, commonServices.YamlTestDB, commonServices.YamlMockDb, tel, commonServices.Instrumentation, *n.cfg), nil
		}
		if cmd == "test" || cmd == "normalize" {
			return replay.NewReplayer(n.logger, commonServices.YamlTestDB, commonServices.YamlMockDb, commonServices.YamlReportDb, tel, commonServices.Instrumentation, *n.cfg), nil
		}
		return nil, errors.New("invalid command")
	default:
		return nil, errors.New("invalid command")
	}
}
