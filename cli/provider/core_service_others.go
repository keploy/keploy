//go:build !linux

package provider

import (
	"context"
	"errors"

	"go.keploy.io/server/v2/config"
	"go.keploy.io/server/v2/pkg/core"
	"go.keploy.io/server/v2/pkg/models"
	"go.keploy.io/server/v2/pkg/platform/telemetry"
	"go.keploy.io/server/v2/pkg/platform/yaml/configdb/testset"
	mockdb "go.keploy.io/server/v2/pkg/platform/yaml/mockdb"
	reportdb "go.keploy.io/server/v2/pkg/platform/yaml/reportdb"
	testdb "go.keploy.io/server/v2/pkg/platform/yaml/testdb"
	"go.keploy.io/server/v2/pkg/service/replay"
	"go.uber.org/zap"
)

type CommonInternalService struct {
	commonPlatformServices
	Instrumentation *core.Core
}

func Get(ctx context.Context, cmd string, c *config.Config, logger *zap.Logger, tel *telemetry.Telemetry) (interface{}, error) {
	commonServices, err := GetCommonServices(ctx, c, logger)
	if err != nil {
		return nil, err
	}

	replaySvc := replay.NewReplayer(logger, commonServices.YamlTestDB, commonServices.YamlMockDb, commonServices.YamlReportDb, commonServices.YamlTestSetDB, tel, commonServices.Instrumentation, c)

	if (cmd == "test" && c.Test.BasePath != "") || cmd == "normalize" || cmd == "templatize" {
		return replaySvc, nil
	}

	return nil, errors.New("command not supported in non linux OS. if you are running on windows or mac, please try running the dockerized version of your application")
}

func GetCommonServices(_ context.Context, c *config.Config, logger *zap.Logger) (*CommonInternalService, error) {
	instrumentation := core.New(logger)
	testDB := testdb.New(logger, c.Path)
	mockDB := mockdb.New(logger, c.Path, "")
	reportDB := reportdb.New(logger, c.Path+"/reports")
	testSetDb := testset.New[*models.TestSet](logger, c.Path)
	return &CommonInternalService{
		commonPlatformServices{
			YamlTestDB:    testDB,
			YamlMockDb:    mockDB,
			YamlReportDb:  reportDB,
			YamlTestSetDB: testSetDb,
		},
		instrumentation,
	}, nil
}
