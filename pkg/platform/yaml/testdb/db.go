// Package testdb provides functionality for working with test databases.
package testdb

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"go.keploy.io/server/v2/pkg/models"
	"go.keploy.io/server/v2/pkg/platform/yaml"
	"go.keploy.io/server/v2/utils"
	"go.uber.org/zap"
	yamlLib "gopkg.in/yaml.v3"
)

type TestYaml struct {
	TcsPath string
	logger  *zap.Logger
}

func New(logger *zap.Logger, tcsPath string) *TestYaml {
	return &TestYaml{
		TcsPath: tcsPath,
		logger:  logger,
	}
}

type tcsInfo struct {
	name string
	path string
}

func (ts *TestYaml) InsertTestCase(ctx context.Context, tc *models.TestCase, testSetID string) error {
	tcsInfo, err := ts.upsert(ctx, testSetID, tc)
	if err != nil {
		return err
	}

	ts.logger.Info("🟠 Keploy has captured test cases for the user's application.", zap.String("path", tcsInfo.path), zap.String("testcase name", tcsInfo.name))

	return nil
}

func (ts *TestYaml) GetAllTestSetIDs(ctx context.Context) ([]string, error) {
	return yaml.ReadSessionIndices(ctx, ts.TcsPath, ts.logger)
}

func (ts *TestYaml) GetTestCases(ctx context.Context, testSetID string) ([]*models.TestCase, error) {
	path := filepath.Join(ts.TcsPath, testSetID, "tests")
	tcs := []*models.TestCase{}
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
		if filepath.Ext(j.Name()) != ".yaml" || strings.Contains(j.Name(), "mocks") {
			continue
		}

		name := strings.TrimSuffix(j.Name(), filepath.Ext(j.Name()))
		data, err := yaml.ReadFile(ctx, ts.logger, TestPath, name)
		if err != nil {
			utils.LogError(ts.logger, err, "failed to read the testcase from yaml")
			return nil, err
		}

		var testCase *yaml.NetworkTrafficDoc
		err = yamlLib.Unmarshal(data, &testCase)
		if err != nil {
			utils.LogError(ts.logger, err, "failed to unmarshall YAML data")
			return nil, err
		}

		tc, err := Decode(testCase, ts.logger)
		if err != nil {
			utils.LogError(ts.logger, err, "failed to decode the testcase")
			return nil, err
		}
		tcs = append(tcs, tc)
	}
	sort.SliceStable(tcs, func(i, j int) bool {
		return tcs[i].HTTPReq.Timestamp.Before(tcs[j].HTTPReq.Timestamp)
	})
	return tcs, nil
}

func (ts *TestYaml) UpdateTestCase(ctx context.Context, tc *models.TestCase, testSetID string) error {

	tcsInfo, err := ts.upsert(ctx, testSetID, tc)
	if err != nil {
		return err
	}

	ts.logger.Info("🔄 Keploy has updated the test cases for the user's application.", zap.String("path", tcsInfo.path), zap.String("testcase name", tcsInfo.name))
	return nil
}

func (ts *TestYaml) upsert(ctx context.Context, testSetID string, tc *models.TestCase) (tcsInfo, error) {
	tcsPath := filepath.Join(ts.TcsPath, testSetID, "tests")
	var tcsName string
	if tc.Name == "" {
		lastIndx, err := yaml.FindLastIndex(tcsPath, ts.logger)
		if err != nil {
			return tcsInfo{name: "", path: tcsPath}, err
		}
		tcsName = fmt.Sprintf("test-%v", lastIndx)
	} else {
		tcsName = tc.Name
	}
	yamlTc, err := EncodeTestcase(*tc, ts.logger)
	if err != nil {
		return tcsInfo{name: tcsName, path: tcsPath}, err
	}
	yamlTc.Name = tcsName
	data, err := yamlLib.Marshal(&yamlTc)
	if err != nil {
		return tcsInfo{name: tcsName, path: tcsPath}, err
	}
	err = yaml.WriteFile(ctx, ts.logger, tcsPath, tcsName, data, false)
	if err != nil {
		utils.LogError(ts.logger, err, "failed to write testcase yaml file")
		return tcsInfo{name: tcsName, path: tcsPath}, err
	}

	return tcsInfo{name: tcsName, path: tcsPath}, nil
}
