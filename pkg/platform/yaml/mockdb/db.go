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
	"sync/atomic"
	"time"
    "strings"

	"go.keploy.io/server/v3/pkg"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/platform/yaml"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
	yamlLib "gopkg.in/yaml.v3"
)

type MockYaml struct {
	MockPath  string
	MockName  string
	Logger    *zap.Logger
	idCounter int64
	expiryDuration time.Duration
}

func New(Logger *zap.Logger, mockPath string, mockName string) *MockYaml {
	ys := &MockYaml{
		MockPath:       mockPath,
		MockName:       mockName,
		Logger:         Logger,
		idCounter:      -1,
		expiryDuration: 18 * time.Hour,
	}
	// start background cleanup of expired mocks
	go ys.startExpiredMockCleanup()
	return ys
}

// startExpiredMockCleanup runs a background goroutine that periodically
// scans the expired area for mock directories that have passed their
// expiry timestamp and removes them permanently.
func (ys *MockYaml) startExpiredMockCleanup() {
	ticker := time.NewTicker(time.Hour)
	// run cleanup once immediately
	ys.cleanupExpiredMocks()
	for range ticker.C {
		ys.cleanupExpiredMocks()
	}
}

func (ys *MockYaml) cleanupExpiredMocks() {
	base := ys.MockPath
	now := time.Now()
	expiredBase := filepath.Join(base, "expired")
	edirs, err := os.ReadDir(expiredBase)
	if err != nil {
		// nothing to cleanup or directory missing
		return
	}
	for _, f := range edirs {
		if !f.IsDir() {
			// look for .expiry files next to directories
			name := f.Name()
			if strings.HasSuffix(name, ".expiry") {
				baseName := strings.TrimSuffix(name, ".expiry")
				metaPath := filepath.Join(expiredBase, name)
				data, err := os.ReadFile(metaPath)
				if err != nil {
					ys.Logger.Debug("failed to read expiry meta, will fallback to file modtime", zap.Error(err), zap.String("meta", metaPath))
					continue
				}
				expTime, err := time.Parse(time.RFC3339, strings.TrimSpace(string(data)))
				if err != nil {
					ys.Logger.Debug("invalid expiry timestamp, skipping", zap.Error(err), zap.String("meta", metaPath))
					continue
				}
				if now.After(expTime) {
					// remove the directory and meta (if it exists)
					dirPath := filepath.Join(expiredBase, baseName)
					_ = os.RemoveAll(dirPath)
					_ = os.Remove(metaPath)
					ys.Logger.Info("removed expired mocks", zap.String("dir", dirPath))
				}
			}
			continue
		}
	}
}

// UpdateMocks deletes the mocks from the mock file with given names
//
// mockNames is a map which contains the name of the mocks as key and a isConfig boolean as value
func (ys *MockYaml) UpdateMocks(ctx context.Context, testSetID string, mockNames map[string]models.MockState) error {
	// no-op: we operate on the entire testSet mocks directory
	ys.Logger.Debug("logging the names of the unused mocks to be removed", zap.Any("mockNames", mockNames), zap.String("for testset", testSetID), zap.String("at_path", filepath.Join(path, mockFileName+".yaml")))

	// Read the mocks from the yaml file
	mockPath, err := yaml.ValidatePath(filepath.Join(path, mockFileName+".yaml"))
	if err != nil {
		utils.LogError(ys.Logger, err, "failed to read mocks due to inaccessible path", zap.String("at_path", filepath.Join(path, mockFileName+".yaml")))
		return err
	}
	if _, err := os.Stat(mockPath); err != nil {
		utils.LogError(ys.Logger, err, "failed to find the mocks yaml file")
		return err
	}
	data, err := yaml.ReadFile(ctx, ys.Logger, path, mockFileName)
	if err != nil {
		utils.LogError(ys.Logger, err, "failed to read the mocks from yaml file", zap.String("at_path", filepath.Join(path, mockFileName+".yaml")))
		return err
	}

	// decode the mocks read from the yaml file
	dec := yamlLib.NewDecoder(bytes.NewReader(data))
	var mockYamls []*yaml.NetworkTrafficDoc
	for {
		var doc *yaml.NetworkTrafficDoc
		err := dec.Decode(&doc)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			utils.LogError(ys.Logger, err, "failed to decode the yaml file documents", zap.String("at_path", filepath.Join(path, mockFileName+".yaml")))
			return fmt.Errorf("failed to decode the yaml file documents. error: %v", err.Error())
		}
		mockYamls = append(mockYamls, doc)
	}
	mocks, err := decodeMocks(mockYamls, ys.Logger)
	if err != nil {
		return err
	}
	var newMocks []*models.Mock
	for _, mock := range mocks {
		if _, ok := mockNames[mock.Name]; ok {
			newMocks = append(newMocks, mock)
			continue
		}
	}
	ys.Logger.Debug("logging the names of the used mocks", zap.Any("mockNames", newMocks), zap.String("for testset", testSetID))

	// remove the old mock yaml file
	err = os.Remove(filepath.Join(path, mockFileName+".yaml"))
	if err != nil {
		return err
	}

	// write the new mocks to the new yaml file
	for _, newMock := range newMocks {
		mockYaml, err := EncodeMock(newMock, ys.Logger)
		if err != nil {
			utils.LogError(ys.Logger, err, "failed to encode the mock to yaml", zap.String("mock", newMock.Name), zap.String("for testset", testSetID))
			return err
		}
		data, err = yamlLib.Marshal(&mockYaml)
		if err != nil {
			utils.LogError(ys.Logger, err, "failed to marshal the mock to yaml", zap.String("mock", newMock.Name), zap.String("for testset", testSetID))
			return err
		}
		err = yaml.WriteFile(ctx, ys.Logger, path, mockFileName, data, true)
		if err != nil {
			utils.LogError(ys.Logger, err, "failed to write the mock to yaml", zap.String("mock", newMock.Name), zap.String("for testset", testSetID))
			return err
		}
	}
	return nil
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

	exists, err := yaml.FileExists(ctx, ys.Logger, mockPath, mockFileName)
	if err != nil {
		utils.LogError(ys.Logger, err, "failed to find yaml file", zap.String("path directory", mockPath), zap.String("yaml", mockFileName))
		return err
	}

	if !exists {
		data = append([]byte(utils.GetVersionAsComment()), data...)
	}

	err = yaml.WriteFile(ctx, ys.Logger, mockPath, mockFileName, data, true)
	if err != nil {
		return err
	}
	return nil
}
func (ys *MockYaml) GetFilteredMocks(ctx context.Context, testSetID string, afterTime time.Time, beforeTime time.Time) ([]*models.Mock, error) {

	var tcsMocks = make([]*models.Mock, 0)
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
		data, err := yaml.ReadFile(ctx, ys.Logger, path, mockFileName)
		if err != nil {
			utils.LogError(ys.Logger, err, "failed to read the mocks from yaml file", zap.String("session", filepath.Base(path)), zap.String("path", mockPath))
			return nil, err
		}
		if len(data) == 0 {
			utils.LogError(ys.Logger, err, "failed to read the mocks from yaml file", zap.String("session", filepath.Base(path)), zap.String("path", mockPath))
			return nil, fmt.Errorf("failed to get mocks, empty file")
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

			// Decode each YAML document into models.Mock as it is read.
			mocks, err := decodeMocks([]*yaml.NetworkTrafficDoc{doc}, ys.Logger)
			if err != nil {
				utils.LogError(ys.Logger, err, "failed to decode the config mocks from yaml doc", zap.String("session", filepath.Base(path)))
				return nil, err
			}

			for _, mock := range mocks {
				isFilteredMock := true
				switch mock.Kind {
				case "Generic":
					isFilteredMock = false
				case "Postgres":
					isFilteredMock = false
				case "Http":
					isFilteredMock = false
				case "Redis":
					isFilteredMock = false
				case "MySQL":
					isFilteredMock = false
				}
				if mock.Spec.Metadata["type"] != "config" && isFilteredMock {
					tcsMocks = append(tcsMocks, mock)
				}
			}
		}
	}

	filtered := pkg.FilterTcsMocks(ctx, ys.Logger, tcsMocks, afterTime, beforeTime)
	return filtered, nil
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
		data, err := yaml.ReadFile(ctx, ys.Logger, path, mockName)
		if err != nil {
			utils.LogError(ys.Logger, err, "failed to read the mocks from config yaml", zap.String("session", filepath.Base(path)))
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

			// Decode each YAML document into models.Mock as it is read.
			mocks, err := decodeMocks([]*yaml.NetworkTrafficDoc{doc}, ys.Logger)
			if err != nil {
				utils.LogError(ys.Logger, err, "failed to decode the config mocks from yaml doc", zap.String("session", filepath.Base(path)))
				return nil, err
			}

			for _, mock := range mocks {
				isUnFilteredMock := false
				switch mock.Kind {
				case "Generic":
					isUnFilteredMock = true
				case "Postgres":
					isUnFilteredMock = true
				case "Http":
					isUnFilteredMock = true
				case "Redis":
					isUnFilteredMock = true
				case "MySQL":
					isUnFilteredMock = true
				}
				if mock.Spec.Metadata["type"] == "config" || isUnFilteredMock {
					configMocks = append(configMocks, mock)
				}
			}
		}
	}

	unfiltered := pkg.FilterConfigMocks(ctx, ys.Logger, configMocks, afterTime, beforeTime)

	return unfiltered, nil
}

func (ys *MockYaml) getNextID() int64 {
	return atomic.AddInt64(&ys.idCounter, 1)
}

func (ys *MockYaml) GetHTTPMocks(ctx context.Context, testSetID string, mockPath string, mockFileName string) ([]*models.HTTPDoc, error) {

	if ys.MockName != "" {
		ys.MockName = mockFileName
	}
	ys.MockPath = mockPath

	tcsMocks, err := ys.GetUnFilteredMocks(ctx, testSetID, time.Time{}, time.Time{})
	if err != nil {
		return nil, err
	}

	var httpMocks []*models.HTTPDoc
	for _, mock := range tcsMocks {
		if mock.Kind != "Http" {
			continue
		}
		var httpMock models.HTTPDoc
		httpMock.Kind = mock.GetKind()
		httpMock.Name = mock.Name
		httpMock.Spec.Request = *mock.Spec.HTTPReq
		httpMock.Spec.Response = *mock.Spec.HTTPResp
		httpMock.Spec.Metadata = mock.Spec.Metadata
		httpMock.Version = string(mock.Version)
		httpMocks = append(httpMocks, &httpMock)
	}

	return httpMocks, nil
}

func (ys *MockYaml) DeleteMocksForSet(ctx context.Context, testSetID string) error {
	mockFileName := "mocks"
	if ys.MockName != "" {
		mockFileName = ys.MockName
	}
	path := filepath.Join(ys.MockPath, testSetID)
	// Instead of deleting mocks immediately, move the testSet mocks directory
	// to an expired area and write an expiry timestamp. The background cleaner
	// will remove expired directories after ys.expiryDuration.
	src := filepath.Join(ys.MockPath, testSetID)
	expiredBase := filepath.Join(ys.MockPath, "expired")
	if err := os.MkdirAll(expiredBase, 0o755); err != nil {
		ys.Logger.Error("failed to create expired dir", zap.Error(err), zap.String("path", expiredBase))
		// fallback to direct remove
		if rmErr := os.RemoveAll(src); rmErr != nil {
			utils.LogError(ys.Logger, rmErr, "failed to delete old mocks (fallback)", zap.String("path", src))
			return rmErr
		}
		ys.Logger.Info("Successfully cleared old mocks for refresh (fallback delete).", zap.String("testSet", testSetID))
		return nil
	}

	dst := filepath.Join(expiredBase, testSetID+"-"+time.Now().UTC().Format("20060102150405"))
	if err := os.Rename(src, dst); err != nil {
		ys.Logger.Error("failed to move mocks to expired folder, attempting delete", zap.Error(err), zap.String("src", src), zap.String("dst", dst))
		if rmErr := os.RemoveAll(src); rmErr != nil {
			utils.LogError(ys.Logger, rmErr, "failed to delete old mocks after failed move", zap.String("path", src))
			return rmErr
		}
		ys.Logger.Info("Successfully cleared old mocks for refresh (deleted after move failure).", zap.String("testSet", testSetID))
		return nil
	}

	// write expiry metadata alongside the moved directory
	metaPath := dst + ".expiry"
	expiry := time.Now().Add(ys.expiryDuration)
	if err := os.WriteFile(metaPath, []byte(expiry.Format(time.RFC3339)), 0o644); err != nil {
		ys.Logger.Warn("failed to write expiry metadata for mocks", zap.Error(err), zap.String("meta", metaPath))
		// do not fail the whole operation for metadata write failure
	}

	ys.Logger.Info("expired mocks (moved) — will be removed after expiry", zap.String("mocksDir", dst), zap.Time("expiresAt", expiry))
	return nil
}

func (ys *MockYaml) GetCurrMockID() int64 {
	return atomic.LoadInt64(&ys.idCounter)
}

func (ys *MockYaml) ResetCounterID() {
	atomic.StoreInt64(&ys.idCounter, -1)
}
