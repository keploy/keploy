//go:build linux

package provider

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"

	"go.keploy.io/server/v2/config"
	"go.keploy.io/server/v2/pkg/core"
	"go.keploy.io/server/v2/pkg/core/hooks"
	"go.keploy.io/server/v2/pkg/core/proxy"
	"go.keploy.io/server/v2/pkg/core/tester"
	"go.keploy.io/server/v2/pkg/models"
	"go.keploy.io/server/v2/pkg/platform/docker"
	"go.keploy.io/server/v2/pkg/platform/http"
	"go.keploy.io/server/v2/pkg/platform/storage"
	"go.keploy.io/server/v2/pkg/platform/telemetry"
	"go.keploy.io/server/v2/pkg/platform/yaml/configdb/testset"
	mockdb "go.keploy.io/server/v2/pkg/platform/yaml/mockdb"
	openapidb "go.keploy.io/server/v2/pkg/platform/yaml/openapidb"
	reportdb "go.keploy.io/server/v2/pkg/platform/yaml/reportdb"
	testdb "go.keploy.io/server/v2/pkg/platform/yaml/testdb"
	"go.keploy.io/server/v2/pkg/service"
	"go.keploy.io/server/v2/pkg/service/contract"
	"go.keploy.io/server/v2/pkg/service/orchestrator"
	"go.keploy.io/server/v2/pkg/service/record"
	"go.keploy.io/server/v2/pkg/service/replay"
	"go.keploy.io/server/v2/pkg/service/tools"
	"go.keploy.io/server/v2/utils"
	"go.uber.org/zap"
)

type CommonInternalService struct {
	commonPlatformServices
	Instrumentation *core.Core
}

func Get(ctx context.Context, cmd string, cfg *config.Config, logger *zap.Logger, tel *telemetry.Telemetry, auth service.Auth) (interface{}, error) {
	commonServices, err := GetCommonServices(ctx, cfg, logger)
	if err != nil {
		return nil, err
	}
	contractSvc := contract.New(logger, commonServices.YamlTestDB, commonServices.YamlMockDb, commonServices.YamlOpenAPIDb, cfg)
	recordSvc := record.New(logger, commonServices.YamlTestDB, commonServices.YamlMockDb, tel, commonServices.Instrumentation, cfg)
	replaySvc := replay.NewReplayer(logger, commonServices.YamlTestDB, commonServices.YamlMockDb, commonServices.YamlReportDb, commonServices.YamlTestSetDB, tel, commonServices.Instrumentation, auth, commonServices.Storage, commonServices.HttpClient, cfg)
	toolsSvc := tools.NewTools(logger, commonServices.YamlTestSetDB, commonServices.YamlTestDB, tel, auth, cfg)
	switch cmd {
	case "rerecord":
		return orchestrator.New(logger, recordSvc, toolsSvc, replaySvc, cfg), nil
	case "record":
		return recordSvc, nil
	case "test", "normalize":
		return replaySvc, nil
	case "templatize", "config", "update", "login", "export", "import":
		return toolsSvc, nil
	case "contract":
		return contractSvc, nil
	default:
		return nil, errors.New("invalid command")
	}

}

func GetCommonServices(_ context.Context, c *config.Config, logger *zap.Logger) (*CommonInternalService, error) {

	h := hooks.NewHooks(logger, c)
	p := proxy.New(logger, h, c)
	//for keploy test bench
	t := tester.New(logger, h)

	var client docker.Client
	var err error
	if utils.IsDockerCmd(utils.CmdType(c.CommandType)) {
		client, err = docker.New(logger)
		if err != nil {
			utils.LogError(logger, err, "failed to create docker client")
		}

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

	instrumentation := core.New(logger, h, p, t, client)

	testDB := testdb.New(logger, c.Path)
	mockDB := mockdb.New(logger, c.Path, "")
	openAPIdb := openapidb.New(logger, filepath.Join(c.Path, "schema"))
	reportDB := reportdb.New(logger, c.Path+"/reports")
	testSetDb := testset.New[*models.TestSet](logger, c.Path)
	storage := storage.New(c.APIServerURL, logger)
	http := http.New(logger)
	return &CommonInternalService{
		commonPlatformServices{
			YamlTestDB:    testDB,
			YamlMockDb:    mockDB,
			YamlOpenAPIDb: openAPIdb,
			YamlReportDb:  reportDB,
			YamlTestSetDB: testSetDb,
			Storage:       storage,
			HttpClient:    http,
		},
		instrumentation,
	}, nil
}
