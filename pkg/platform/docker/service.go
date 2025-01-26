package docker

import (
	"context"

	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/client"
)

type Client interface {
	client.APIClient
	ExtractNetworksForContainer(containerName string) (map[string]*network.EndpointSettings, error)
	ConnectContainerToNetworks(containerName string, settings map[string]*network.EndpointSettings) error
	AttachNetwork(containerName string, networkName []string) error
	StopAndRemoveDockerContainer() error
	GetContainerID() string
	SetContainerID(containerID string)
	NetworkExists(network string) (bool, error)

	HasRelativePath(c *Compose) bool
	ForceAbsolutePath(c *Compose, basePath string) error

	GetNetworkInfo(compose *Compose) *NetworkInfo

	CreateNetwork(network string) error
	MakeNetworkExternal(c *Compose) error
	SetKeployNetwork(c *Compose) (*NetworkInfo, error)
	ReadComposeFile(filePath string) (*Compose, error)
	WriteComposeFile(compose *Compose, path string) error
	SetInitPid(c *Compose, containerName string) error
	IsContainerRunning(containerName string) (bool, error)
	CreateVolume(ctx context.Context, volumeName string, recreate bool) error
}

type NetworkInfo struct {
	External bool
	Name     string
}
