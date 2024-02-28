package cli

import (
	"errors"

	"go.keploy.io/server/v2/config"
	"go.keploy.io/server/v2/pkg/core"
	"go.keploy.io/server/v2/pkg/core/hooks"
	"go.keploy.io/server/v2/pkg/core/proxy"
	"go.keploy.io/server/v2/pkg/platform/telemetry"
	mockdb "go.keploy.io/server/v2/pkg/platform/yaml/mockdb"
	reportdb "go.keploy.io/server/v2/pkg/platform/yaml/reportdb"
	testdb "go.keploy.io/server/v2/pkg/platform/yaml/testdb"

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
	YamlTestDB      *testdb.TestYaml
	YamlMockDb      *mockdb.MockYaml
	YamlReportDb    *reportdb.TestReport
	Instrumentation *core.Core
}

func NewServiceProvider(logger *zap.Logger) *serviceProvider {
	return &serviceProvider{
		logger: logger,
	}
}

func (n *serviceProvider) GetTelemetryService(config config.Config) *telemetry.Telemetry {
	// TODO: Add installation ID
	return telemetry.NewTelemetry(n.logger, telemetry.Options{
		Enabled:   config.Telemetry,
		Version:   utils.Version,
		GlobalMap: map[string]interface{}{},
	})
}

// TODO error handling like path vadilations, remove tele from platform
func (n *serviceProvider) GetCommonServices(config config.Config) *commonInternalService {
	hooks := hooks.NewHooks()
	proxy := proxy.New(n.logger, hooks, config)
	intrumentation := core.New(n.logger, hooks, proxy)
	testDB := testdb.New(n.logger, config.Path+"/keploy", "", telemetry.Telemetry{})
	mockDB := mockdb.New(n.logger, telemetry.Telemetry{}, config.Path+"/keploy", "")
	reportDB := reportdb.New(n.logger, config.Path+"/keploy")
	return &commonInternalService{
		Instrumentation: intrumentation,
		YamlTestDB:      testDB,
		YamlMockDb:      mockDB,
		YamlReportDb:    reportDB,
	}
}

func (n *serviceProvider) GetService(cmd string, config config.Config) (interface{}, error) {
	telemetry := n.GetTelemetryService(config)
	telemetry.Ping()
	switch cmd {
	case "config", "update":
		return tools.NewTools(n.logger, telemetry), nil
	case "record", "test", "mock":
		commonServices := n.GetCommonServices(config)
		if cmd == "record" {
			record.New(n.logger, commonServices.YamlTestDB, commonServices.YamlMockDb, telemetry, commonServices.Instrumentation, config)
		}
		if cmd == "test" {
			replay.NewReplayer(n.logger, commonServices.YamlTestDB, commonServices.YamlMockDb, commonServices.YamlReportDb, telemetry, commonServices.Instrumentation, config)
		}
		return nil, nil
	default:
		return nil, errors.New("invalid command")
	}
}
