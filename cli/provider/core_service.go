package provider

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/client/app"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/platform/docker"
	"go.keploy.io/server/v3/pkg/platform/http"
	"go.keploy.io/server/v3/pkg/platform/storage"
	"go.keploy.io/server/v3/pkg/platform/telemetry"
	"go.keploy.io/server/v3/pkg/platform/yaml/configdb/testset"
	"go.keploy.io/server/v3/pkg/platform/yaml/mapdb"
	mockdb "go.keploy.io/server/v3/pkg/platform/yaml/mockdb"
	openapidb "go.keploy.io/server/v3/pkg/platform/yaml/openapidb"
	reportdb "go.keploy.io/server/v3/pkg/platform/yaml/reportdb"
	testdb "go.keploy.io/server/v3/pkg/platform/yaml/testdb"
	"go.keploy.io/server/v3/pkg/service"
	"go.keploy.io/server/v3/pkg/service/contract"
	"go.keploy.io/server/v3/pkg/service/orchestrator"
	"go.keploy.io/server/v3/pkg/service/record"
	"go.keploy.io/server/v3/pkg/service/replay"
	"go.keploy.io/server/v3/pkg/service/report"
	"go.keploy.io/server/v3/pkg/service/tools"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
)

type CommonInternalService struct {
	commonPlatformServices
	Instrumentation *http.AgentClient
}

func Get(ctx context.Context, cmd string, cfg *config.Config, logger *zap.Logger, tel *telemetry.Telemetry, auth service.Auth) (interface{}, error) {
	commonServices, err := GetCommonServices(ctx, cfg, logger)
	if err != nil {
		return nil, err
	}
	contractSvc := contract.New(logger, commonServices.YamlTestDB, commonServices.YamlMockDb, commonServices.YamlOpenAPIDb, cfg)
	recordSvc := record.New(logger, commonServices.YamlTestDB, commonServices.YamlMockDb, commonServices.YamlMappingDb, tel, commonServices.Instrumentation, commonServices.YamlTestSetDB, cfg)
	replaySvc := replay.NewReplayer(logger, commonServices.YamlTestDB, commonServices.YamlMockDb, commonServices.YamlReportDb, commonServices.YamlMappingDb, commonServices.YamlTestSetDB, tel, commonServices.Instrumentation, auth, commonServices.Storage, cfg)
	toolsSvc := tools.NewTools(logger, commonServices.YamlTestSetDB, commonServices.YamlTestDB, commonServices.YamlReportDb, tel, auth, cfg)
	reportSvc := report.New(logger, cfg, commonServices.YamlReportDb, commonServices.YamlTestDB)
	switch cmd {
	case "rerecord":
		return orchestrator.New(logger, recordSvc, toolsSvc, replaySvc, cfg), nil
	case "record":
		return recordSvc, nil
	case "test", "mock":
		return replaySvc, nil
	case "templatize", "config", "update", "login", "export", "import", "sanitize", "normalize":
		return toolsSvc, nil
	case "contract":
		return contractSvc, nil
	case "report":
		return reportSvc, nil
	default:
		return nil, errors.New("invalid command")
	}

}

func GetCommonServices(ctx context.Context, c *config.Config, logger *zap.Logger) (*CommonInternalService, error) {

	app.HookImpl = app.NewHooks(logger)
	logger.Debug("app hooks initialized - oss")

	var client docker.Client
	var err error

	if utils.IsDockerCmd(utils.CmdType(c.CommandType)) {
		client, err = docker.New(logger, c)
		if err != nil {
			utils.LogError(logger, err, "failed to create docker client")
		}
		c.Agent.IsDocker = true

		//parse docker command only in case of docker start or docker run commands
		if utils.CmdType(c.CommandType) != utils.DockerCompose {
			cont, net, err := docker.ParseDockerCmd(c.Command, utils.CmdType(c.CommandType), client)
			logger.Debug("container and network parsed from command", zap.String("container", cont), zap.String("network", net), zap.String("command", c.Command))
			if err != nil {
				utils.LogError(logger, err, "failed to parse container name from given docker command", zap.String("cmd", c.Command))
			}
			if c.ContainerName != "" && c.ContainerName != cont {
				logger.Warn(fmt.Sprintf("given app container:(%v) is different from parsed app container:(%v), taking parsed value", c.ContainerName, cont))
			}
			c.ContainerName = cont

			if c.NetworkName != "" && c.NetworkName != net {
				logger.Warn(fmt.Sprintf("given docker network:(%v) is different from parsed docker network:(%v), taking parsed value", c.NetworkName, net))
			}
			c.NetworkName = net

			logger.Debug("Using container and network", zap.String("container", c.ContainerName), zap.String("network", c.NetworkName))
		}
	}

	instrumentation := http.New(logger, client, c)

	testDB := testdb.New(logger, c.Path)
	mockDB := mockdb.New(logger, c.Path, "")
	mapDB := mapdb.New(logger, c.Path, "")
	openAPIdb := openapidb.New(logger, filepath.Join(c.Path, "schema"))
	reportDB := reportdb.New(logger, c.Path+"/reports")
	testSetDb := testset.New[*models.TestSet](logger, c.Path)
	storage := storage.New(c.APIServerURL, logger)
	return &CommonInternalService{
		commonPlatformServices{
			YamlTestDB:    testDB,
			YamlMockDb:    mockDB,
			YamlMappingDb: mapDB,
			YamlOpenAPIDb: openAPIdb,
			YamlReportDb:  reportDB,
			YamlTestSetDB: testSetDb,
			Storage:       storage,
		},
		instrumentation,
	}, nil
}
