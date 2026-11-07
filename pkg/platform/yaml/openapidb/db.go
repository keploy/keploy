// Package openapidb provides a openAPI database implementation.
package openapidb

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/platform/yaml"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
	yamlLib "gopkg.in/yaml.v3"
)

type OpenAPIYaml struct {
	OpenAPIPath string
	logger      *zap.Logger
}

func New(logger *zap.Logger, openAPIPath string) *OpenAPIYaml {
	return &OpenAPIYaml{
		OpenAPIPath: openAPIPath,
		logger:      logger,
	}
}
func (ts *OpenAPIYaml) GetTestCasesSchema(ctx context.Context, testSetID string, testPath string) ([]*models.OpenAPI, error) {
	root := testPath
	if root == "" {
		root = ts.OpenAPIPath
	}
	path := filepath.Join(root, testSetID)

	tcs := []*models.OpenAPI{}
	TestPath, err := yaml.ValidatePath(path)
	if err != nil {
		return nil, err
	}
	// Only a set folder that does not exist means no tests. One that is
	// there but cannot be opened (permission denied, an I/O error, a
	// symlink loop, not a directory) is an error: reading it as empty would
	// report a set's schemas as none.
	dir, err := yaml.ReadDir(TestPath, fs.ModePerm)
	if errors.Is(err, fs.ErrNotExist) {
		ts.logger.Debug("no tests are recorded for the session", zap.String("index", testSetID))
		return nil, nil
	}
	if err != nil {
		utils.LogError(ts.logger, err, "failed to open the directory containing yaml testcases", zap.String("path", TestPath))
		return nil, err
	}
	defer func() { _ = dir.Close() }()
	files, err := dir.ReadDir(0)
	if err != nil {
		utils.LogError(ts.logger, err, "failed to read the file names of yaml testcases", zap.String("path", TestPath))
		return nil, err
	}
	for _, j := range files {

		name := strings.TrimSuffix(j.Name(), filepath.Ext(j.Name()))
		data, err := yaml.ReadRecordingFileF(ctx, ts.logger, root, TestPath, name, yaml.FormatYAML)
		if err != nil {
			utils.LogError(ts.logger, err, "failed to read the testcase from yaml")
			return nil, err
		}

		var testCase *models.OpenAPI
		err = yamlLib.Unmarshal(data, &testCase)
		if err != nil {
			utils.LogError(ts.logger, err, "failed to unmarshall YAML data")
			return nil, err
		}

		tcs = append(tcs, testCase)
	}

	return tcs, nil
}

func (ts *OpenAPIYaml) GetMocksSchemas(ctx context.Context, testSetID string, mockPath string, mockFileName string) ([]*models.OpenAPI, error) {

	var tcsMocks = make([]*models.OpenAPI, 0)

	root := mockPath
	path := filepath.Join(root, testSetID)
	mockPath, err := yaml.ValidatePath(path + "/" + mockFileName + ".yaml")
	if err != nil {
		return nil, err
	}

	// Only a schema file that does not exist means no mocks. One that is
	// there but cannot be looked at (permission denied, an I/O error, a
	// symlink loop) is an error: reading it as empty would report the mock
	// set's schemas as none.
	_, err = os.Stat(mockPath)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		utils.LogError(ts.logger, err, "failed to look at the mocks schema file", zap.String("path", mockPath))
		return nil, err
	}
	if err == nil {
		var mockYamls []*models.OpenAPI
		data, err := yaml.ReadRecordingFileF(ctx, ts.logger, root, path, mockFileName, yaml.FormatYAML)
		if err != nil {
			utils.LogError(ts.logger, err, "failed to read the mocks from config yaml", zap.String("session", filepath.Base(path)))
			return nil, err
		}
		dec := yamlLib.NewDecoder(bytes.NewReader(data))
		for {
			var doc *models.OpenAPI
			err := dec.Decode(&doc)
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				return nil, fmt.Errorf("failed to decode the yaml file documents. error: %v", err.Error())
			}
			mockYamls = append(mockYamls, doc)
		}
		if err != nil {
			utils.LogError(ts.logger, err, "failed to decode the config mocks from yaml docs", zap.String("session", filepath.Base(path)))
			return nil, err
		}
		tcsMocks = mockYamls
	}

	return tcsMocks, nil
}
func (ts *OpenAPIYaml) ChangePath(path string) {

	// ts.OpenAPIPath = "./keploy/"
	ts.OpenAPIPath = path
}

// WriteSchema writes a schema file, name, in outputPath, a folder of the
// recording at root (see yaml.WriteRecordingFileF).
func (ts *OpenAPIYaml) WriteSchema(ctx context.Context, logger *zap.Logger, root, outputPath, name string, openapi models.OpenAPI, isAppend bool) error {
	openapiYAML, err := yamlLib.Marshal(openapi)
	if err != nil {
		return err
	}
	_, err = yaml.StatInRecording(root, outputPath)
	if os.IsNotExist(err) {
		err = yaml.MkdirAllInRecording(root, outputPath, os.ModePerm)
		if err != nil {
			utils.LogError(logger, err, "failed to create directory", zap.String("directory", outputPath))
			return err
		}
		logger.Info("Directory created", zap.String("directory", outputPath))
	}

	err = yaml.WriteRecordingFileF(ctx, logger, root, outputPath, name, openapiYAML, isAppend, yaml.FormatYAML)
	if err != nil {
		utils.LogError(logger, err, "failed to write OpenAPI YAML to a file", zap.String("outputPath", outputPath), zap.String("name", name))
		return err
	}

	outputFilePath := outputPath + "/" + name + ".yaml"
	logger.Info("OpenAPI YAML has been saved to ", zap.String("path", outputFilePath))
	return nil
}
