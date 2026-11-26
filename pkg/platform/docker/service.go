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
	FindContainerInComposeFiles(ctx context.Context, composePaths []string, scope ComposeScope, containerName string) (*ComposeServiceInfo, error)

	// Function for generating keploy-agent service configuration
	ModifyComposeForAgent(compose *Compose, opts models.SetupOptions, appContainerName string) error

	// FindContainerInCompose is FindContainerInComposeFiles for an in-memory
	// compose document, resolved as `docker compose -f -` resolves it in scope.
	FindContainerInCompose(ctx context.Context, content []byte, scope ComposeScope, containerName string) (*ComposeServiceInfo, error)

	// MarshalCompose serialises a Compose structure to YAML bytes without touching disk.
	MarshalCompose(compose *Compose) ([]byte, error)
}

type NetworkInfo struct {
	External bool
	Name     string
}

// ComposeExtraFile is a companion compose file (beyond ComposePath) that also
// keys the app service and carries properties that conflict with or override
// `network_mode: service:keploy-agent` / `pid: service:keploy-agent` (ports,
// networks, dns*, extra_hosts, network_mode, pid, hostname, domainname,
// mac_address, links, expose) which keploy moves to keploy-agent or strips
// before compose merges the files.
type ComposeExtraFile struct {
	Path    string
	Compose *Compose
}

// ComposeServiceInfo represents information about a service found in a Docker Compose file
type ComposeServiceInfo struct {
	AppServiceName string             // Service name in the compose file, used for --exit-code-from flag
	ComposePath    string             // Path to the docker-compose file
	Networks       []string           // Networks that the service is connected to
	Ports          []string           // Port mappings for the service
	Compose        *Compose           // The entire Compose structure for further modifications if needed
	ExtraFiles     []ComposeExtraFile // Companion compose files whose app service also needed conflicting keys stripped
}
