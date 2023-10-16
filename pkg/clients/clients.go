package clients

import (
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/client"
)

type InternalDockerClient interface {
	client.APIClient
	ExtractNetworksForContainer(containerName string) (map[string]*network.EndpointSettings, error)
	ConnectContainerToNetworks(containerName string, settings map[string]*network.EndpointSettings) error
	ConnectContainerToNetworksByNames(containerName string, networkName []string) error
	StopAndRemoveDockerContainer() error
	GetContainerID() string
	SetContainerID(containerID string)
	NetworkExists(network string) (bool, error)
	CheckNetworkInfo(filePath string) (bool, bool, string)
	CreateCustomNetwork(network string) error
	MakeNetworkExternal(dockerComposeFilePath, newComposeFile string) error
	AddNetworkToCompose(dockerComposeFilePath, newComposeFile string) error
}
