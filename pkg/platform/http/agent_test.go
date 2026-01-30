package http

import (
	"bytes"
	"context"
	"encoding/gob"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"testing"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/client"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/client/app"
	"go.keploy.io/server/v3/pkg/models"
	kdocker "go.keploy.io/server/v3/pkg/platform/docker"
	"go.uber.org/zap"
)

// MockDockerClient mocks the kdocker.Client interface
type MockDockerClient struct {
	client.APIClient
	mock.Mock
}

func (m *MockDockerClient) ExtractNetworksForContainer(containerName string) (map[string]*network.EndpointSettings, error) {
	args := m.Called(containerName)
	return args.Get(0).(map[string]*network.EndpointSettings), args.Error(1)
}

func (m *MockDockerClient) ReadComposeFile(filePath string) (*kdocker.Compose, error) {
	args := m.Called(filePath)
	val := args.Get(0)
	if val == nil {
		return nil, args.Error(1)
	}
	return val.(*kdocker.Compose), args.Error(1)
}

func (m *MockDockerClient) WriteComposeFile(compose *kdocker.Compose, path string) error {
	args := m.Called(compose, path)
	return args.Error(0)
}

func (m *MockDockerClient) IsContainerRunning(containerName string) (bool, error) {
	args := m.Called(containerName)
	return args.Bool(0), args.Error(1)
}

func (m *MockDockerClient) CreateVolume(ctx context.Context, volumeName string, recreate bool, driverOpts map[string]string) error {
	args := m.Called(ctx, volumeName, recreate, driverOpts)
	return args.Error(0)
}

func (m *MockDockerClient) FindContainerInComposeFiles(composePaths []string, containerName string) (*kdocker.ComposeServiceInfo, error) {
	args := m.Called(composePaths, containerName)
	val := args.Get(0)
	if val == nil {
		return nil, args.Error(1)
	}
	return val.(*kdocker.ComposeServiceInfo), args.Error(1)
}

func (m *MockDockerClient) ModifyComposeForAgent(compose *kdocker.Compose, opts models.SetupOptions, appContainerName string) error {
	args := m.Called(compose, opts, appContainerName)
	return args.Error(0)
}

func TestNew(t *testing.T) {
	logger := zap.NewNop()
	mockDocker := new(MockDockerClient)
	cfg := &config.Config{}

	agent := New(logger, mockDocker, cfg)

	assert.NotNil(t, agent)
	assert.Equal(t, logger, agent.logger)
	assert.Equal(t, cfg, agent.conf)
}

func TestSetup_DockerCompose_MissingFile(t *testing.T) {
	// This test verifies that Setup logic runs correctly (allocates ports, updates config)
	// and attempts to setup the app. We expect it to fail at App.Setup because
	// no docker-compose file exists in the test environment.
	// This avoids the 30s timeout of Native setup and tests the Docker branch.

	logger := zap.NewNop()
	mockDocker := new(MockDockerClient)

	cfg := &config.Config{
		Agent: config.Agent{
			SetupOptions: models.SetupOptions{
				AgentPort: 6789,
				ProxyPort: 2000,
				DnsPort:   53,
			},
		},
		ProxyPort: 2000,
		DNSPort:   53,
	}

	agent := New(logger, mockDocker, cfg)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	opts := models.SetupOptions{
		CommandType: "docker-compose",
		Container:   "my-container",
	}

	err := agent.Setup(ctx, "docker-compose up", opts)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "can't find the docker compose file")
}

func TestSetup_DockerCompose_Success(t *testing.T) {
	logger := zap.NewNop()
	mockDocker := new(MockDockerClient)

	// Create a dummy docker-compose file
	composeContent := `version: "3.9"
services:
  app:
    image: my-app:latest
`
	fileName := "docker-compose.yml"
	err := os.WriteFile(fileName, []byte(composeContent), 0644)
	assert.NoError(t, err)
	defer os.Remove(fileName)

	cfg := &config.Config{
		Agent: config.Agent{
			SetupOptions: models.SetupOptions{
				AgentPort: 6789,
				ProxyPort: 2000,
				DnsPort:   53,
			},
		},
		ProxyPort: 2000,
		DNSPort:   53,
	}

	agent := New(logger, mockDocker, cfg)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	opts := models.SetupOptions{
		CommandType: "docker-compose",
		Container:   "app",
	}

	// Mocks for App.Setup flow
	// 1. FindContainerInComposeFiles
	mockDocker.On("FindContainerInComposeFiles", []string{fileName}, "app").Return(&kdocker.ComposeServiceInfo{
		Compose:        &kdocker.Compose{},
		ComposePath:    fileName,
		AppServiceName: "app",
		Ports:          []string{"8080:8080"},
		Networks:       []string{"net1"},
	}, nil)

	// 2. ModifyComposeForAgent
	mockDocker.On("ModifyComposeForAgent", mock.Anything, mock.Anything, "app").Return(nil)

	// 3. WriteComposeFile
	mockDocker.On("WriteComposeFile", mock.Anything, "docker-compose-tmp.yaml").Return(nil)

	err = agent.Setup(ctx, "docker-compose -f docker-compose.yml up", opts)
	assert.NoError(t, err)

	// Cleanup tmp file if created by Setup
	os.Remove("docker-compose-tmp.yaml")
}

func TestRun(t *testing.T) {
	logger := zap.NewNop()
	mockDocker := new(MockDockerClient)
	cfg := &config.Config{
		Agent: config.Agent{
			SetupOptions: models.SetupOptions{
				AgentURI: "http://localhost:6789",
			},
		},
	}

	agent := New(logger, mockDocker, cfg)

	// Create and inject a dummy App
	// We use "echo" as command so it runs and exits successfully quickly
	dummyApp := app.NewApp(logger, "echo hello", mockDocker, models.SetupOptions{
		CommandType: "native",
	})

	agent.apps.Store(uint64(0), dummyApp)

	ctx := context.Background()
	appErr := agent.Run(ctx, models.RunOptions{})

	assert.Nil(t, appErr.Err)
	assert.Equal(t, models.AppErrorType(""), appErr.AppErrorType)
}

// MockRoundTripper allows mocking valid http responses
type MockRoundTripper struct {
	RoundTripFunc func(req *http.Request) (*http.Response, error)
}

func (m *MockRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	if m.RoundTripFunc != nil {
		return m.RoundTripFunc(req)
	}
	return &http.Response{
		StatusCode: http.StatusNotFound,
		Body:       io.NopCloser(bytes.NewBufferString("Not Found")),
	}, nil
}

func TestGetIncoming(t *testing.T) {
	logger := zap.NewNop()
	mockDocker := new(MockDockerClient)
	cfg := &config.Config{
		Agent: config.Agent{
			SetupOptions: models.SetupOptions{
				AgentURI: "http://localhost:6789",
			},
		},
	}

	agent := New(logger, mockDocker, cfg)

	// Mock the HTTP client
	mockTripper := &MockRoundTripper{}
	agent.client.Transport = mockTripper

	// Test Case Data
	expectedTC := models.TestCase{
		Name: "test-case-1",
	}

	mockTripper.RoundTripFunc = func(req *http.Request) (*http.Response, error) {
		assert.Equal(t, "POST", req.Method)
		assert.Equal(t, "http://localhost:6789/incoming", req.URL.String())

		// Simulate stream of TestCases
		var buf bytes.Buffer
		encoder := json.NewEncoder(&buf)
		_ = encoder.Encode(expectedTC)

		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(&buf),
			Header:     make(http.Header),
		}, nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()

	ch, err := agent.GetIncoming(ctx, models.IncomingOptions{})
	assert.NoError(t, err)
	assert.NotNil(t, ch)

	// Read from channel
	select {
	case tc, ok := <-ch:
		if !ok {
			t.Fatal("Channel closed unexpectedly")
		}
		assert.Equal(t, expectedTC.Name, tc.Name)
	case <-time.After(100 * time.Millisecond):
		t.Fatal("Timeout waiting for test case")
	}
}

func TestGetOutgoing(t *testing.T) {
	logger := zap.NewNop()
	mockDocker := new(MockDockerClient)
	cfg := &config.Config{
		Agent: config.Agent{
			SetupOptions: models.SetupOptions{
				AgentURI: "http://localhost:6789",
			},
		},
	}

	agent := New(logger, mockDocker, cfg)
	mockTripper := &MockRoundTripper{}
	agent.client.Transport = mockTripper

	// Mock Data
	expectedMock := models.Mock{
		Name: "mock-1",
	}

	mockTripper.RoundTripFunc = func(req *http.Request) (*http.Response, error) {
		assert.Equal(t, "POST", req.Method)
		assert.Contains(t, req.URL.String(), "/outgoing")

		var buf bytes.Buffer
		encoder := gob.NewEncoder(&buf)
		_ = encoder.Encode(expectedMock)

		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(&buf),
			Header:     make(http.Header),
		}, nil
	}

	eg, ctx := errgroup.WithContext(context.Background())
	ctx = context.WithValue(ctx, models.ErrGroupKey, eg)
	ctx, cancel := context.WithTimeout(ctx, 1*time.Second)
	defer cancel()

	ch, err := agent.GetOutgoing(ctx, models.OutgoingOptions{})
	assert.NoError(t, err)
	assert.NotNil(t, ch)

	select {
	case m, ok := <-ch:
		if !ok {
			t.Fatal("Channel closed unexpectedly")
		}
		assert.Equal(t, expectedMock.Name, m.Name)
	case <-time.After(100 * time.Millisecond):
		t.Fatal("Timeout waiting for mock")
	}
}

func TestMockOutgoing(t *testing.T) {
	logger := zap.NewNop()
	mockDocker := new(MockDockerClient)
	cfg := &config.Config{
		Agent: config.Agent{
			SetupOptions: models.SetupOptions{
				AgentURI: "http://localhost:6789",
			},
		},
	}

	agent := New(logger, mockDocker, cfg)
	mockTripper := &MockRoundTripper{}
	agent.client.Transport = mockTripper

	mockTripper.RoundTripFunc = func(req *http.Request) (*http.Response, error) {
		assert.Equal(t, "POST", req.Method)
		assert.Contains(t, req.URL.String(), "/mock")

		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(bytes.NewBufferString(`{}`)), // Empty JSON for minimal AgentResp
			Header:     make(http.Header),
		}, nil
	}

	ctx := context.Background()
	err := agent.MockOutgoing(ctx, models.OutgoingOptions{})
	assert.NoError(t, err)
}

func TestStoreMocks(t *testing.T) {
	logger := zap.NewNop()
	mockDocker := new(MockDockerClient)
	cfg := &config.Config{
		Agent: config.Agent{
			SetupOptions: models.SetupOptions{
				AgentURI: "http://localhost:6789",
			},
		},
	}

	agent := New(logger, mockDocker, cfg)
	mockTripper := &MockRoundTripper{}
	agent.client.Transport = mockTripper

	mockTripper.RoundTripFunc = func(req *http.Request) (*http.Response, error) {
		assert.Equal(t, "POST", req.Method)
		assert.Contains(t, req.URL.String(), "/storemocks")
		assert.Equal(t, "application/x-gob", req.Header.Get("Content-Type"))

		// Return empty gob encoded AgentResp
		var buf bytes.Buffer
		enc := gob.NewEncoder(&buf)
		_ = enc.Encode(models.AgentResp{})

		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(&buf),
			Header:     make(http.Header),
		}, nil
	}

	ctx := context.Background()
	err := agent.StoreMocks(ctx, []*models.Mock{{Name: "m1"}}, nil)
	assert.NoError(t, err)
}

func TestHooks_BeforeSimulate(t *testing.T) {
	logger := zap.NewNop()
	mockDocker := new(MockDockerClient)
	cfg := &config.Config{
		Agent: config.Agent{
			SetupOptions: models.SetupOptions{
				AgentURI: "http://localhost:6789",
			},
		},
	}

	agent := New(logger, mockDocker, cfg)
	mockTripper := &MockRoundTripper{}
	agent.client.Transport = mockTripper

	mockTripper.RoundTripFunc = func(req *http.Request) (*http.Response, error) {
		assert.Equal(t, "POST", req.Method)
		assert.Contains(t, req.URL.String(), "/hooks/before-simulate")
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(bytes.NewBufferString(`{}`)),
			Header:     make(http.Header),
		}, nil
	}

	ctx := context.Background()
	ts := time.Now()
	err := agent.BeforeSimulate(ctx, &ts, "test-set-1", "test-case-1")
	assert.NoError(t, err)
}

func TestHooks_AfterSimulate(t *testing.T) {
	logger := zap.NewNop()
	mockDocker := new(MockDockerClient)
	cfg := &config.Config{
		Agent: config.Agent{
			SetupOptions: models.SetupOptions{
				AgentURI: "http://localhost:6789",
			},
		},
	}

	agent := New(logger, mockDocker, cfg)
	mockTripper := &MockRoundTripper{}
	agent.client.Transport = mockTripper

	mockTripper.RoundTripFunc = func(req *http.Request) (*http.Response, error) {
		assert.Equal(t, "POST", req.Method)
		assert.Contains(t, req.URL.String(), "/hooks/after-simulate")
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(bytes.NewBufferString(`{}`)),
			Header:     make(http.Header),
		}, nil
	}

	ctx := context.Background()
	err := agent.AfterSimulate(ctx, "test-case-1", "test-set-1")
	assert.NoError(t, err)
}

func TestHooks_BeforeTestRun(t *testing.T) {
	logger := zap.NewNop()
	mockDocker := new(MockDockerClient)
	cfg := &config.Config{
		Agent: config.Agent{
			SetupOptions: models.SetupOptions{
				AgentURI: "http://localhost:6789",
			},
		},
	}

	agent := New(logger, mockDocker, cfg)
	mockTripper := &MockRoundTripper{}
	agent.client.Transport = mockTripper

	mockTripper.RoundTripFunc = func(req *http.Request) (*http.Response, error) {
		assert.Equal(t, "POST", req.Method)
		assert.Contains(t, req.URL.String(), "/hooks/before-test-run")
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(bytes.NewBufferString(`{}`)),
			Header:     make(http.Header),
		}, nil
	}

	ctx := context.Background()
	err := agent.BeforeTestRun(ctx, "test-run-1")
	assert.NoError(t, err)
}

func TestHooks_BeforeTestSetCompose(t *testing.T) {
	logger := zap.NewNop()
	mockDocker := new(MockDockerClient)
	cfg := &config.Config{
		Agent: config.Agent{
			SetupOptions: models.SetupOptions{
				AgentURI: "http://localhost:6789",
			},
		},
	}

	agent := New(logger, mockDocker, cfg)
	mockTripper := &MockRoundTripper{}
	agent.client.Transport = mockTripper

	mockTripper.RoundTripFunc = func(req *http.Request) (*http.Response, error) {
		assert.Equal(t, "POST", req.Method)
		assert.Contains(t, req.URL.String(), "/hooks/before-test-set-compose")
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(bytes.NewBufferString(`{}`)),
			Header:     make(http.Header),
		}, nil
	}

	ctx := context.Background()
	err := agent.BeforeTestSetCompose(ctx, "test-run-1", true)
	assert.NoError(t, err)
}

func TestHooks_AfterTestRun(t *testing.T) {
	logger := zap.NewNop()
	mockDocker := new(MockDockerClient)
	cfg := &config.Config{
		Agent: config.Agent{
			SetupOptions: models.SetupOptions{
				AgentURI: "http://localhost:6789",
			},
		},
	}

	agent := New(logger, mockDocker, cfg)
	mockTripper := &MockRoundTripper{}
	agent.client.Transport = mockTripper

	mockTripper.RoundTripFunc = func(req *http.Request) (*http.Response, error) {
		assert.Equal(t, "POST", req.Method)
		assert.Contains(t, req.URL.String(), "/hooks/after-test-run")
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(bytes.NewBufferString(`{}`)),
			Header:     make(http.Header),
		}, nil
	}

	ctx := context.Background()
	err := agent.AfterTestRun(ctx, "test-run-1", []string{"ts1"}, models.TestCoverage{})
	assert.NoError(t, err)
}

func TestUpdateMockParams(t *testing.T) {
	logger := zap.NewNop()
	mockDocker := new(MockDockerClient)
	cfg := &config.Config{
		Agent: config.Agent{
			SetupOptions: models.SetupOptions{
				AgentURI: "http://localhost:6789",
			},
		},
	}

	agent := New(logger, mockDocker, cfg)
	mockTripper := &MockRoundTripper{}
	agent.client.Transport = mockTripper

	mockTripper.RoundTripFunc = func(req *http.Request) (*http.Response, error) {
		assert.Equal(t, "POST", req.Method)
		assert.Contains(t, req.URL.String(), "/updatemockparams")
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(bytes.NewBufferString(`{}`)),
			Header:     make(http.Header),
		}, nil
	}

	ctx := context.Background()
	err := agent.UpdateMockParams(ctx, models.MockFilterParams{})
	assert.NoError(t, err)
}

func TestGetConsumedMocks(t *testing.T) {
	logger := zap.NewNop()
	mockDocker := new(MockDockerClient)
	cfg := &config.Config{
		Agent: config.Agent{
			SetupOptions: models.SetupOptions{
				AgentURI: "http://localhost:6789",
			},
		},
	}

	agent := New(logger, mockDocker, cfg)
	mockTripper := &MockRoundTripper{}
	agent.client.Transport = mockTripper

	expectedMocks := []models.MockState{{}}

	mockTripper.RoundTripFunc = func(req *http.Request) (*http.Response, error) {
		assert.Equal(t, "GET", req.Method)
		assert.Contains(t, req.URL.String(), "/consumedmocks")

		var buf bytes.Buffer
		_ = json.NewEncoder(&buf).Encode(expectedMocks)

		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(&buf),
			Header:     make(http.Header),
		}, nil
	}

	ctx := context.Background()
	mocks, err := agent.GetConsumedMocks(ctx)
	assert.NoError(t, err)
	assert.Len(t, mocks, 1)
}