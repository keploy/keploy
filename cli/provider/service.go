package provider

import (
	"context"
	"errors"
	"fmt"

	"go.keploy.io/server/v2/config"
	"go.keploy.io/server/v2/pkg/core"
	"go.keploy.io/server/v2/pkg/core/hooks"
	"go.keploy.io/server/v2/pkg/core/proxy"
	"go.keploy.io/server/v2/pkg/core/tester"

	"go.keploy.io/server/v2/pkg/models"
	"go.keploy.io/server/v2/pkg/platform/docker"
	"go.keploy.io/server/v2/pkg/platform/telemetry"
	"go.keploy.io/server/v2/pkg/platform/yaml/configdb/testset"
	"go.keploy.io/server/v2/pkg/platform/yaml/configdb/user"
	mockdb "go.keploy.io/server/v2/pkg/platform/yaml/mockdb"
	reportdb "go.keploy.io/server/v2/pkg/platform/yaml/reportdb"
	testdb "go.keploy.io/server/v2/pkg/platform/yaml/testdb"

	"go.keploy.io/server/v2/pkg/service/record"
	"go.keploy.io/server/v2/pkg/service/replay"
	"go.keploy.io/server/v2/pkg/service/tools"
	"go.keploy.io/server/v2/pkg/service/utgen"
	"go.keploy.io/server/v2/utils"
	"go.uber.org/zap"
)

type ServiceProvider struct {
	logger *zap.Logger
	userDb *user.Db
	cfg    *config.Config
}

type CommonInternalService struct {
	YamlTestDB      *testdb.TestYaml
	YamlMockDb      *mockdb.MockYaml
	YamlReportDb    *reportdb.TestReport
	YamlTestSetDB   *testset.Db[*models.TestSet]
	Instrumentation *core.Core
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
		GlobalMap:      map[string]interface{}{},
		InstallationID: installationID,
	},
	), nil
}

func (n *ServiceProvider) GetCommonServices(c *config.Config) *CommonInternalService {

	h := hooks.NewHooks(n.logger, c)
	p := proxy.New(n.logger, h, c)
	//for keploy test bench
	t := tester.New(n.logger, h)

	var client docker.Client
	var err error
	if utils.IsDockerKind(utils.CmdType(c.CommandType)) {
		client, err = docker.New(n.logger)
		if err != nil {
			utils.LogError(n.logger, err, "failed to create docker client")
		}

		//parse docker command only in case of docker start or docker run commands
		if utils.CmdType(c.CommandType) != utils.DockerCompose {

			cont, net, err := docker.ParseDockerCmd(c.Command, utils.CmdType(c.CommandType), client)
			n.logger.Debug("container and network parsed from command", zap.String("container", cont), zap.String("network", net), zap.String("command", c.Command))
			if err != nil {
				utils.LogError(n.logger, err, "failed to parse container name from given docker command", zap.String("cmd", c.Command))
			}
			if c.ContainerName != "" && c.ContainerName != cont {
				n.logger.Warn(fmt.Sprintf("given app container:(%v) is different from parsed app container:(%v), taking parsed value", c.ContainerName, cont))
			}
			c.ContainerName = cont

			if c.NetworkName != "" && c.NetworkName != net {
				n.logger.Warn(fmt.Sprintf("given docker network:(%v) is different from parsed docker network:(%v), taking parsed value", c.NetworkName, net))
			}
			c.NetworkName = net

			n.logger.Debug("Using container and network", zap.String("container", c.ContainerName), zap.String("network", c.NetworkName))
		}
	}

	instrumentation := core.New(n.logger, h, p, t, client)
	testDB := testdb.New(n.logger, c.Path)
	mockDB := mockdb.New(n.logger, c.Path, "")
	reportDB := reportdb.New(n.logger, c.Path+"/reports")
	testSetDb := testset.New[*models.TestSet](n.logger, c.Path)
	return &CommonInternalService{
		Instrumentation: instrumentation,
		YamlTestDB:      testDB,
		YamlMockDb:      mockDB,
		YamlReportDb:    reportDB,
		YamlTestSetDB:   testSetDb,
	}
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
		return utgen.NewUnitTestGenerator(n.cfg.Gen.SourceFilePath, n.cfg.Gen.TestFilePath, n.cfg.Gen.CoverageReportPath, n.cfg.Gen.TestCommand, n.cfg.Gen.TestDir, n.cfg.Gen.CoverageFormat, n.cfg.Gen.DesiredCoverage, n.cfg.Gen.MaxIterations, n.cfg.Gen.Model, n.cfg.Gen.APIBaseURL, n.cfg, tel, n.logger)
	case "record", "test", "mock", "normalize":
		commonServices := n.GetCommonServices(n.cfg)
		if cmd == "record" {
			return record.New(n.logger, commonServices.YamlTestDB, commonServices.YamlMockDb, tel, commonServices.Instrumentation, n.cfg), nil
		}
		if cmd == "test" || cmd == "normalize" {
			return replay.NewReplayer(n.logger, commonServices.YamlTestDB, commonServices.YamlMockDb, commonServices.YamlReportDb, commonServices.YamlTestSetDB, tel, commonServices.Instrumentation, n.cfg), nil
		}
		return nil, errors.New("invalid command")
	default:
		return nil, errors.New("invalid command")
	}
}
