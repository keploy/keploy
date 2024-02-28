package testdb

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"go.keploy.io/server/v2/pkg/models"
	"go.keploy.io/server/v2/pkg/platform/yaml"
	"go.uber.org/zap"
	yamlLib "gopkg.in/yaml.v3"
)

type TestYaml struct {
	TcsPath string
	Logger  *zap.Logger
}

func New(Logger *zap.Logger, tcsPath string) *TestYaml {
	return &TestYaml{
		TcsPath: tcsPath,
		Logger:  Logger,
	}
}

func (ts *TestYaml) InsertTestCase(ctx context.Context, tc *models.TestCase, testSetId string) error {
	tcsPath := filepath.Join(ts.TcsPath, testSetId, "tests")
	var tcsName string
	if tc.Name == "" {
		lastIndx, err := yaml.FindLastIndex(tcsPath, ts.Logger)
		if err != nil {
			return err
		}
		tcsName = fmt.Sprintf("test-%v", lastIndx)
	} else {
		tcsName = tc.Name
	}
	yamlTc, err := EncodeTestcase(*tc, ts.Logger)
	if err != nil {
		return err
	}
	yamlTc.Name = tcsName
	data, err := yamlLib.Marshal(&yamlTc)
	if err != nil {
		return err
	}
	err = yaml.WriteFile(ctx, ts.Logger, tcsPath, tcsName, data)
	if err != nil {
		ts.Logger.Error("failed to write testcase yaml file", zap.Error(err))
		return err
	}
	ts.Logger.Info("🟠 Keploy has captured test cases for the user's application.", zap.String("path", tcsPath), zap.String("testcase name", tcsName))
	return nil
}

func (ts *TestYaml) GetAllTestSetIds(ctx context.Context) ([]string, error) {
	return yaml.ReadSessionIndices(ts.TcsPath, ts.Logger)
}

func (ts *TestYaml) GetTestCases(ctx context.Context, testSetId string) ([]*models.TestCase, error) {
	path := filepath.Join(ts.TcsPath, testSetId, "tests")
	tcs := []*models.TestCase{}
	TestPath, err := yaml.ValidatePath(path)
	if err != nil {
		return nil, err
	}
	_, err = os.Stat(TestPath)
	if err != nil {
		ts.Logger.Debug("no tests are recorded for the session", zap.String("index", testSetId))
		return nil, nil
	}
	dir, err := yaml.ReadDir(TestPath, os.ModePerm)
	if err != nil {
		ts.Logger.Error("failed to open the directory containing yaml testcases", zap.Error(err), zap.Any("path", TestPath))
		return nil, err
	}
	files, err := dir.ReadDir(0)
	if err != nil {
		ts.Logger.Error("failed to read the file names of yaml testcases", zap.Error(err), zap.Any("path", TestPath))
		return nil, err
	}
	for _, j := range files {
		if filepath.Ext(j.Name()) != ".yaml" || strings.Contains(j.Name(), "mocks") {
			continue
		}

		name := strings.TrimSuffix(j.Name(), filepath.Ext(j.Name()))
		data, err := yaml.ReadFile(TestPath, name)

		var testCase *yaml.NetworkTrafficDoc
		err = yamlLib.Unmarshal(data, &testCase)
		if err != nil {
			ts.Logger.Error("failed to unmarshall YAML data", zap.Error(err))
			return nil, err
		}

		if err != nil {
			ts.Logger.Error("failed to read the testcase from yaml", zap.Error(err))
			return nil, err
		}
		tc, err := Decode(testCase, ts.Logger)
		if err != nil {
			return nil, err
		}
		tcs = append(tcs, tc)
	}
	sort.SliceStable(tcs, func(i, j int) bool {
		return tcs[i].HttpReq.Timestamp.Before(tcs[j].HttpReq.Timestamp)
	})
	return tcs, nil
}
