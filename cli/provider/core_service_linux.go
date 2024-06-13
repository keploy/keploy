//go:build linux

package provider

import (
	"context"
	"errors"
	"fmt"

	"github.com/docker/docker/api/types"
	"go.keploy.io/server/v2/config"
	"go.keploy.io/server/v2/pkg/core"
	"go.keploy.io/server/v2/pkg/core/hooks"
	"go.keploy.io/server/v2/pkg/core/proxy"
	"go.keploy.io/server/v2/pkg/core/tester"
	"go.keploy.io/server/v2/pkg/models"
	"go.keploy.io/server/v2/pkg/platform/docker"
	"go.keploy.io/server/v2/pkg/platform/telemetry"
	"go.keploy.io/server/v2/pkg/platform/yaml/configdb/testset"
	mockdb "go.keploy.io/server/v2/pkg/platform/yaml/mockdb"
	reportdb "go.keploy.io/server/v2/pkg/platform/yaml/reportdb"
	testdb "go.keploy.io/server/v2/pkg/platform/yaml/testdb"
	"go.keploy.io/server/v2/pkg/service/record"
	"go.keploy.io/server/v2/pkg/service/replay"
	"go.keploy.io/server/v2/utils"
	"go.uber.org/zap"
)

type CommonInternalService struct {
	YamlTestDB      *testdb.TestYaml
	YamlMockDb      *mockdb.MockYaml
	YamlReportDb    *reportdb.TestReport
	YamlTestSetDB   *testset.Db[*models.TestSet]
	Instrumentation *core.Core
}

func GetCoreService(ctx context.Context, cmd string, cfg *config.Config, logger *zap.Logger, tel *telemetry.Telemetry) (interface{}, error) {
	commonServices, err := GetCommonServices(ctx, cfg, logger)
	if err != nil {
		return nil, err
	}
	if cmd == "record" {
		return record.New(logger, commonServices.YamlTestDB, commonServices.YamlMockDb, tel, commonServices.Instrumentation, cfg), nil
	}
	if cmd == "test" || cmd == "normalize" {
		return replay.NewReplayer(logger, commonServices.YamlTestDB, commonServices.YamlMockDb, commonServices.YamlReportDb, commonServices.YamlTestSetDB, tel, commonServices.Instrumentation, cfg), nil
	}
	return nil, errors.New("invalid command")
}

func GetCommonServices(ctx context.Context, c *config.Config, logger *zap.Logger) (*CommonInternalService, error) {

	h := hooks.NewHooks(logger, c)
	p := proxy.New(logger, h, c)
	//for keploy test bench
	t := tester.New(logger, h)

	var client docker.Client
	var err error
	if utils.IsDockerKind(utils.CmdType(c.CommandType)) {
		client, err = docker.New(logger)
		if err != nil {
			utils.LogError(logger, err, "failed to create docker client")
		}

		addKeployNetwork(ctx, logger, client)
		err := client.CreateVolume(ctx, "debugfs")
		if err != nil {
			utils.LogError(logger, err, "failed to debugfs volume")
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
	reportDB := reportdb.New(logger, c.Path+"/reports")
	testSetDb := testset.New[*models.TestSet](logger, c.Path)
	return &CommonInternalService{
		Instrumentation: instrumentation,
		YamlTestDB:      testDB,
		YamlMockDb:      mockDB,
		YamlReportDb:    reportDB,
		YamlTestSetDB:   testSetDb,
	}, nil
}

func addKeployNetwork(ctx context.Context, logger *zap.Logger, client docker.Client) {

	// Check if the 'keploy-network' network exists
	networks, err := client.NetworkList(ctx, types.NetworkListOptions{})
	if err != nil {
		logger.Debug("failed to list docker networks")
		return
	}

	for _, network := range networks {
		if network.Name == "keploy-network" {
			logger.Debug("keploy network already exists")
			return
		}
	}

	// Create the 'keploy' network if it doesn't exist
	_, err = client.NetworkCreate(ctx, "keploy-network", types.NetworkCreate{
		CheckDuplicate: true,
	})
	if err != nil {
		logger.Debug("failed to create keploy network")
		return
	}

	logger.Debug("keploy network created")
}
