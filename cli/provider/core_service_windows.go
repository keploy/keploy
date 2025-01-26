//go:build windows

package provider

import (
	"context"
	"errors"

	"go.keploy.io/server/v2/config"
	"go.keploy.io/server/v2/pkg/models"
	"go.keploy.io/server/v2/pkg/platform/telemetry"
	"go.keploy.io/server/v2/pkg/platform/yaml/configdb/testset"
	mockdb "go.keploy.io/server/v2/pkg/platform/yaml/mockdb"
	openapidb "go.keploy.io/server/v2/pkg/platform/yaml/openapidb"
	reportdb "go.keploy.io/server/v2/pkg/platform/yaml/reportdb"
	testdb "go.keploy.io/server/v2/pkg/platform/yaml/testdb"
	"go.keploy.io/server/v2/pkg/service"
	"go.keploy.io/server/v2/pkg/service/contract"
	"go.uber.org/zap"
)

type CommonInternalService struct {
	commonPlatformServices
}

func Get(ctx context.Context, cmd string, cfg *config.Config, logger *zap.Logger, tel *telemetry.Telemetry, auth service.Auth) (interface{}, error) {
	commonServices, err := GetCommonServices(ctx, cfg, logger)
	if err != nil {
		return nil, err
	}
	contractSvc := contract.New(logger, commonServices.YamlTestDB, commonServices.YamlMockDb, commonServices.YamlOpenAPIDb, cfg)

	if cmd == "contract" {
		return contractSvc, nil
	}

	switch cmd {
	case "rerecord":
		return nil, errors.New("command not supported in non linux os. if you are on windows or mac, please use the dockerized version of your application")
	case "record":
		return nil, errors.New("command not supported in non linux os. if you are on windows or mac, please use the dockerized version of your application")
	case "test", "normalize", "templatize":
		return nil, errors.New("command not supported in non linux os. if you are on windows or mac, please use the dockerized version of your application")
	case "contract":
		return contractSvc, nil
	default:
		return nil, errors.New("command not supported in non linux os. if you are on windows or mac, please use the dockerized version of your application")
	}
}

func GetCommonServices(_ context.Context, c *config.Config, logger *zap.Logger) (*CommonInternalService, error) {

	testDB := testdb.New(logger, c.Path)
	mockDB := mockdb.New(logger, c.Path, "")
	openAPIdb := openapidb.New(logger, c.Path)
	reportDB := reportdb.New(logger, c.Path+"/reports")
	testSetDb := testset.New[*models.TestSet](logger, c.Path)
	return &CommonInternalService{
		commonPlatformServices{
			YamlTestDB:    testDB,
			YamlMockDb:    mockDB,
			YamlOpenAPIDb: openAPIdb,
			YamlReportDb:  reportDB,
			YamlTestSetDB: testSetDb,
		},
	}, nil
}

func GetAgent(ctx context.Context, cmd string, c *config.Config, logger *zap.Logger, auth service.Auth) (interface{}, error) {
	return nil, errors.New("command not supported in non linux os")
}
