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
	"go.keploy.io/server/v3/pkg/platform/yaml"
	"go.keploy.io/server/v3/pkg/platform/yaml/configdb/testset"
	"go.keploy.io/server/v3/pkg/platform/yaml/mapdb"
	mockdb "go.keploy.io/server/v3/pkg/platform/yaml/mockdb"
	openapidb "go.keploy.io/server/v3/pkg/platform/yaml/openapidb"
	reportdb "go.keploy.io/server/v3/pkg/platform/yaml/reportdb"
	testdb "go.keploy.io/server/v3/pkg/platform/yaml/testdb"
	"go.keploy.io/server/v3/pkg/service/contract"
	"go.keploy.io/server/v3/pkg/service/diff"
	"go.keploy.io/server/v3/pkg/service/mock"
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

func Get(ctx context.Context, cmd string, cfg *config.Config, logger *zap.Logger, tel *telemetry.Telemetry) (interface{}, error) {
	commonServices, err := GetCommonServices(ctx, cfg, logger)
	if err != nil {
		return nil, err
	}
	contractSvc := contract.New(logger, commonServices.YamlTestDB, commonServices.YamlMockDb, commonServices.YamlOpenAPIDb, cfg)
	recordSvc := record.New(logger, commonServices.YamlTestDB, commonServices.YamlMockDb, commonServices.YamlMappingDb, tel, commonServices.Instrumentation, commonServices.YamlTestSetDB, nil, cfg)
	logger.Debug("async: lanes parsed from config", zap.Int("count", len(cfg.Async.Lanes)))
	if len(cfg.Async.Lanes) > 0 {
		if rec, ok := recordSvc.(*record.Recorder); ok {
			parsers := record.ResolveAsyncParsers(logger, cfg.Async.Lanes)
			rec.SetRecordHooks(record.NewAsyncRecorder(logger, cfg.Async.Lanes, parsers))
		}
	}
	replaySvc := replay.NewReplayer(logger, commonServices.YamlTestDB, commonServices.YamlMockDb, commonServices.YamlReportDb, commonServices.YamlMappingDb, commonServices.YamlTestSetDB, tel, commonServices.Instrumentation, commonServices.Storage, cfg)
	// mockSvc reuses the SAME AgentClient instrumentation, mock/mapping DBs and
	// (in enterprise) RecordHooks that record/replay use, so every runtime
	// behaviour those apply to the wrapped command — LD_PRELOAD/java-agent/Go
	// binary TLS shims, time-freeze, secret obfuscation — applies to
	// `keploy mock` unchanged. OSS uses the file-backed store.
	mockSvc := mock.New(logger, commonServices.Instrumentation, commonServices.YamlMockDb, commonServices.YamlMappingDb, mock.FileStore{}, nil, cfg)
	toolsSvc := tools.NewTools(logger, commonServices.YamlTestSetDB, commonServices.YamlTestDB, commonServices.YamlReportDb, tel, cfg)
	reportSvc := report.New(logger, cfg, commonServices.YamlReportDb, commonServices.YamlTestDB)
	diffSvc := diff.New(logger, commonServices.YamlReportDb, commonServices.YamlTestDB)
	switch cmd {
	case "record":
		return recordSvc, nil
	case "test":
		return replaySvc, nil
	case "mock-record", "mock-replay":
		return mockSvc, nil
	case "templatize", "config", "update", "export", "import", "sanitize", "normalize":
		return toolsSvc, nil
	case "contract":
		return contractSvc, nil
	case "report":
		return reportSvc, nil
	case "diff":
		return diffSvc, nil
	default:
		return nil, errors.New("invalid command")
	}

}

// resolveDockerNames settles which container and network keploy will act on,
// and reports the outcome at a severity that matches it.
//
// A parse that produced nothing must never overwrite a value the user gave
// through --container-name/--network-name or keploy.yml. It used to: the
// assignment was unconditional, so a command keploy could not parse left the
// container name empty, and everything downstream - the name-conflict
// pre-clean, teardown, and app-log capture - then operated on "" and silently
// did nothing.
//
// A parse that DID produce a value wins over the flag, because for docker-run
// and docker-start the command is ground truth: docker names the container
// from the command, so instrumenting anything else would target a container
// that was never created. The parse is usually right rather than provably
// right - it reads the leftmost --name, which a container's own arguments can
// still shadow - so a disagreement is warned about rather than silently
// applied.
//
// Severity is decided AFTER applying, against what was actually resolved.
// ParseDockerCmd reports a missing --network as an error even when the
// container parsed perfectly, which is the ordinary default-bridge shape; if
// the level were chosen before applying, every such run would print an ERROR
// naming the container it had in fact just resolved. Several e2e lanes fail
// the build on any ERROR line.
func resolveDockerNames(logger *zap.Logger, c *config.Config, cont, net string, err error) {
	if cont != "" {
		if c.ContainerName != "" && c.ContainerName != cont {
			logger.Warn("given app container differs from the one named in the docker command; using the command's",
				zap.String("given", c.ContainerName), zap.String("parsed", cont))
		}
		c.ContainerName = cont
	}

	if net != "" {
		if c.NetworkName != "" && c.NetworkName != net {
			logger.Warn("given docker network differs from the one named in the docker command; using the command's",
				zap.String("given", c.NetworkName), zap.String("parsed", net))
		}
		c.NetworkName = net
	}

	if err == nil {
		return
	}

	switch {
	case c.ContainerName == "":
		utils.LogError(logger, err, "failed to resolve the app container from the docker command", zap.String("cmd", c.Command))
	case cont == "":
		logger.Warn("could not read a container name out of the docker command; using the one that was provided",
			zap.String("containerName", c.ContainerName), zap.Error(err))
	default:
		// The container resolved and only the network did not. Nothing reads
		// the network name today, so this is not worth the user's attention.
		logger.Debug("docker command parsed without a network name", zap.String("containerName", c.ContainerName), zap.Error(err))
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
			resolveDockerNames(logger, c, cont, net, err)

			logger.Debug("Using container and network", zap.String("container", c.ContainerName), zap.String("network", c.NetworkName))
		}
	}

	instrumentation := http.New(logger, client, c)

	// MockFormat (yaml vs gob — chosen for record-time CPU vs grep-friendliness)
	// is orthogonal to StorageFormat (yaml vs json — chosen for the on-disk
	// document encoding). Both knobs flow through here. The KEPLOY_MOCK_FORMAT
	// env var still wins over Record.MockFormat for ad-hoc runs.
	mockdb.SetConfiguredMockFormat(c.Record.MockFormat)

	format := yaml.ParseFormat(c.StorageFormat)
	namingStrategy, err := testdb.ParseNamingStrategy(c.Record.TestCaseNaming)
	if err != nil {
		return nil, fmt.Errorf("invalid record.testCaseNaming in keploy.yml: %w (set it to %q or %q)",
			err, testdb.NamingDescriptive, testdb.NamingSequential)
	}
	testDB := testdb.NewWithFormatAndNaming(logger, c.Path, format, namingStrategy)
	mockDB := mockdb.NewWithFormat(logger, c.Path, "", format)
	mapDB := mapdb.NewWithFormat(logger, c.Path, "", format)
	openAPIdb := openapidb.New(logger, filepath.Join(c.Path, "schema"))
	reportDB := reportdb.NewWithFormat(logger, c.Path+"/reports", format)
	testSetDb := testset.NewWithFormat[*models.TestSet](logger, c.Path, format)
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
