package provider

import (
	"context"
	"testing"

	"errors"

	"github.com/docker/docker/api/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"go.keploy.io/server/v2/config"
	dockerMocks "go.keploy.io/server/v2/pkg/platform/docker"
	"go.uber.org/zap"
)

// TestGenerateDockerEnvs_WithOneEnv_123 tests the GenerateDockerEnvs function with one environment variable.
func TestGenerateDockerEnvs_WithOneEnv_123(t *testing.T) {
	config := DockerConfigStruct{
		Envs: map[string]string{
			"KEY1": "VALUE1",
		},
	}
	result := GenerateDockerEnvs(config)
	assert.Equal(t, "-e KEY1='VALUE1'", result)
}

// TestStartInDocker_NotDockerCmd_456 tests that StartInDocker returns early if the command is not a Docker command.
func TestStartInDocker_NotDockerCmd_456(t *testing.T) {
	conf := &config.Config{InDocker: false, Command: "not-a-docker-command"}
	err := StartInDocker(context.Background(), zap.NewNop(), conf)
	assert.NoError(t, err)
}

// TestStartInDocker_InDocker_678 tests that the function returns early if already in Docker.
func TestStartInDocker_InDocker_678(t *testing.T) {
	conf := &config.Config{InDocker: true, Command: "docker run my-app"}
	err := StartInDocker(context.Background(), zap.NewNop(), conf)
	assert.NoError(t, err)
}

// TestAddKeployNetwork_NetworkExists_234 verifies that NetworkCreate is not called if the network exists.
func TestAddKeployNetwork_NetworkExists_234(t *testing.T) {
	mockClient := new(dockerMocks.MockClient)
	networks := []types.NetworkResource{
		{Name: "keploy-network"},
	}
	mockClient.On("NetworkList", mock.Anything, mock.Anything).Return(networks, nil)

	addKeployNetwork(context.Background(), zap.NewNop(), mockClient)

	mockClient.AssertExpectations(t)
	// Assert that NetworkCreate was not called
	mockClient.AssertNotCalled(t, "NetworkCreate", mock.Anything, "keploy-network", mock.Anything)
}

// TestAddKeployNetwork_ListFails_567 ensures the function handles errors from NetworkList gracefully.
func TestAddKeployNetwork_ListFails_567(t *testing.T) {
	mockClient := new(dockerMocks.MockClient)
	mockClient.On("NetworkList", mock.Anything, mock.Anything).Return(nil, errors.New("list failed"))

	// As addKeployNetwork only logs errors, we just check that it runs without panicking.
	addKeployNetwork(context.Background(), zap.NewNop(), mockClient)

	mockClient.AssertExpectations(t)
	mockClient.AssertNotCalled(t, "NetworkCreate", mock.Anything, mock.Anything, mock.Anything)
}

// TestAddKeployNetwork_CreateFails_890 ensures the function handles errors from NetworkCreate gracefully.
func TestAddKeployNetwork_CreateFails_890(t *testing.T) {
	mockClient := new(dockerMocks.MockClient)
	mockClient.On("NetworkList", mock.Anything, mock.Anything).Return([]types.NetworkResource{}, nil)
	mockClient.On("NetworkCreate", mock.Anything, "keploy-network", mock.Anything).Return(types.NetworkCreateResponse{}, errors.New("create failed"))

	// As addKeployNetwork only logs errors, we just check that it runs without panicking.
	addKeployNetwork(context.Background(), zap.NewNop(), mockClient)

	mockClient.AssertExpectations(t)
}

// TestAddKeployNetwork_CreateSuccess_909 tests the successful creation of the keploy network.
func TestAddKeployNetwork_CreateSuccess_909(t *testing.T) {
	mockClient := new(dockerMocks.MockClient)
	mockClient.On("NetworkList", mock.Anything, mock.Anything).Return([]types.NetworkResource{}, nil)
	mockClient.On("NetworkCreate", mock.Anything, "keploy-network", mock.Anything).Return(types.NetworkCreateResponse{ID: "test-id"}, nil)

	addKeployNetwork(context.Background(), zap.NewNop(), mockClient)

	mockClient.AssertExpectations(t)
	mockClient.AssertCalled(t, "NetworkCreate", mock.Anything, "keploy-network", mock.Anything)
}
