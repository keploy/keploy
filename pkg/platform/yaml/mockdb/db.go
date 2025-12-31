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
	"strings"
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
	mocks, err := DecodeMocks(mockYamls, ys.Logger)
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
	// Use simple sequential naming
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

// generateContextualName generates a contextual name for a mock based on its type and content
func (ys *MockYaml) generateContextualName(mock *models.Mock) string {
	// Build name parts based on mock kind
	parts := []string{}

	switch mock.Kind {
	case models.HTTP:
		parts = append(parts, "http")
		if mock.Spec.HTTPReq != nil {
			method := mock.Spec.HTTPReq.Method
			if method != "" {
				parts = append(parts, strings.ToLower(string(method)))
			}
			// Extract resource from URL
			if mock.Spec.HTTPReq.URL != "" {
				resource := extractResourceFromURL(mock.Spec.HTTPReq.URL)
				if resource != "" {
					parts = append(parts, resource)
				}
			}
		}
	case models.Postgres:
		parts = append(parts, "postgres")
		if mock.Spec.Metadata != nil {
			if op, ok := mock.Spec.Metadata["operation"]; ok {
				parts = append(parts, strings.ToLower(fmt.Sprint(op)))
			}
		}
	case models.MySQL:
		parts = append(parts, "mysql")
		if mock.Spec.Metadata != nil {
			if op, ok := mock.Spec.Metadata["operation"]; ok {
				parts = append(parts, strings.ToLower(fmt.Sprint(op)))
			}
		}
	case models.Mongo:
		parts = append(parts, "mongo")
		if mock.Spec.Metadata != nil {
			if col, ok := mock.Spec.Metadata["collection"]; ok {
				parts = append(parts, sanitizeNamePart(fmt.Sprint(col)))
			}
		}
	case models.REDIS:
		parts = append(parts, "redis")
		if mock.Spec.Metadata != nil {
			if cmd, ok := mock.Spec.Metadata["command"]; ok {
				parts = append(parts, strings.ToLower(fmt.Sprint(cmd)))
			}
		}
	case models.GENERIC:
		parts = append(parts, "generic")
	case models.GRPC_EXPORT:
		parts = append(parts, "grpc")
	default:
		return "" // Fall back to sequential naming
	}

	if len(parts) == 0 {
		return ""
	}

	// Add sequential ID for uniqueness
	parts = append(parts, fmt.Sprintf("%d", ys.getNextID()))

	return strings.Join(parts, "-")
}

// extractResourceFromURL extracts the main resource name from a URL path
func extractResourceFromURL(urlPath string) string {
	// Remove query parameters
	if idx := strings.Index(urlPath, "?"); idx != -1 {
		urlPath = urlPath[:idx]
	}

	// Split path segments
	segments := strings.Split(strings.Trim(urlPath, "/"), "/")

	// Find the last non-ID segment
	for i := len(segments) - 1; i >= 0; i-- {
		seg := segments[i]
		if seg != "" && !isIDSegment(seg) && !isVersionSegment(seg) {
			return sanitizeNamePart(seg)
		}
	}

	return ""
}

// isIDSegment checks if a segment looks like an ID
func isIDSegment(segment string) bool {
	// UUID pattern
	if len(segment) == 36 && strings.Count(segment, "-") == 4 {
		return true
	}
	// Numeric ID
	for _, c := range segment {
		if c < '0' || c > '9' {
			return false
		}
	}
	return len(segment) > 0 && len(segment) <= 20
}

// isVersionSegment checks if a segment is an API version
func isVersionSegment(segment string) bool {
	if len(segment) < 2 {
		return false
	}
	lower := strings.ToLower(segment)
	return lower[0] == 'v' && lower[1] >= '0' && lower[1] <= '9'
}

// sanitizeNamePart converts a string to a valid name part
func sanitizeNamePart(name string) string {
	name = strings.ToLower(name)
	result := strings.Builder{}
	for _, c := range name {
		if (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') {
			result.WriteRune(c)
		} else if c == '-' || c == '_' || c == ' ' {
			result.WriteRune('-')
		}
	}
	return strings.Trim(result.String(), "-")
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
			mocks, err := DecodeMocks([]*yaml.NetworkTrafficDoc{doc}, ys.Logger)
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
			mocks, err := DecodeMocks([]*yaml.NetworkTrafficDoc{doc}, ys.Logger)
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
