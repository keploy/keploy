package tools

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"sync"

	"go.keploy.io/server/v2/config"
	"go.keploy.io/server/v2/pkg/service"
	"go.keploy.io/server/v2/pkg/service/export"
	postmanimport "go.keploy.io/server/v2/pkg/service/import"
	"go.keploy.io/server/v2/pkg/service/update" // <-- This is now the update service
	"go.keploy.io/server/v2/utils"
	"go.uber.org/zap"
	yamlLib "gopkg.in/yaml.v3"
)

// NewTools now accepts the update.Service
func NewTools(logger *zap.Logger, testsetConfig TestSetConfig, testDB TestDB, reportDB ReportDB, telemetry teleDB, auth service.Auth, config *config.Config, updater update.Service) Service {
	return &Tools{
		logger:      logger,
		telemetry:   telemetry,
		auth:        auth,
		testSetConf: testsetConfig,
		testDB:      testDB,
		reportDB:    reportDB,
		config:      config,
		update:      updater, // <-- Store the updater
	}
}

type Tools struct {
	logger      *zap.Logger
	telemetry   teleDB
	testSetConf TestSetConfig
	testDB      TestDB
	reportDB    ReportDB
	config      *config.Config
	auth        service.Auth
	update      update.Service // <-- Holds the update service
}

var ErrGitHubAPIUnresponsive = errors.New("GitHub API is unresponsive")

func (t *Tools) SendTelemetry(event string, output ...*sync.Map) {
	t.telemetry.SendTelemetry(event, output...)
}

func (t *Tools) Export(ctx context.Context) error {
	return export.Export(ctx, t.logger)
}

func (t *Tools) Import(ctx context.Context, path, basePath string) error {
	postmanImport := postmanimport.NewPostmanImporter(ctx, t.logger)
	return postmanImport.Import(path, basePath)
}

// Update now just delegates the call to the update service
func (t *Tools) Update(ctx context.Context) error {
	return t.update.Update(ctx)
}

func (t *Tools) CreateConfig(_ context.Context, filePath string, configData string) error {
	var node yamlLib.Node
	var data []byte
	var err error

	if configData != "" {
		data = []byte(configData)
	} else {
		configData, err = config.Merge(config.InternalConfig, config.GetDefaultConfig())
		if err != nil {
			utils.LogError(t.logger, err, "failed to create default config string")
			return nil
		}
		data = []byte(configData)
	}

	if err := yamlLib.Unmarshal(data, &node); err != nil {
		utils.LogError(t.logger, err, "failed to unmarshal the config")
		return nil
	}
	results, err := yamlLib.Marshal(node.Content[0])
	if err != nil {
		utils.LogError(t.logger, err, "failed to marshal the config")
		return nil
	}

	finalOutput := append(results, []byte(utils.ConfigGuide)...)
	finalOutput = append([]byte(utils.GetVersionAsComment()), finalOutput...)

	err = os.WriteFile(filePath, finalOutput, fs.ModePerm)
	if err != nil {
		utils.LogError(t.logger, err, "failed to write config file")
		return nil
	}

	err = os.Chmod(filePath, 0777) // Set permissions to 777
	if err != nil {
		utils.LogError(t.logger, err, "failed to set the permission of config file")
		return nil
	}

	return nil
}

func (t *Tools) IgnoreTests(_ context.Context, _ string, _ []string) error {
	return nil
}

func (t *Tools) IgnoreTestSet(_ context.Context, _ string) error {
	return nil
}

func (t *Tools) Login(ctx context.Context) bool {
	return t.auth.Login(ctx)
}

func (t *Tools) Templatize(ctx context.Context) error {

	testSets := t.config.Templatize.TestSets
	if len(testSets) == 0 {
		all, err := t.testDB.GetAllTestSetIDs(ctx)
		if err != nil {
			utils.LogError(t.logger, err, "failed to get all test sets")
			return err
		}
		testSets = all
	}

	if len(testSets) == 0 {
		t.logger.Warn("No test sets found to templatize")
		return nil
	}

	for _, testSetID := range testSets {

		testSet, err := t.testSetConf.Read(ctx, testSetID)
		if err == nil && (testSet != nil && testSet.Template != nil) {
			utils.TemplatizedValues = testSet.Template
		} else {
			utils.TemplatizedValues = make(map[string]interface{})
		}

		if err == nil && (testSet != nil && testSet.Secret != nil) {
			utils.SecretValues = testSet.Secret
		} else {
			utils.SecretValues = make(map[string]interface{})
		}

		// Get test cases from the database
		tcs, err := t.testDB.GetTestCases(ctx, testSetID)
		if err != nil {
			utils.LogError(t.logger, err, "failed to get test cases")
			return err
		}

		if len(tcs) == 0 {
			t.logger.Warn("The test set is empty. Please record some test cases to templatize.", zap.String("testSet", testSetID))
			continue
		}

		err = t.ProcessTestCasesV2(ctx, tcs, testSetID)
		if err != nil {
			utils.LogError(t.logger, err, "failed to process test cases")
			return err
		}
	}
	return nil
}