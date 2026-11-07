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
	if err := schemaFolderInside(root); err != nil {
		utils.LogError(ts.logger, err, "failed to open the folder of test schemas")
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
			err = outOfSchemas(root, filepath.Join(TestPath, j.Name()), err)
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
	if err := schemaFolderInside(root); err != nil {
		utils.LogError(ts.logger, err, "failed to open the folder of mock schemas")
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
			err = outOfSchemas(root, mockPath, err)
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

// schemaFolderInside refuses root, a folder schemas are read in, when it leads
// out of the project: it is opened as utils.OpenRecordingDir opens it, so a
// keploy folder, a schema folder or a folder in it (./keploy/schema/tests,
// Download/Mocks/<service>) that links out of the folder it is in is refused
// with why, before anything in it is read. The schemas themselves are then
// read inside root (outOfSchemas). A root that is not there is no schemas,
// and is left to the readers to say so.
func schemaFolderInside(root string) error {
	r, err := utils.OpenRecordingDir(root)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("a schema is read only inside the project, and the folder %q leads out of it: %w", root, err)
	}
	return r.Close()
}

// outOfSchemas says how to have a schema read that was refused, err, because
// path, a file under root, the folder schemas are kept in, leads out of it:
// a schema is read only inside that folder, so a set kept elsewhere is read
// as a copy in it. Any other refusal or error is returned as it is.
func outOfSchemas(root, path string, err error) error {
	if !leadsOut(root, path) {
		return err
	}
	return fmt.Errorf("%w; a schema is read only inside its schema folder %q: copy the set into it", err, root)
}

// leadsOut reports whether path, followed through its links, is a file or a
// folder outside root, followed the same way. A path that leads nowhere has
// nothing to copy, and does not.
func leadsOut(root, path string) bool {
	p, err := resolved(path)
	if err != nil {
		return false
	}
	r, err := resolved(root)
	if err != nil {
		return false
	}
	rel, err := filepath.Rel(r, p)
	return err != nil || !filepath.IsLocal(rel)
}

// resolved is path made absolute and followed through its links.
func resolved(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(abs)
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
