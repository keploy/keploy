package cli

import (
	"errors"

	"go.keploy.io/server/v2/config"
	"go.keploy.io/server/v2/pkg/core"
	"go.keploy.io/server/v2/pkg/core/hooks"
	"go.keploy.io/server/v2/pkg/core/proxy"
	"go.keploy.io/server/v2/pkg/platform/telemetry"
	"go.keploy.io/server/v2/pkg/service/record"
	"go.keploy.io/server/v2/pkg/service/replay"
	"go.keploy.io/server/v2/pkg/service/tools"
	"go.keploy.io/server/v2/utils"
	"go.uber.org/zap"
)

type serviceProvider struct {
	logger *zap.Logger
}

type commonInternalService struct {
	YamlTestDB      yaml.testDB
	YamlMockDb      yaml.mockDB
	YamlReportDb    yaml.reportDB
	Instrumentation core.Core
}

func NewServiceProvider(logger *zap.Logger) *serviceProvider {
	return &serviceProvider{
		logger: logger,
	}
}

func (n *serviceProvider) GetTelemetryService(config config.Config) *telemetry.Telemetry {
	return telemetry.NewTelemetry(n.logger, telemetry.Options{
		Enabled:   config.Telemetry,
		Version:   utils.Version,
		GlobalMap: map[string]interface{}{},
	})
}

// TODO error handling
func (n *serviceProvider) GetCommonServices(config config.Config) *commonInternalService {
	hooks := hooks.NewHooks()
	proxy := proxy.New(n.logger, hooks, config)
	intrumentation := core.New(logger, id, apps, hooks, proxy)
	testDB := yaml.NewTestDB()
	mockDB := yaml.NewMockDb()
	reportDB := yaml.NewReportDb()
	return &CommonService{
		Instrumentation: intrumentation,
		TestDB:          testDB,
		MockDB:          mockDB,
		ReportDB:        reportDB,
	}
}

func (n *serviceProvider) setConfig(config config.Config) {
	n.Config = config
}

func (n *serviceProvider) GetService(cmd string, config config.Config) (interface{}, error) {
	n.setConfig(config)
	telemetry := n.GetTelemetryService(config)
	switch cmd {
	case "config", "update":
		return tools.NewTools(n.logger, telemetry), nil
	case "record", "test":
		commonServices := n.GetCommonServices(config)
		if cmd == "record" {
			record.NewRecorder(n.logger, commonServices.YamlTestDB, commonServices.YamlMockDb, telemetry, commonServices.Instrumentation, config)
		}
		if cmd == "test" {
			replay.NewReplayer(n.logger, commonServices.YamlTestDB, commonServices.YamlMockDb, commonServices.ReportDB, telemetry, commonServices.Instrumentation, config)
		}
		return nil, nil
	default:
		return nil, errors.New("invalid command")
	}
}
