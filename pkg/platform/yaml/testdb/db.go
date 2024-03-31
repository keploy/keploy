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

func (ts *TestYaml) InsertTestCase(ctx context.Context, tc *models.TestCase, testSetID string) error {
	tcsPath := filepath.Join(ts.TcsPath, testSetID, "tests")
	var tcsName string
	if tc.Name == "" {
		lastIndx, err := yaml.FindLastIndex(tcsPath, ts.logger)
		if err != nil {
			return err
		}
		tcsName = fmt.Sprintf("test-%v", lastIndx)
	} else {
		tcsName = tc.Name
	}
	yamlTc, err := EncodeTestcase(*tc, ts.logger)
	if err != nil {
		return err
	}
	yamlTc.Name = tcsName
	data, err := yamlLib.Marshal(&yamlTc)
	if err != nil {
		return err
	}
	err = yaml.WriteFile(ctx, ts.logger, tcsPath, tcsName, data, false)
	if err != nil {
		utils.LogError(ts.logger, err, "failed to write testcase yaml file")
		return err
	}
	ts.logger.Info("🟠 Keploy has captured test cases for the user's application.", zap.String("path", tcsPath), zap.String("testcase name", tcsName))
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
	tests, err := dir.ReadDir(0)
	if err != nil {
		utils.LogError(ts.logger, err, "failed to read the file names of yaml testcases", zap.Any("path", TestPath))
		return nil, err
	}
	for _, testcase := range tests {
		if filepath.Ext(testcase.Name()) != ".yaml" || strings.Contains(testcase.Name(), "mocks") {
			continue
		}

		name := strings.TrimSuffix(testcase.Name(), filepath.Ext(testcase.Name()))
		data, err := yaml.ReadFile(ctx, ts.logger, TestPath, name)
		if err != nil {
			utils.LogError(ts.logger, err, "failed to read the testcase from yaml")
			return nil, err
		}

		if len(data) == 0 {
			utils.LogError(ts.logger, err, "failed to run the testcase case: testcase is empty. continuing execution")
			continue
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
