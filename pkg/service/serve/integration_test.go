package serve

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/agent/mocks"
	"go.uber.org/zap"
	"gopkg.in/yaml.v3"
)

// Integration tests that test the full flow with real HTTP server

// TestIntegration_ServeWithRealMocks tests the serve command with actual mock files
func TestIntegration_ServeWithRealMocks(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	// Arrange
	logger := zap.NewNop()
	tmpDir := t.TempDir()

	// Create mock directory structure
	testSetPath := filepath.Join(tmpDir, "keploy", "mocks", "test-set-1")
	err := os.MkdirAll(testSetPath, 0755)
	require.NoError(t, err)

	// Create a sample mock file
	mockData := createSampleMockYAML()
	mockFile := filepath.Join(testSetPath, "mocks.yaml")
	err = os.WriteFile(mockFile, []byte(mockData), 0644)
	require.NoError(t, err)

	// Find available port
	port := uint32(findAvailablePort(t))

	cfg := &config.Config{
		Path:       tmpDir,
		ServerPort: port,
		Test: config.Test{
			SelectedTests: map[string][]string{
				"test-set-1": {},
			},
			Delay:          5,
			FallBackOnMiss: false,
		},
	}

	mockProxy := mocks.NewMockProxy(t)
	mockProxy.On("Mock", mock.Anything, mockMatchAny()).Return(nil)
	mockProxy.On("SetMocks", mock.Anything, mockMatchAny(), mockMatchAny()).Return(nil)
	mockProxy.On("StartProxy", mock.Anything, mock.Anything).Return(nil)

	svc := New(logger, cfg, mockProxy)

	// Create context with cancel
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Start server in goroutine
	errChan := make(chan error, 1)
	go func() {
		errChan <- svc.Start(ctx)
	}()

	// Wait for server to start
	time.Sleep(100 * time.Millisecond)

	// Act & Assert - Test health endpoint
	healthURL := fmt.Sprintf("http://localhost:%d/health", port)
	resp, err := http.Get(healthURL)
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode)
	body, _ := io.ReadAll(resp.Body)
	assert.Equal(t, "OK", string(body))

	// Test status endpoint
	statusURL := fmt.Sprintf("http://localhost:%d/status", port)
	resp, err = http.Get(statusURL)
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, "application/json", resp.Header.Get("Content-Type"))

	statusBody, _ := io.ReadAll(resp.Body)
	assert.Contains(t, string(statusBody), "status")
	assert.Contains(t, string(statusBody), "running")
	assert.Contains(t, string(statusBody), fmt.Sprintf("%d", port))

	// Cancel context to stop server
	cancel()

	// Wait for server to stop
	select {
	case <-errChan:
		// Server stopped
	case <-time.After(2 * time.Second):
		t.Fatal("Server did not stop in time")
	}
}

func mockMatchAny() interface{} {
	return mock.MatchedBy(func(interface{}) bool { return true })
}

func createSampleMockYAML() string {
	mockData := map[string]interface{}{
		"version": "api.keploy.io/v1beta1",
		"kind":    "Http",
		"name":    "mock-1",
		"spec": map[string]interface{}{
			"metadata": map[string]string{"type": "data"},
			"req":      map[string]interface{}{"method": "GET", "url": "http://example.com/api"},
			"resp":     map[string]interface{}{"statusCode": 200, "body": `{"message": "success"}`},
		},
	}
	data, _ := yaml.Marshal(mockData)
	return string(data)
}

func findAvailablePort(t *testing.T) int {
	for port := 17000; port < 18000; port++ {
		listener, err := net.Listen("tcp", fmt.Sprintf(":%d", port))
		if err == nil {
			_ = listener.Close()
			return port
		}
	}
	t.Fatal("Could not find an available port")
	return 0
}
