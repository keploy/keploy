package docker

import (
	"context"

	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/client"
	"gopkg.in/yaml.v3"
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
	GetServiceNode(compose *Compose, containerName string) *yaml.Node

	CreateNetwork(network string) error
	MakeNetworkExternal(c *Compose) error
	SetKeployNetwork(c *Compose) (*NetworkInfo, error)
	VolumeExists(service *yaml.Node, source, destination string) bool
	SetVolume(service *yaml.Node, source, destination string)
	EnvironmentExists(service *yaml.Node, key string, value string) bool
	SetEnvironment(service *yaml.Node, key, value string)
	ReadComposeFile(filePath string) (*Compose, error)
	WriteComposeFile(compose *Compose, path string) error

	IsContainerRunning(containerName string) (bool, error)
	CreateVolume(ctx context.Context, volumeName string, recreate bool) error
}

type NetworkInfo struct {
	External bool
	Name     string
}
