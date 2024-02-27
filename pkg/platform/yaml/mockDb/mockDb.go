package mockdb

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync/atomic"
	"time"

	"go.keploy.io/server/v2/pkg/models"
	"go.keploy.io/server/v2/pkg/platform/telemetry"
	"go.keploy.io/server/v2/pkg/platform/yaml"
	"go.uber.org/zap"
)

type MockYaml struct {
	MockPath  string
	MockName  string
	Logger    *zap.Logger
	idCounter int64
}

func New(Logger *zap.Logger, tele telemetry.Telemetry, mockPath string, mockName string) *MockYaml {
	return &MockYaml{
		MockPath:  mockPath,
		MockName:  mockName,
		Logger:    Logger,
		idCounter: -1,
	}
}

func (ys *MockYaml) InsertMock(ctx context.Context, mock *models.Mock, testSetId string) error {
	mock.Name = fmt.Sprint("mock-", ys.getNextID())
	mockYaml, err := EncodeMock(mock, ys.Logger)
	if err != nil {
		return err
	}
	mockPath := filepath.Join(ys.MockPath, testSetId)
	mockFileName := ys.MockName
	if mockFileName == "" {
		mockFileName = "mocks"
	}
	err = yaml.Write(ctx, ys.Logger, mockPath, mockFileName, mockYaml)
	if err != nil {
		return err
	}
	return nil
}

func (ys *MockYaml) GetFilteredMocks(ctx context.Context, testSetId string, afterTime time.Time, beforeTime time.Time) ([]*models.Mock, error) {

	var tcsMocks = make([]*models.Mock, 0)
	var filteredTcsMocks = make([]*models.Mock, 0)

	mockFileName := "mocks"
	if ys.MockName != "" {
		mockFileName = ys.MockName
	}

	path := filepath.Join(ys.MockPath, testSetId)
	mockPath, err := yaml.ValidatePath(path + "/" + mockFileName + ".yaml")
	if err != nil {
		return nil, err
	}

	if _, err := os.Stat(mockPath); err == nil {

		yamls, err := yaml.Read(path, mockFileName)
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
			if mock.Spec.Metadata["type"] != "config" && mock.Kind != "Generic" && mock.Kind != "Postgres" {
				tcsMocks = append(tcsMocks, mock)
			}
		}
	}

	if afterTime == (time.Time{}) {
		ys.Logger.Warn("request timestamp is missing for mock filtering")
		return tcsMocks, nil
	}

	if beforeTime == (time.Time{}) {
		ys.Logger.Warn("response timestamp is missing for mock filtering")
		return tcsMocks, nil
	}

	isNonKeploy := false
	for _, mock := range tcsMocks {
		if mock.Version != "api.keploy.io/v1beta1" && mock.Version != "api.keploy.io/v1beta2" {
			isNonKeploy = true
			continue
		}
		if mock.Spec.ReqTimestampMock == (time.Time{}) || mock.Spec.ResTimestampMock == (time.Time{}) {
			ys.Logger.Warn("request or response timestamp of mock is missing ")
			filteredTcsMocks = append(filteredTcsMocks, mock)
			continue
		}
		if mock.Spec.ReqTimestampMock.After(afterTime) && mock.Spec.ResTimestampMock.Before(beforeTime) {
			filteredTcsMocks = append(filteredTcsMocks, mock)
		}
	}
	if isNonKeploy {
		ys.Logger.Warn("Few mocks in the mock File are not recorded by keploy ignoring them")
	}

	sort.SliceStable(filteredTcsMocks, func(i, j int) bool {
		return filteredTcsMocks[i].Spec.ReqTimestampMock.Before(filteredTcsMocks[j].Spec.ReqTimestampMock)
	})

	return tcsMocks, nil
}

func (ys *MockYaml) GetUnFilteredMocks(ctx context.Context, testSetId string, afterTime time.Time, beforeTime time.Time) ([]*models.Mock, error) {

	var configMocks = make([]*models.Mock, 0)

	mockName := "mocks"
	if ys.MockName != "" {
		mockName = ys.MockName
	}

	path := filepath.Join(ys.MockPath, testSetId)

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
	sort.SliceStable(filteredMocks, func(i, j int) bool {
		return filteredMocks[i].Spec.ReqTimestampMock.Before(filteredMocks[j].Spec.ReqTimestampMock)
	})
	
	return filteredMocks, nil
}

func (ys *MockYaml) getNextID() int64 {
	return atomic.AddInt64(&ys.idCounter, 1)
}
