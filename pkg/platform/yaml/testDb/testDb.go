package testdb

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"go.keploy.io/server/v2/pkg/models"
	"go.keploy.io/server/v2/pkg/platform"
	"go.keploy.io/server/v2/pkg/platform/telemetry"
	"go.keploy.io/server/v2/pkg/platform/yaml"
	"go.keploy.io/server/v2/pkg/service/record"
	"go.uber.org/zap"
)

type TestYaml struct {
	TcsPath     string
	TcsName     string
	Logger      *zap.Logger
	tele        *telemetry.Telemetry
	nameCounter int
	mutex       sync.RWMutex
}

func New(Logger *zap.Logger, tcsPath, TcsName string, tele telemetry.Telemetry) record.TestDB {
	return &TestYaml{
		TcsPath:     tcsPath,
		TcsName:     TcsName,
		Logger:      Logger,
		tele:        &tele,
		nameCounter: 0,
		mutex:       sync.RWMutex{},
	}
}

func (ts *TestYaml) InsertTestCase(ctx context.Context, tc *models.TestCase, testSetId string) error {
	if ts.tele != nil {
		ts.tele.RecordedTestAndMocks()
		ts.mutex.Lock()
		testsTotal, ok := ctx.Value("testsTotal").(*int)
		if !ok {
			ts.Logger.Debug("failed to get testsTotal from context")
		} else {
			*testsTotal++
		}
		ts.mutex.Unlock()
	}
	tcsPath := filepath.Join(ts.TcsPath, testSetId)

	var tcsName string
	if ts.TcsName == "" {
		if tc.Name == "" {
			// finds the recently generated testcase to derive the sequence number for the current testcase
			lastIndx, err := yaml.FindLastIndex(tcsPath, ts.Logger)
			if err != nil {
				return err
			}
			tcsName = fmt.Sprintf("test-%v", lastIndx)
		} else {
			tcsName = tc.Name
		}
	} else {
		tcsName = ts.TcsName
	}

	// encode the testcase and its mocks into yaml docs
	yamlTc, err := EncodeTestcase(*tc, ts.Logger)
	if err != nil {
		return err
	}

	// write testcase yaml
	yamlTc.Name = tcsName
	err = yaml.Write(ctx, ts.Logger, tcsPath, tcsName, yamlTc)
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

func (ys *TestYaml) ReadTestcases(testSet string, lastSeenId platform.KindSpecifier, options platform.KindSpecifier) ([]platform.KindSpecifier, error) {
	path := ys.TcsPath + "/" + testSet + "/tests"
	tcs := []*models.TestCase{}

	mockPath, err := yaml.ValidatePath(path)
	if err != nil {
		return nil, err
	}
	_, err = os.Stat(mockPath)
	if err != nil {
		ys.Logger.Debug("no tests are recorded for the session", zap.String("index", testSet))
		tcsRead := make([]platform.KindSpecifier, len(tcs))
		return tcsRead, nil
	}

	dir, err := os.OpenFile(mockPath, os.O_RDONLY, os.ModePerm)
	if err != nil {
		ys.Logger.Error("failed to open the directory containing yaml testcases", zap.Error(err), zap.Any("path", mockPath))
		return nil, err
	}

	files, err := dir.ReadDir(0)
	if err != nil {
		ys.Logger.Error("failed to read the file names of yaml testcases", zap.Error(err), zap.Any("path", mockPath))
		return nil, err
	}
	for _, j := range files {
		if filepath.Ext(j.Name()) != ".yaml" || strings.Contains(j.Name(), "mocks") {
			continue
		}

		name := strings.TrimSuffix(j.Name(), filepath.Ext(j.Name()))
		yamlTestcase, err := yaml.Read(mockPath, name)
		if err != nil {
			ys.Logger.Error("failed to read the testcase from yaml", zap.Error(err))
			return nil, err
		}
		// Unmarshal the yaml doc into Testcase
		tc, err := Decode(yamlTestcase[0], ys.Logger)
		if err != nil {
			return nil, err
		}
		// Append the encoded testcase
		tcs = append(tcs, tc)
	}

	sort.SliceStable(tcs, func(i, j int) bool {
		return tcs[i].HttpReq.Timestamp.Before(tcs[j].HttpReq.Timestamp)
	})
	tcsRead := make([]platform.KindSpecifier, len(tcs))
	for i, tc := range tcs {
		tcsRead[i] = tc
	}
	return tcsRead, nil
}
