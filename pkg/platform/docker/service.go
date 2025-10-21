package docker

import (
	"context"

	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/client"
	"go.keploy.io/server/v3/pkg/models"
)

type Client interface {
	client.APIClient
	ExtractNetworksForContainer(containerName string) (map[string]*network.EndpointSettings, error)

	ReadComposeFile(filePath string) (*Compose, error)
	WriteComposeFile(compose *Compose, path string) error
	IsContainerRunning(containerName string) (bool, error)
	CreateVolume(ctx context.Context, volumeName string, recreate bool, driverOpts map[string]string) error

	// New functions for finding containers in compose files
	FindContainerInComposeFiles(composePaths []string, containerName string) (*ComposeServiceInfo, error)

	// Function for generating keploy-agent service configuration
	ModifyComposeForAgent(compose *Compose, opts models.SetupOptions, appContainerName string) error
}

type NetworkInfo struct {
	External bool
	Name     string
}

// ComposeServiceInfo represents information about a service found in a Docker Compose file
type ComposeServiceInfo struct {
	ComposePath string   // Path to the docker-compose file
	Networks    []string // Networks that the service is connected to
	Ports       []string // Port mappings for the service
	Compose     *Compose // The entire Compose structure for further modifications if needed
}
