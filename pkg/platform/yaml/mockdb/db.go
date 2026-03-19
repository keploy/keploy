// Package mockdb provides a mock database implementation.
package mockdb

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
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

func New(Logger *zap.Logger, mockPath string, mockName string) *MockYaml {
	return &MockYaml{
		MockPath:  mockPath,
		MockName:  mockName,
		Logger:    Logger,
		idCounter: -1,
	}
}

// UpdateMocks deletes the mocks from the mock file with given names
//
// mockNames is a map which contains the name of the mocks as key and a isConfig boolean as value
func (ys *MockYaml) UpdateMocks(ctx context.Context, testSetID string, mockNames map[string]models.MockState) error {
	mockFileName := "mocks"
	if ys.MockName != "" {
		mockFileName = ys.MockName
	}
	path := filepath.Join(ys.MockPath, testSetID)
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
	// Use buffered reader instead of loading entire file into memory
	reader, err := yaml.NewMockReader(ctx, ys.Logger, path, mockFileName)
	if err != nil {
		utils.LogError(ys.Logger, err, "failed to read the mocks from yaml file", zap.String("at_path", filepath.Join(path, mockFileName+".yaml")))
		return err
	}
	defer reader.Close()

	// decode the mocks read from the yaml file using streaming reader
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
	var newMocks []*models.Mock
	for _, mock := range mocks {
		if _, ok := mockNames[mock.Name]; ok || mock.Spec.Metadata["type"] == "config" {
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
		data, err := yamlLib.Marshal(&mockYaml)
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
	mockPath := filepath.Join(ys.MockPath, testSetID)
	mockFileName := ys.MockName
	if mockFileName == "" {
		mockFileName = "mocks"
	}

	// If this is an SSE stream mock, split frames into a separate stream file
	if mock.Spec.Metadata != nil && mock.Spec.Metadata["type"] == "sse-stream" && mock.Spec.HTTPResp != nil && mock.Spec.HTTPResp.Body != "" {
		err := ys.writeSSEStreamFile(ctx, mockPath, mock)
		if err != nil {
			utils.LogError(ys.Logger, err, "failed to write SSE stream file")
			return err
		}
	}

	mockYaml, err := EncodeMock(mock, ys.Logger)
	if err != nil {
		return err
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

// writeSSEStreamFile writes SSE frame data to a separate stream file and
// updates the mock metadata to reference it. The body is cleared from the mock.
func (ys *MockYaml) writeSSEStreamFile(ctx context.Context, mockPath string, mock *models.Mock) error {
	streamsDir := filepath.Join(mockPath, "streams")
	if err := os.MkdirAll(streamsDir, 0755); err != nil {
		return fmt.Errorf("failed to create streams directory: %w", err)
	}

	// Generate stream file name based on mock name
	streamFileName := fmt.Sprintf("%s-stream", mock.Name)

	// Write the frame data to the stream file
	frameData := []byte(mock.Spec.HTTPResp.Body)
	err := yaml.WriteFile(ctx, ys.Logger, streamsDir, streamFileName, frameData, false)
	if err != nil {
		return fmt.Errorf("failed to write stream file: %w", err)
	}

	// Update mock metadata to reference the stream file
	mock.Spec.Metadata["streamFile"] = filepath.Join("streams", streamFileName+".yaml")

	// Clear the body — frames are in the stream file now
	mock.Spec.HTTPResp.Body = ""

	ys.Logger.Info("Wrote SSE stream file",
		zap.String("streamFile", mock.Spec.Metadata["streamFile"]),
		zap.Int("dataSize", len(frameData)))

	return nil
}

// loadSSEStreamFrames loads SSE frame data from a stream file into the mock body.
func (ys *MockYaml) loadSSEStreamFrames(mock *models.Mock, testSetPath string) {
	if mock.Spec.Metadata == nil || mock.Spec.Metadata["type"] != "sse-stream" {
		return
	}
	streamFile, ok := mock.Spec.Metadata["streamFile"]
	if !ok || streamFile == "" {
		return
	}

	// Read the stream file
	streamPath := filepath.Join(testSetPath, streamFile)
	data, err := os.ReadFile(streamPath)
	if err != nil {
		ys.Logger.Warn("failed to read SSE stream file",
			zap.String("path", streamPath),
			zap.Error(err))
		return
	}

	// Load frames back into the body for replay
	mock.Spec.HTTPResp.Body = string(data)
	ys.Logger.Debug("Loaded SSE stream frames",
		zap.String("streamFile", streamFile),
		zap.Int("dataSize", len(data)))
}

func (ys *MockYaml) GetFilteredMocks(ctx context.Context, testSetID string, afterTime time.Time, beforeTime time.Time, mocksThatHaveMappings map[string]bool, mocksWeNeed map[string]bool) ([]*models.Mock, error) {

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
				// Load SSE stream frames from file if needed
				ys.loadSSEStreamFrames(mock, path)

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
				// Load SSE stream frames from file if needed
				ys.loadSSEStreamFrames(mock, path)

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

	// Also delete the streams directory if it exists
	streamsDir := filepath.Join(path, "streams")
	if _, err := os.Stat(streamsDir); err == nil {
		if err := os.RemoveAll(streamsDir); err != nil {
			utils.LogError(ys.Logger, err, "failed to delete SSE streams directory", zap.String("path", streamsDir))
			// Non-fatal — continue even if streams cleanup fails
		} else {
			ys.Logger.Debug("Deleted SSE streams directory", zap.String("path", streamsDir))
		}
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
