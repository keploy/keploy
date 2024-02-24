package mockstore

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
	"go.keploy.io/server/v2/pkg/service/record"
	"go.keploy.io/server/v2/pkg/service/replay"
	"go.uber.org/zap"
)

type RecordMockYaml struct {
	MockPath    string
	MockName    string
	Logger      *zap.Logger
	tele        *telemetry.Telemetry
	nameCounter int
	mutex       sync.RWMutex
}

type ReplayMockYaml struct {
	MockPath    string
	MockName    string
	Logger      *zap.Logger
	tele        *telemetry.Telemetry
	nameCounter int
	mutex       sync.RWMutex
}

func NewYamlStore(tcsPath string, mockPath string, tcsName string, mockName string, Logger *zap.Logger, tele telemetry.Telemetry) record.MockDB {
	return &RecordMockYaml{
		MockPath:    mockPath,
		MockName:    mockName,
		Logger:      Logger,
		tele:        &tele,
		nameCounter: 0,
		mutex:       sync.RWMutex{},
	}
}

func NewReplayYamlStore(mockPath string, Logger *zap.Logger) replay.MockDB {
	return &ReplayMockYaml{
		MockPath: mockPath,
		Logger:   Logger,
	}
}

func (ys *RecordMockYaml) InsertMock(ctx context.Context, mock *models.Mock, testSetId string) error {
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

func (ys *ReplayMockYaml) GetFilteredMocks(ctx context.Context, testSetId string, afterTime time.Time, beforeTime time.Time) ([]*models.Mock, error) {
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
	filteredMocks, unFilteredMocks := FilterMocks(ctx, tcsMocks, afterTime, beforeTime, ys.Logger)
	// Sort the filtered mocks based on the request timestamp
	sort.SliceStable(filteredMocks, func(i, j int) bool {
		return filteredMocks[i].Spec.ReqTimestampMock.Before(filteredMocks[j].Spec.ReqTimestampMock)
	})

	// Sort the unfiltered mocks based on some criteria (modify as needed)
	sort.SliceStable(unFilteredMocks, func(i, j int) bool {
		return unFilteredMocks[i].Spec.ReqTimestampMock.Before(unFilteredMocks[j].Spec.ReqTimestampMock)
	})

	// select first 10 mocks from the unfiltered mocks
	if len(unFilteredMocks) > 10 {
		unFilteredMocks = unFilteredMocks[:10]
	}

	// Append the unfiltered mocks to the filtered mocks
	sortedMocks := append(filteredMocks, unFilteredMocks...)
	// logger.Info("sorted mocks after sorting accornding to the testcase timestamps", zap.Any("testcase", tc.Name), zap.Any("mocks", sortedMocks))
	for _, v := range sortedMocks {
		ys.Logger.Debug("sorted mocks", zap.Any("testcase", ys.MockName), zap.Any("mocks", v))
	}

	return tcsMocks, nil

}

func (ys *ReplayMockYaml) GetUnFilteredMocks(ctx context.Context, testSetId string, afterTime time.Time, beforeTime time.Time) ([]*models.Mock, error) {
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
	filteredMocks, unFilteredMocks := FilterMocks(ctx, configMocks, afterTime, beforeTime, ys.Logger)
	// Sort the filtered mocks based on the request timestamp
	sort.SliceStable(filteredMocks, func(i, j int) bool {
		return filteredMocks[i].Spec.ReqTimestampMock.Before(filteredMocks[j].Spec.ReqTimestampMock)
	})

	// Sort the unfiltered mocks based on some criteria (modify as needed)
	sort.SliceStable(unFilteredMocks, func(i, j int) bool {
		return unFilteredMocks[i].Spec.ReqTimestampMock.Before(unFilteredMocks[j].Spec.ReqTimestampMock)
	})

	// select first 10 mocks from the unfiltered mocks
	if len(unFilteredMocks) > 10 {
		unFilteredMocks = unFilteredMocks[:10]
	}

	// Append the unfiltered mocks to the filtered mocks
	sortedMocks := append(filteredMocks, unFilteredMocks...)
	// logger.Info("sorted mocks after sorting accornding to the testcase timestamps", zap.Any("testcase", tc.Name), zap.Any("mocks", sortedMocks))
	for _, v := range sortedMocks {
		ys.Logger.Debug("sorted mocks", zap.Any("testcase", ys.MockName), zap.Any("mocks", v))
	}
	return sortedMocks, nil
}

// Filter the mocks based on req and res timestamp of test
func FilterMocks(ctx context.Context, m []*models.Mock, afterTime time.Time, beforeTime time.Time, logger *zap.Logger) ([]*models.Mock, []*models.Mock) {
	filteredMocks := make([]*models.Mock, 0)
	unFilteredMocks := make([]*models.Mock, 0)

	if afterTime == (time.Time{}) {
		logger.Warn("request timestamp is missing  ")
		return m, filteredMocks
	}

	if beforeTime == (time.Time{}) {
		logger.Warn("response timestamp is missing  ")
		return m, filteredMocks
	}
	for _, mock := range m {
		if mock.Spec.ReqTimestampMock == (time.Time{}) || mock.Spec.ResTimestampMock == (time.Time{}) {
			// If mock doesn't have either of one timestamp, then, logging a warning msg and appending the mock to filteredMocks to support backward compatibility.
			logger.Warn("request or response timestamp of mock is missing  ")
			mock.TestModeInfo.IsFiltered = true
			filteredMocks = append(filteredMocks, mock)
			continue
		}

		// Checking if the mock's request and response timestamps lie between the test's request and response timestamp
		if mock.Spec.ReqTimestampMock.After(afterTime) && mock.Spec.ResTimestampMock.Before(beforeTime) {
			mock.TestModeInfo.IsFiltered = true
			filteredMocks = append(filteredMocks, mock)
			continue
		}
		mock.TestModeInfo.IsFiltered = false
		unFilteredMocks = append(unFilteredMocks, mock)
	}
	logger.Debug("filtered mocks after filtering accornding to the testcase timestamps", zap.Any("mocks", filteredMocks))
	// TODO change this to debug
	logger.Debug("number of filtered mocks", zap.Any("number of filtered mocks", len(filteredMocks)))
	return filteredMocks, unFilteredMocks
}
