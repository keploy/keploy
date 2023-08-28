package clients

import (
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/client"
)

type InternalDockerClient interface {
	client.APIClient
	ExtractNetworksForContainer(containerName string) (map[string]*network.EndpointSettings, error)
	ConnectContainerToNetworks(containerName string, settings map[string]*network.EndpointSettings) error
}
