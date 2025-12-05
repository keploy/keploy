package serve

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/agent/mocks"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// TestNew tests the New function for creating a serve service
func TestNew(t *testing.T) {
	// Arrange
	logger := zap.NewNop()
	cfg := &config.Config{
		Path: "/test/path",
	}
	mockProxy := mocks.NewMockProxy(t)

	// Act
	svc := New(logger, cfg, mockProxy)

	// Assert
	require.NotNil(t, svc)
	assert.IsType(t, &serve{}, svc)
	serveImpl := svc.(*serve)
	assert.Equal(t, logger, serveImpl.logger)
	assert.Equal(t, cfg, serveImpl.config)
	assert.Equal(t, mockProxy, serveImpl.proxy)
	assert.NotNil(t, serveImpl.mockDb)
}

// TestSeparateMocks_AllFiltered tests separating mocks with all filtered mocks
func TestSeparateMocks_AllFiltered(t *testing.T) {
	// Arrange
	logger := zap.NewNop()
	cfg := &config.Config{Path: "."}
	mockProxy := mocks.NewMockProxy(t)
	svc := New(logger, cfg, mockProxy).(*serve)

	mocks := []*models.Mock{
		{
			Name: "mock-1",
			Kind: models.Mongo,
			Spec: models.MockSpec{
				Metadata: map[string]string{"type": "data"},
			},
		},
		{
			Name: "mock-2",
			Kind: models.GRPC_EXPORT,
			Spec: models.MockSpec{
				Metadata: map[string]string{"type": "data"},
			},
		},
	}

	// Act
	filtered, unfiltered := svc.separateMocksByFilter(mocks)

	// Assert
	assert.Len(t, filtered, 2, "All mocks should be filtered")
	assert.Len(t, unfiltered, 0, "No mocks should be unfiltered")
}

// TestSeparateMocks_AllUnfiltered tests separating mocks with all unfiltered mocks
func TestSeparateMocks_AllUnfiltered(t *testing.T) {
	// Arrange
	logger := zap.NewNop()
	cfg := &config.Config{Path: "."}
	mockProxy := mocks.NewMockProxy(t)
	svc := New(logger, cfg, mockProxy).(*serve)

	mocks := []*models.Mock{
		{
			Name: "mock-1",
			Kind: models.HTTP,
			Spec: models.MockSpec{
				Metadata: map[string]string{"type": "data"},
			},
		},
		{
			Name: "mock-2",
			Kind: models.MySQL,
			Spec: models.MockSpec{
				Metadata: map[string]string{"type": "config"},
			},
		},
		{
			Name: "mock-3",
			Kind: models.Postgres,
			Spec: models.MockSpec{
				Metadata: map[string]string{"type": "data"},
			},
		},
		{
			Name: "mock-4",
			Kind: models.REDIS,
			Spec: models.MockSpec{
				Metadata: map[string]string{"type": "data"},
			},
		},
		{
			Name: "mock-5",
			Kind: models.GENERIC,
			Spec: models.MockSpec{
				Metadata: map[string]string{"type": "data"},
			},
		},
	}

	// Act
	filtered, unfiltered := svc.separateMocksByFilter(mocks)

	// Assert
	assert.Len(t, filtered, 0, "No mocks should be filtered")
	assert.Len(t, unfiltered, 5, "All mocks should be unfiltered")
}

// TestSeparateMocks_Mixed tests separating mocks with mixed types
func TestSeparateMocks_Mixed(t *testing.T) {
	// Arrange
	logger := zap.NewNop()
	cfg := &config.Config{Path: "."}
	mockProxy := mocks.NewMockProxy(t)
	svc := New(logger, cfg, mockProxy).(*serve)

	mocks := []*models.Mock{
		{
			Name: "http-mock",
			Kind: models.HTTP,
			Spec: models.MockSpec{
				Metadata: map[string]string{"type": "data"},
			},
		},
		{
			Name: "grpc-mock",
			Kind: models.GRPC_EXPORT,
			Spec: models.MockSpec{
				Metadata: map[string]string{"type": "data"},
			},
		},
		{
			Name: "mysql-config",
			Kind: models.MySQL,
			Spec: models.MockSpec{
				Metadata: map[string]string{"type": "config"},
			},
		},
		{
			Name: "mysql-data",
			Kind: models.MySQL,
			Spec: models.MockSpec{
				Metadata: map[string]string{"type": "data"},
			},
		},
	}

	// Act
	filtered, unfiltered := svc.separateMocksByFilter(mocks)

	// Assert
	assert.Len(t, filtered, 1, "Only gRPC mock should be filtered")
	assert.Len(t, unfiltered, 3, "HTTP, MySQL config, and MySQL data should be unfiltered")

	// Verify filtered contains gRPC
	assert.Equal(t, "grpc-mock", filtered[0].Name)

	// Verify unfiltered contains the rest
	unfilteredNames := make([]string, len(unfiltered))
	for i, m := range unfiltered {
		unfilteredNames[i] = m.Name
	}
	assert.Contains(t, unfilteredNames, "http-mock")
	assert.Contains(t, unfilteredNames, "mysql-config")
	assert.Contains(t, unfilteredNames, "mysql-data")
}

// TestCountMocksByKind tests counting mocks by kind
func TestCountMocksByKind(t *testing.T) {
	// Arrange
	logger := zap.NewNop()
	cfg := &config.Config{Path: "."}
	mockProxy := mocks.NewMockProxy(t)
	svc := New(logger, cfg, mockProxy).(*serve)

	mocks := []*models.Mock{
		{Kind: models.HTTP},
		{Kind: models.HTTP},
		{Kind: models.GRPC_EXPORT},
		{Kind: models.MySQL},
		{Kind: models.MySQL},
		{Kind: models.MySQL},
		{Kind: models.Postgres},
	}

	// Act & Assert
	assert.Equal(t, 2, svc.countMocksByKind(mocks, models.HTTP))
	assert.Equal(t, 1, svc.countMocksByKind(mocks, models.GRPC_EXPORT))
	assert.Equal(t, 3, svc.countMocksByKind(mocks, models.MySQL))
	assert.Equal(t, 1, svc.countMocksByKind(mocks, models.Postgres))
	assert.Equal(t, 0, svc.countMocksByKind(mocks, models.REDIS))
}

// TestGetTestSets_FromConfig tests getting test sets from config
func TestGetTestSets_FromConfig(t *testing.T) {
	// Arrange
	logger := zap.NewNop()
	cfg := &config.Config{
		Path: ".",
		Test: config.Test{
			SelectedTests: map[string][]string{
				"test-set-1": {},
				"test-set-2": {},
			},
		},
	}
	mockProxy := mocks.NewMockProxy(t)
	svc := New(logger, cfg, mockProxy).(*serve)

	// Act
	testSets, err := svc.getTestSets()

	// Assert
	require.NoError(t, err)
	assert.Len(t, testSets, 2)
	assert.Contains(t, testSets, "test-set-1")
	assert.Contains(t, testSets, "test-set-2")
}

// TestGetTestSets_DirectoryNotExist tests error when directory doesn't exist
func TestGetTestSets_DirectoryNotExist(t *testing.T) {
	// Arrange
	logger := zap.NewNop()
	cfg := &config.Config{
		Path: "/nonexistent/path/that/does/not/exist",
		Test: config.Test{
			SelectedTests: map[string][]string{},
		},
	}
	mockProxy := mocks.NewMockProxy(t)
	svc := New(logger, cfg, mockProxy).(*serve)

	// Act
	testSets, err := svc.getTestSets()

	// Assert
	assert.Error(t, err)
	assert.Nil(t, testSets)
	assert.Contains(t, err.Error(), "does not exist")
}

// TestGetTestSets_AutoDiscovery tests automatic test set discovery
func TestGetTestSets_AutoDiscovery(t *testing.T) {
	// Arrange
	logger := zap.NewNop()

	// Create temp directory structure
	tmpDir := t.TempDir()
	mockPath := filepath.Join(tmpDir, "keploy", "mocks")
	err := os.MkdirAll(filepath.Join(mockPath, "test-set-1"), 0755)
	require.NoError(t, err)
	err = os.MkdirAll(filepath.Join(mockPath, "test-set-2"), 0755)
	require.NoError(t, err)
	err = os.MkdirAll(filepath.Join(mockPath, ".hidden"), 0755)
	require.NoError(t, err)

	cfg := &config.Config{
		Path: tmpDir,
		Test: config.Test{
			SelectedTests: map[string][]string{},
		},
	}
	mockProxy := mocks.NewMockProxy(t)
	svc := New(logger, cfg, mockProxy).(*serve)

	// Act
	testSets, err := svc.getTestSets()

	// Assert
	require.NoError(t, err)
	assert.Len(t, testSets, 2, "Should find 2 test sets (excluding hidden)")
	assert.Contains(t, testSets, "test-set-1")
	assert.Contains(t, testSets, "test-set-2")
	assert.NotContains(t, testSets, ".hidden")
}

// TestStart_ProxyMockError tests error handling when proxy.Mock fails
func TestStart_ProxyMockError(t *testing.T) {
	// Arrange
	logger := zap.NewNop()

	// Create temp directory with empty mocks
	tmpDir := t.TempDir()
	mockPath := filepath.Join(tmpDir, "keploy", "mocks", "test-set-1")
	err := os.MkdirAll(mockPath, 0755)
	require.NoError(t, err)

	cfg := &config.Config{
		Path:       tmpDir,
		ServerPort: 9999,
		Test: config.Test{
			SelectedTests: map[string][]string{
				"test-set-1": {},
			},
		},
	}

	mockProxy := mocks.NewMockProxy(t)
	mockProxy.On("Mock", mock.Anything, mock.Anything).Return(errors.New("proxy mock error"))

	svc := New(logger, cfg, mockProxy)

	ctx := context.Background()

	// Act
	err = svc.Start(ctx)

	// Assert
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "proxy mock error")
	mockProxy.AssertCalled(t, "Mock", mock.Anything, mock.Anything)
}

// TestStart_SetMocksError tests error handling when proxy.SetMocks fails
func TestStart_SetMocksError(t *testing.T) {
	// Arrange
	logger := zap.NewNop()

	// Create temp directory with empty mocks
	tmpDir := t.TempDir()
	mockPath := filepath.Join(tmpDir, "keploy", "mocks", "test-set-1")
	err := os.MkdirAll(mockPath, 0755)
	require.NoError(t, err)

	cfg := &config.Config{
		Path:       tmpDir,
		ServerPort: 9998,
		Test: config.Test{
			SelectedTests: map[string][]string{
				"test-set-1": {},
			},
		},
	}

	mockProxy := mocks.NewMockProxy(t)
	mockProxy.On("Mock", mock.Anything, mock.Anything).Return(nil)
	mockProxy.On("SetMocks", mock.Anything, mock.Anything, mock.Anything).
		Return(errors.New("set mocks error"))

	svc := New(logger, cfg, mockProxy)

	ctx := context.Background()

	// Act
	err = svc.Start(ctx)

	// Assert
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "set mocks error")
	mockProxy.AssertCalled(t, "Mock", mock.Anything, mock.Anything)
	mockProxy.AssertCalled(t, "SetMocks", mock.Anything, mock.Anything, mock.Anything)
}
