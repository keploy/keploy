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

	"go.keploy.io/server/v2/pkg/models"
	"go.keploy.io/server/v2/pkg/platform/yaml"
	"go.keploy.io/server/v2/utils"
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
	var path string
	if testPath == "" {
		path = filepath.Join(ts.OpenAPIPath, testSetID)

	} else {
		path = filepath.Join(testPath, testSetID)
	}

	tcs := []*models.OpenAPI{}
	TestPath, err := yaml.ValidatePath(path)
	if err != nil {
		return nil, err
	}
	_, err = os.Stat(TestPath)
	if err != nil {
		ts.logger.Debug("no tests are recorded for the session", zap.String("index", testSetID))
		return nil, nil
	}
	dir, err := yaml.ReadDir(TestPath, fs.ModePerm)
	if err != nil {
		utils.LogError(ts.logger, err, "failed to open the directory containing yaml testcases", zap.Any("path", TestPath))
		return nil, err
	}
	files, err := dir.ReadDir(0)
	if err != nil {
		utils.LogError(ts.logger, err, "failed to read the file names of yaml testcases", zap.Any("path", TestPath))
		return nil, err
	}
	for _, j := range files {

		name := strings.TrimSuffix(j.Name(), filepath.Ext(j.Name()))
		data, err := yaml.ReadFile(ctx, ts.logger, TestPath, name)
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

	path := filepath.Join(mockPath, testSetID)
	mockPath, err := yaml.ValidatePath(path + "/" + mockFileName + ".yaml")
	if err != nil {
		return nil, err
	}

	if _, err := os.Stat(mockPath); err == nil {
		var mockYamls []*models.OpenAPI
		data, err := yaml.ReadFile(ctx, ts.logger, path, mockFileName)
		if err != nil {
			utils.LogError(ts.logger, err, "failed to read the mocks from config yaml", zap.Any("session", filepath.Base(path)))
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
			utils.LogError(ts.logger, err, "failed to decode the config mocks from yaml docs", zap.Any("session", filepath.Base(path)))
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

func (ts *OpenAPIYaml) WriteSchema(ctx context.Context, logger *zap.Logger, outputPath, name string, openapi models.OpenAPI, isAppend bool) error {
	openapiYAML, err := yamlLib.Marshal(openapi)
	if err != nil {
		return err
	}
	_, err = os.Stat(outputPath)
	if os.IsNotExist(err) {
		err = os.MkdirAll(outputPath, os.ModePerm)
		if err != nil {
			utils.LogError(logger, err, "failed to create directory", zap.String("directory", outputPath))
			return err
		}
		logger.Info("Directory created", zap.String("directory", outputPath))
	}

	err = yaml.WriteFile(ctx, logger, outputPath, name, openapiYAML, isAppend)
	if err != nil {
		utils.LogError(logger, err, "failed to write OpenAPI YAML to a file", zap.String("outputPath", outputPath), zap.String("name", name))
		return err
	}

	outputFilePath := outputPath + "/" + name + ".yaml"
	logger.Info("OpenAPI YAML has been saved to " + outputFilePath)
	return nil
}
