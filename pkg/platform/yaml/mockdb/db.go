// Package mockdb provides a mock database implementation.
package mockdb

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

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
}

const mockFileLockStripeCount = 256

var mockFileLockStripes [mockFileLockStripeCount]sync.RWMutex

func New(Logger *zap.Logger, mockPath string, mockName string) *MockYaml {
	return &MockYaml{
		MockPath:  mockPath,
		MockName:  mockName,
		Logger:    Logger,
		idCounter: -1,
	}
}

func mockFileLockKey(path, fileName string) string {
	fullPath := filepath.Join(path, fileName+".yaml")
	if absPath, err := filepath.Abs(fullPath); err == nil {
		return absPath
	}
	return fullPath
}

func getMockFileLock(lockKey string) *sync.RWMutex {
	hasher := fnv.New32a()
	_, _ = hasher.Write([]byte(lockKey))
	return &mockFileLockStripes[hasher.Sum32()%mockFileLockStripeCount]
}

func (ys *MockYaml) writeMocksAtomically(path, fileName string, mocks []*models.Mock) error {
	targetPath := filepath.Join(path, fileName+".yaml")
	if len(mocks) == 0 {
		if err := os.Remove(targetPath); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	}

	if err := os.MkdirAll(path, 0o777); err != nil {
		return err
	}

	tmpFile, err := os.CreateTemp(path, fileName+".*.tmp")
	if err != nil {
		return err
	}
	tmpPath := tmpFile.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tmpPath)
		}
	}()

	writer := bufio.NewWriter(tmpFile)
	if version := utils.GetVersionAsComment(); version != "" {
		if _, err := writer.WriteString(version); err != nil {
			_ = tmpFile.Close()
			return err
		}
	}

	for i, mock := range mocks {
		if i > 0 {
			if _, err := writer.WriteString("---\n"); err != nil {
				_ = tmpFile.Close()
				return err
			}
		}
		mockYaml, err := EncodeMock(mock, ys.Logger)
		if err != nil {
			_ = tmpFile.Close()
			return err
		}
		data, err := yamlLib.Marshal(&mockYaml)
		if err != nil {
			_ = tmpFile.Close()
			return err
		}
		if _, err := writer.Write(data); err != nil {
			_ = tmpFile.Close()
			return err
		}
	}

	if err := writer.Flush(); err != nil {
		_ = tmpFile.Close()
		return err
	}
	if err := tmpFile.Sync(); err != nil {
		_ = tmpFile.Close()
		return err
	}
	if err := tmpFile.Close(); err != nil {
		return err
	}

	fileMode, err := resolveMockFileMode(targetPath)
	if err != nil {
		return err
	}
	if err := os.Chmod(tmpPath, fileMode); err != nil {
		return err
	}

	if err := replaceFile(tmpPath, targetPath); err != nil {
		return err
	}
	cleanup = false
	return nil
}

func resolveMockFileMode(targetPath string) (os.FileMode, error) {
	info, err := os.Stat(targetPath)
	if err == nil {
		return info.Mode().Perm(), nil
	}
	if os.IsNotExist(err) {
		return 0o777, nil
	}
	return 0, err
}

func replaceFile(src, dst string) error {
	if err := os.Rename(src, dst); err == nil {
		return nil
	} else {
		renameErr := err
		if _, statErr := os.Stat(dst); statErr != nil {
			if os.IsNotExist(statErr) {
				return renameErr
			}
			return fmt.Errorf("failed to stat target after rename error: %v; initial rename error: %w", statErr, renameErr)
		}

		if removeErr := os.Remove(dst); removeErr != nil {
			return fmt.Errorf("failed to remove target for replace: %v; initial rename error: %w", removeErr, renameErr)
		}

		if retryErr := os.Rename(src, dst); retryErr != nil {
			return fmt.Errorf("failed to replace file after removing existing target: %v; initial rename error: %w", retryErr, renameErr)
		}
	}
	return nil
}

// UpdateMocks prunes unused mocks from the mock file and keeps required ones.
//
// mockNames is a keep-set keyed by mock name (values carry models.MockState details).
// Mocks present in mockNames are retained; other mocks may still be retained by
// timestamp-based exemptions (for replay writes and startup/init traffic).
func (ys *MockYaml) UpdateMocks(ctx context.Context, testSetID string, mockNames map[string]models.MockState, pruneBefore time.Time, firstTestCaseTime time.Time) error {
	mockFileName := "mocks"
	if ys.MockName != "" {
		mockFileName = ys.MockName
	}
	path := filepath.Join(ys.MockPath, testSetID)
	lock := getMockFileLock(mockFileLockKey(path, mockFileName))
	lock.Lock()
	defer lock.Unlock()

	ys.Logger.Debug("pruning unused mocks",
		zap.Any("consumedMocks", mockNames),
		zap.String("testSetID", testSetID),
		zap.String("path", filepath.Join(path, mockFileName+".yaml")),
		zap.Time("pruneBefore", pruneBefore))

	// Read the mocks from the yaml file
	mockPath, err := yaml.ValidatePath(filepath.Join(path, mockFileName+".yaml"))
	if err != nil {
		utils.LogError(ys.Logger, err, "failed to read mocks due to inaccessible path", zap.String("at_path", filepath.Join(path, mockFileName+".yaml")))
		return err
	}
	if _, err := os.Stat(mockPath); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		utils.LogError(ys.Logger, err, "failed to find the mocks yaml file")
		return err
	}
	reader, err := yaml.NewMockReader(ctx, ys.Logger, path, mockFileName)
	if err != nil {
		utils.LogError(ys.Logger, err, "failed to read the mocks from yaml file", zap.String("at_path", filepath.Join(path, mockFileName+".yaml")))
		return err
	}
	defer reader.Close()

	var mockYamls []*yaml.NetworkTrafficDoc
	for {
		doc, err := reader.ReadNextDoc()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			utils.LogError(ys.Logger, err, "failed to decode the yaml file documents", zap.String("at_path", filepath.Join(path, mockFileName+".yaml")))
			return fmt.Errorf("failed to decode the yaml file documents. error: %v", err.Error())
		}
		mockYamls = append(mockYamls, doc)
	}
	mocks, err := DecodeMocks(mockYamls, ys.Logger)
	if err != nil {
		return err
	}

	newMocks := make([]*models.Mock, 0, len(mocks))
	prunedCount := 0
	for _, mock := range mocks {
		if mock.Spec.Metadata["type"] == "config" {
			newMocks = append(newMocks, mock)
			continue
		}
		if _, ok := mockNames[mock.Name]; ok {
			newMocks = append(newMocks, mock)
			continue
		}
		// Preserve mocks written after replay start.
		if !mock.Spec.ReqTimestampMock.IsZero() && mock.Spec.ReqTimestampMock.After(pruneBefore) {
			newMocks = append(newMocks, mock)
			continue
		}
		// Keep startup/init mocks: mocks recorded before the first test case
		// are connection-level or app-init traffic (DNS, TLS, DB handshake,
		// config fetch, etc.) that only fires once at app startup. In multi-
		// test-set replays without app restart, these won't be consumed in
		// later test-sets but are still needed for future replays.
		if !firstTestCaseTime.IsZero() && !mock.Spec.ReqTimestampMock.IsZero() &&
			mock.Spec.ReqTimestampMock.Before(firstTestCaseTime) {
			newMocks = append(newMocks, mock)
			continue
		}
		prunedCount++
	}

	if err := ys.writeMocksAtomically(path, mockFileName, newMocks); err != nil {
		return err
	}

	ys.Logger.Debug("pruned mocks successfully",
		zap.String("testSetID", testSetID),
		zap.Int("total", len(mocks)),
		zap.Int("kept", len(newMocks)),
		zap.Int("pruned", prunedCount),
		zap.Time("pruneBefore", pruneBefore))

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
	lock := getMockFileLock(mockFileLockKey(mockPath, mockFileName))
	lock.Lock()
	defer lock.Unlock()

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

func (ys *MockYaml) GetFilteredMocks(ctx context.Context, testSetID string, afterTime time.Time, beforeTime time.Time, mocksThatHaveMappings map[string]bool, mocksWeNeed map[string]bool) ([]*models.Mock, error) {

	var tcsMocks = make([]*models.Mock, 0)
	mockFileName := "mocks"
	if ys.MockName != "" {
		mockFileName = ys.MockName
	}

	path := filepath.Join(ys.MockPath, testSetID)
	lock := getMockFileLock(mockFileLockKey(path, mockFileName))
	lock.RLock()
	defer lock.RUnlock()

	mockPath, err := yaml.ValidatePath(path + "/" + mockFileName + ".yaml")
	if err != nil {
		return nil, err
	}

	if _, err := os.Stat(mockPath); err == nil {
		// Use buffered reader for memory-efficient reading of large mock files
		reader, err := yaml.NewMockReader(ctx, ys.Logger, path, mockFileName)
		if err != nil {
			utils.LogError(ys.Logger, err, "failed to read the mocks from yaml file", zap.String("session", filepath.Base(path)), zap.String("path", mockPath))
			return nil, err
		}
		defer reader.Close()

		hasContent := false
		for {
			doc, err := reader.ReadNextDoc()
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				return nil, fmt.Errorf("failed to decode the yaml file documents. error: %v", err.Error())
			}
			hasContent = true

			// Decode each YAML document into models.Mock as it is read.
			mocks, err := DecodeMocks([]*yaml.NetworkTrafficDoc{doc}, ys.Logger)
			if err != nil {
				utils.LogError(ys.Logger, err, "failed to decode the config mocks from yaml doc", zap.String("session", filepath.Base(path)))
				return nil, err
			}

			for _, mock := range mocks {
				_, isMappedToSpecificTest := mocksThatHaveMappings[mock.Name]

				_, isNeededForCurrentRun := mocksWeNeed[mock.Name]
				if isMappedToSpecificTest && !isNeededForCurrentRun {
					continue
				}
				isFilteredMock := true
				switch mock.Kind {
				case "Generic":
					isFilteredMock = false
				case "Postgres":
					isFilteredMock = false
				case "Http":
					isFilteredMock = false
				case "Http2":
					isFilteredMock = false
				case "Redis":
					isFilteredMock = false
				case "Hbase":
					isFilteredMock = false
				case "MySQL":
					isFilteredMock = false
				case "DNS":
					isFilteredMock = false
				}
				if mock.Spec.Metadata["type"] != "config" && isFilteredMock {
					tcsMocks = append(tcsMocks, mock)
				}
			}
		}

		if !hasContent {
			utils.LogError(ys.Logger, nil, "failed to read the mocks from yaml file", zap.String("session", filepath.Base(path)), zap.String("path", mockPath))
			return nil, fmt.Errorf("failed to get mocks, empty file")
		}
	}

	filtered := pkg.FilterTcsMocks(ctx, ys.Logger, tcsMocks, afterTime, beforeTime)
	ys.Logger.Debug("filtered mocks count", zap.Int("count", len(filtered)))

	return filtered, nil
}

func (ys *MockYaml) GetUnFilteredMocks(ctx context.Context, testSetID string, afterTime time.Time, beforeTime time.Time, mocksThatHaveMappings map[string]bool, mocksWeNeed map[string]bool) ([]*models.Mock, error) {

	var configMocks = make([]*models.Mock, 0)

	mockName := "mocks"
	if ys.MockName != "" {
		mockName = ys.MockName
	}

	path := filepath.Join(ys.MockPath, testSetID)
	lock := getMockFileLock(mockFileLockKey(path, mockName))
	lock.RLock()
	defer lock.RUnlock()

	mockPath, err := yaml.ValidatePath(path + "/" + mockName + ".yaml")
	if err != nil {
		return nil, err
	}

	if _, err := os.Stat(mockPath); err == nil {
		// Use buffered reader for memory-efficient reading of large mock files
		reader, err := yaml.NewMockReader(ctx, ys.Logger, path, mockName)
		if err != nil {
			utils.LogError(ys.Logger, err, "failed to read the mocks from config yaml", zap.String("session", filepath.Base(path)))
			return nil, err
		}
		defer reader.Close()

		for {
			doc, err := reader.ReadNextDoc()
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				return nil, fmt.Errorf("failed to decode the yaml file documents. error: %v", err.Error())
			}

			// Decode each YAML document into models.Mock as it is read.
			mocks, err := DecodeMocks([]*yaml.NetworkTrafficDoc{doc}, ys.Logger)
			if err != nil {
				utils.LogError(ys.Logger, err, "failed to decode the config mocks from yaml doc", zap.String("session", filepath.Base(path)))
				return nil, err
			}

			for _, mock := range mocks {
				_, isMappedToSpecificTest := mocksThatHaveMappings[mock.Name]

				_, isNeededForCurrentRun := mocksWeNeed[mock.Name]
				if isMappedToSpecificTest && !isNeededForCurrentRun {
					continue
				}
				isUnFilteredMock := false
				switch mock.Kind {
				case "Generic":
					isUnFilteredMock = true
				case "Postgres":
					isUnFilteredMock = true
				case "Http":
					isUnFilteredMock = true
				case "Http2":
					isUnFilteredMock = true
				case "Redis":
					isUnFilteredMock = true
				case "Hbase":
					isUnFilteredMock = true
				case "MySQL", "PostgresV2":
					isUnFilteredMock = true
				case "DNS":
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

	tcsMocks, err := ys.GetUnFilteredMocks(ctx, testSetID, time.Time{}, time.Time{}, nil, nil)
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

	// Read the mocks from the yaml file
	mockPath, err := yaml.ValidatePath(filepath.Join(path, mockFileName+".yaml"))
	if err != nil {
		utils.LogError(ys.Logger, err, "failed to read mocks due to inaccessible path", zap.String("at_path", filepath.Join(path, mockFileName+".yaml")))
		return err
	}

	// Delete all contents of the mocks directory
	err = os.RemoveAll(mockPath)
	if err != nil {
		utils.LogError(ys.Logger, err, "failed to delete old mocks", zap.String("path", mockPath))
		return err
	}

	ys.Logger.Info("Successfully cleared old mocks for refresh.", zap.String("testSet", testSetID))
	return nil
}

func (ys *MockYaml) GetCurrMockID() int64 {
	return atomic.LoadInt64(&ys.idCounter)
}

func (ys *MockYaml) ResetCounterID() {
	atomic.StoreInt64(&ys.idCounter, -1)
}
