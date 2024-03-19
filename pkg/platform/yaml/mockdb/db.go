// Package mockdb provides a mock database implementation.
package mockdb

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"sync/atomic"
	"time"

	"go.keploy.io/server/v2/pkg/models"
	"go.keploy.io/server/v2/pkg/platform/yaml"
	"go.keploy.io/server/v2/utils"
	"go.uber.org/zap"
	yamlLib "gopkg.in/yaml.v3"
)

type MockYaml struct {
	MockPath  string
	MockName  string
	Logger    *zap.Logger
	idCounter int64
}

func New(Logger *zap.Logger, mockPath string, mockName string) *MockYaml {
	return &MockYaml{
		MockPath:  mockPath,
		MockName:  mockName,
		Logger:    Logger,
		idCounter: -1,
	}
}

func (ys *MockYaml) InsertMock(ctx context.Context, mock *models.Mock, testSetID string) error {
	mock.Name = fmt.Sprint("mock-", ys.getNextID())
	mockYaml, err := EncodeMock(mock, ys.Logger)
	if err != nil {
		return err
	}
	mockPath := filepath.Join(ys.MockPath, testSetID)
	mockFileName := ys.MockName
	if mockFileName == "" {
		mockFileName = "mocks"
	}
	data, err := yamlLib.Marshal(&mockYaml)
	if err != nil {
		return err
	}
	err = yaml.WriteFile(ctx, ys.Logger, mockPath, mockFileName, data, true)
	if err != nil {
		return err
	}
	return nil
}

func (ys *MockYaml) GetFilteredMocks(ctx context.Context, testSetID string, afterTime time.Time, beforeTime time.Time) ([]*models.Mock, error) {

	var tcsMocks = make([]*models.Mock, 0)
	var filteredTcsMocks = make([]*models.Mock, 0)

	mockFileName := "mocks"
	if ys.MockName != "" {
		mockFileName = ys.MockName
	}

	path := filepath.Join(ys.MockPath, testSetID)
	mockPath, err := yaml.ValidatePath(path + "/" + mockFileName + ".yaml")
	if err != nil {
		return nil, err
	}

	if _, err := os.Stat(mockPath); err == nil {
		var mockYamls []*yaml.NetworkTrafficDoc
		data, err := yaml.ReadFile(ctx, ys.Logger, path, mockFileName)
		if err != nil {
			utils.LogError(ys.Logger, err, "failed to read the mocks from config yaml", zap.Any("session", filepath.Base(path)))
			return nil, err
		}
		dec := yamlLib.NewDecoder(bytes.NewReader(data))
		for {
			var doc *yaml.NetworkTrafficDoc
			err := dec.Decode(&doc)
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				return nil, fmt.Errorf("failed to decode the yaml file documents. error: %v", err.Error())
			}
			mockYamls = append(mockYamls, doc)
		}
		mocks, err := decodeMocks(mockYamls, ys.Logger)
		if err != nil {
			utils.LogError(ys.Logger, err, "failed to decode the config mocks from yaml docs", zap.Any("session", filepath.Base(path)))
			return nil, err
		}

		for _, mock := range mocks {
			if mock.Spec.Metadata["type"] != "config" && mock.Kind != "Generic" && mock.Kind != "Postgres" { //&& mock.Kind == "PostgresV2" {
				tcsMocks = append(tcsMocks, mock)
			}
		}
	}

	filteredTcsMocks, _ = ys.filterByTimeStamp(ctx, tcsMocks, afterTime, beforeTime, ys.Logger)

	sort.SliceStable(filteredTcsMocks, func(i, j int) bool {
		return filteredTcsMocks[i].Spec.ReqTimestampMock.Before(filteredTcsMocks[j].Spec.ReqTimestampMock)
	})

	return filteredTcsMocks, nil
}

func (ys *MockYaml) GetUnFilteredMocks(ctx context.Context, testSetID string, afterTime time.Time, beforeTime time.Time) ([]*models.Mock, error) {

	var configMocks = make([]*models.Mock, 0)

	mockName := "mocks"
	if ys.MockName != "" {
		mockName = ys.MockName
	}

	path := filepath.Join(ys.MockPath, testSetID)

	mockPath, err := yaml.ValidatePath(path + "/" + mockName + ".yaml")
	if err != nil {
		return nil, err
	}

	if _, err := os.Stat(mockPath); err == nil {
		var mockYamls []*yaml.NetworkTrafficDoc
		data, err := yaml.ReadFile(ctx, ys.Logger, path, mockName)
		if err != nil {
			utils.LogError(ys.Logger, err, "failed to read the mocks from config yaml", zap.Any("session", filepath.Base(path)))
			return nil, err
		}
		dec := yamlLib.NewDecoder(bytes.NewReader(data))
		for {
			var doc *yaml.NetworkTrafficDoc
			err := dec.Decode(&doc)
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				return nil, fmt.Errorf("failed to decode the yaml file documents. error: %v", err.Error())
			}
			mockYamls = append(mockYamls, doc)
		}
		mocks, err := decodeMocks(mockYamls, ys.Logger)
		if err != nil {
			utils.LogError(ys.Logger, err, "failed to decode the config mocks from yaml docs", zap.Any("session", filepath.Base(path)))
			return nil, err
		}
		for _, mock := range mocks {
			if mock.Spec.Metadata["type"] == "config" || mock.Kind == "Postgres" || mock.Kind == "Generic" {
				configMocks = append(configMocks, mock)
			}
		}
	}

	filteredMocks, unfilteredMocks := ys.filterByTimeStamp(ctx, configMocks, afterTime, beforeTime, ys.Logger)

	sort.SliceStable(filteredMocks, func(i, j int) bool {
		return filteredMocks[i].Spec.ReqTimestampMock.Before(filteredMocks[j].Spec.ReqTimestampMock)
	})

	sort.SliceStable(unfilteredMocks, func(i, j int) bool {
		return unfilteredMocks[i].Spec.ReqTimestampMock.Before(unfilteredMocks[j].Spec.ReqTimestampMock)
	})

	// if len(unfilteredMocks) > 10 {
	// 	unfilteredMocks = unfilteredMocks[:10]
	// }
	mocks := append(filteredMocks, unfilteredMocks...)

	return mocks, nil
}

func (ys *MockYaml) getNextID() int64 {
	return atomic.AddInt64(&ys.idCounter, 1)
}

func (ys *MockYaml) filterByTimeStamp(_ context.Context, m []*models.Mock, afterTime time.Time, beforeTime time.Time, logger *zap.Logger) ([]*models.Mock, []*models.Mock) {

	filteredMocks := make([]*models.Mock, 0)
	unfilteredMocks := make([]*models.Mock, 0)

	if afterTime == (time.Time{}) {
		return m, unfilteredMocks
	}

	if beforeTime == (time.Time{}) {
		return m, unfilteredMocks
	}

	isNonKeploy := false

	for _, mock := range m {
		if mock.Version != "api.keploy.io/v1beta1" && mock.Version != "api.keploy.io/v1beta2" {
			isNonKeploy = true
			continue
		}
		if mock.Spec.ReqTimestampMock == (time.Time{}) || mock.Spec.ResTimestampMock == (time.Time{}) {
			logger.Debug("request or response timestamp of mock is missing")
			mock.TestModeInfo.IsFiltered = true
			filteredMocks = append(filteredMocks, mock)
			continue
		}

		if mock.Spec.ReqTimestampMock.After(afterTime) && mock.Spec.ResTimestampMock.Before(beforeTime) {
			mock.TestModeInfo.IsFiltered = true
			filteredMocks = append(filteredMocks, mock)
			continue
		}
		mock.TestModeInfo.IsFiltered = false
		unfilteredMocks = append(unfilteredMocks, mock)
	}
	if isNonKeploy {
		ys.Logger.Warn("Few mocks in the mock File are not recorded by keploy ignoring them")
	}
	return filteredMocks, unfilteredMocks
}
