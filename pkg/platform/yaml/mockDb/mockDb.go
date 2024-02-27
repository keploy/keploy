package mockdb

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"go.keploy.io/server/v2/pkg/models"
	"go.keploy.io/server/v2/pkg/platform/telemetry"
	"go.keploy.io/server/v2/pkg/platform/yaml"
	"go.uber.org/zap"
)

type MockYaml struct {
	MockPath    string
	MockName    string
	Logger      *zap.Logger
	tele        *telemetry.Telemetry
	nameCounter int
	mutex       sync.RWMutex
}

func New(Logger *zap.Logger, tele telemetry.Telemetry, mockPath string, mockName string) *MockYaml {
	return &MockYaml{
		MockPath:    mockPath,
		MockName:    mockName,
		Logger:      Logger,
		mutex:       sync.RWMutex{},
		nameCounter: 0,
		tele:        &tele,
	}
}

func (ys *MockYaml) InsertMock(ctx context.Context, mock *models.Mock, testSetId string) error {
	mocksTotal, ok := ctx.Value("mocksTotal").(*map[string]int)
	if !ok {
		ys.Logger.Debug("failed to get mocksTotal from context")
	}
	(*mocksTotal)[string(mock.Kind)]++
	if ctx.Value("cli") == "mockrecord" {
		if ys.tele != nil {
			ys.tele.RecordedMock(string(mock.Kind))
		}
	}
	if ys.MockName != "" {
		mock.Name = ys.MockName
	}

	mock.Name = fmt.Sprint("mock-", yaml.GetNextID())
	mockYaml, err := EncodeMock(mock, ys.Logger)
	if err != nil {
		return err
	}

	ys.MockPath = filepath.Join(ys.MockPath, testSetId, "tests")

	err = yaml.Write(ctx, ys.Logger, ys.MockPath, "mocks", mockYaml)
	if err != nil {
		return err
	}

	return nil
}

func (ys *MockYaml) GetFilteredMocks(ctx context.Context, testSetId string, afterTime time.Time, beforeTime time.Time) ([]*models.Mock, error) {
	var (
		tcsMocks = make([]*models.Mock, 0)
	)

	mockName := "mocks"
	if ys.MockName != "" {
		mockName = ys.MockName
	}

	path := ys.MockPath + "/" + testSetId
	mockPath, err := yaml.ValidatePath(path + "/" + mockName + ".yaml")
	if err != nil {
		return nil, err
	}

	if _, err := os.Stat(mockPath); err == nil {

		yamls, err := yaml.Read(path, mockName)
		if err != nil {
			ys.Logger.Error("failed to read the mocks from config yaml", zap.Error(err), zap.Any("session", filepath.Base(path)))
			return nil, err
		}
		mocks, err := decodeMocks(yamls, ys.Logger)
		if err != nil {
			ys.Logger.Error("failed to decode the config mocks from yaml docs", zap.Error(err), zap.Any("session", filepath.Base(path)))
			return nil, err
		}

		for _, mock := range mocks {
			if mock.Spec.Metadata["type"] != "config" && mock.Kind != "Generic" {
				tcsMocks = append(tcsMocks, mock)
			}
			//if postgres type confgi
		}
	}
	filteredTcsMocks := make([]*models.Mock, 0)

	if afterTime == (time.Time{}) {
		ys.Logger.Warn("request timestamp is missing for " + ys.MockName)
		return tcsMocks, nil
	}

	if beforeTime == (time.Time{}) {
		ys.Logger.Warn("response timestamp is missing for " + ys.MockName)
		return tcsMocks, nil
	}
	var entMocks, nonKeployMocks []string
	for _, mock := range tcsMocks {
		if mock.Version == "api.keploy-enterprise.io/v1beta1" {
			entMocks = append(entMocks, mock.Name)
		} else if mock.Version != "api.keploy.io/v1beta1" && mock.Version != "api.keploy.io/v1beta2" {
			nonKeployMocks = append(nonKeployMocks, mock.Name)
		}
		if (mock.Spec.ReqTimestampMock == (time.Time{}) || mock.Spec.ResTimestampMock == (time.Time{})) && mock.Kind != "SQL" {
			// If mock doesn't have either of one timestamp, then, logging a warning msg and appending the mock to filteredMocks to support backward compatibility.
			ys.Logger.Warn("request or response timestamp of mock is missing ")
			filteredTcsMocks = append(filteredTcsMocks, mock)
			continue
		}

		// Checking if the mock's request and response timestamps lie between the test's request and response timestamp
		if mock.Spec.ReqTimestampMock.After(afterTime) && mock.Spec.ResTimestampMock.Before(beforeTime) {
			filteredTcsMocks = append(filteredTcsMocks, mock)
		}
	}
	if len(entMocks) > 0 {
		ys.Logger.Warn("These mocks have been recorded with Keploy Enterprise, may not work properly with the open-source version", zap.Strings("enterprise mocks:", entMocks))
	}
	if len(nonKeployMocks) > 0 {
		ys.Logger.Warn("These mocks have not been recorded by Keploy, may not work properly with Keploy.", zap.Strings("non-keploy mocks:", nonKeployMocks))
	}
	filteredMocks := filterMocks(ctx, tcsMocks, afterTime, beforeTime, ys.Logger)
	// Sort the filtered mocks based on the request timestamp
	sort.SliceStable(filteredMocks, func(i, j int) bool {
		return filteredMocks[i].Spec.ReqTimestampMock.Before(filteredMocks[j].Spec.ReqTimestampMock)
	})

	// logger.Info("sorted mocks after sorting accornding to the testcase timestamps", zap.Any("testcase", tc.Name), zap.Any("mocks", sortedMocks))
	for _, v := range filteredMocks {
		ys.Logger.Debug("sorted mocks", zap.Any("testcase", ys.MockName), zap.Any("mocks", v))
	}

	return tcsMocks, nil

}

func (ys *MockYaml) GetUnFilteredMocks(ctx context.Context, testSetId string, afterTime time.Time, beforeTime time.Time) ([]*models.Mock, error) {
	var (
		configMocks = make([]*models.Mock, 0)
	)

	mockName := "mocks"
	if ys.MockName != "" {
		mockName = ys.MockName
	}
	path := ys.MockPath + "/" + testSetId

	mockPath, err := yaml.ValidatePath(path + "/" + mockName + ".yaml")
	if err != nil {
		return nil, err
	}
	if _, err := os.Stat(mockPath); err == nil {

		yamls, err := yaml.Read(path, mockName)
		if err != nil {
			ys.Logger.Error("failed to read the mocks from config yaml", zap.Error(err), zap.Any("session", filepath.Base(path)))
			return nil, err
		}
		mocks, err := decodeMocks(yamls, ys.Logger)
		if err != nil {
			ys.Logger.Error("failed to decode the config mocks from yaml docs", zap.Error(err), zap.Any("session", filepath.Base(path)))
			return nil, err
		}

		for _, mock := range mocks {
			if mock.Spec.Metadata["type"] == "config" || mock.Kind == "Postgres" || mock.Kind == "Generic" {
				configMocks = append(configMocks, mock)
			}
		}
	}

	filteredMocks := filterMocks(ctx, configMocks, afterTime, beforeTime, ys.Logger)
	// Sort the filtered mocks based on the request timestamp
	sort.SliceStable(filteredMocks, func(i, j int) bool {
		return filteredMocks[i].Spec.ReqTimestampMock.Before(filteredMocks[j].Spec.ReqTimestampMock)
	})

	for _, v := range filteredMocks {
		ys.Logger.Debug("sorted mocks", zap.Any("testcase", ys.MockName), zap.Any("mocks", v))
	}
	return filteredMocks, nil
}

// Filter the mocks based on req and res timestamp of test
