package docker

import (
	"context"
	"fmt"

	"os"
	"path/filepath"
	"time"

	nativeDockerClient "github.com/docker/docker/client"
	"go.uber.org/zap"
	"gopkg.in/yaml.v3"

	"github.com/docker/docker/api/types/network"

	"go.keploy.io/server/pkg/clients"

	"github.com/docker/docker/api/types"
	dockerContainerPkg "github.com/docker/docker/api/types/container"
)

const (
	kDefaultTimeoutForDockerQuery = 1 * time.Minute
)

type internalDockerClient struct {
	nativeDockerClient.APIClient
	timeoutForDockerQuery time.Duration
	logger                *zap.Logger
	containerID           string
}

func NewInternalDockerClient(logger *zap.Logger) (clients.InternalDockerClient, error) {
	dockerClient, err := nativeDockerClient.NewClientWithOpts(nativeDockerClient.FromEnv,
		nativeDockerClient.WithAPIVersionNegotiation())
	if err != nil {
		return nil, err
	}
	return &internalDockerClient{
		APIClient:             dockerClient,
		timeoutForDockerQuery: kDefaultTimeoutForDockerQuery,
		logger:                logger,
	}, nil
}

// Getter function for containerID
func (idc *internalDockerClient) GetContainerID() string {
	return idc.containerID
}

// Setter function for containerID
func (idc *internalDockerClient) SetContainerID(containerID string) {
	idc.containerID = containerID
}

// ExtractNetworksForContainer returns the list of all the networks that the container is a part of.
// Note that if a user did not explicitly attach the container to a network, the Docker daemon attaches it
// to a network called "bridge".
func (idc *internalDockerClient) ExtractNetworksForContainer(containerName string) (map[string]*network.EndpointSettings, error) {
	ctx, cancel := context.WithTimeout(context.Background(), idc.timeoutForDockerQuery)
	defer cancel()

	containerJSON, err := idc.ContainerInspect(ctx, containerName)
	if err != nil {
		idc.logger.Error("Could not inspect container via the Docker API", zap.Error(err),
			zap.Any("container_name", containerName))
		return nil, err
	}

	if settings := containerJSON.NetworkSettings; settings != nil {
		return settings.Networks, nil
	} else {
		// Docker attaches the container to "bridge" network by default.
		// If the network list is empty, the docker daemon is possibly misbehaving,
		// or the container is in a bad state.
		idc.logger.Error("The network list for the given container is empty. This is unexpected.",
			zap.Any("container_name", containerName))
		return nil, fmt.Errorf("the container is not attached to any network")
	}
}

func (idc *internalDockerClient) ConnectContainerToNetworks(containerName string, settings map[string]*network.EndpointSettings) error {
	if settings == nil {
		return fmt.Errorf("provided network settings is empty")
	}

	existingNetworks, err := idc.ExtractNetworksForContainer(containerName)
	if err != nil {
		return fmt.Errorf("could not get existing networks for container %s", containerName)
	}

	ctx, cancel := context.WithTimeout(context.Background(), idc.timeoutForDockerQuery)
	defer cancel()

	for networkName, setting := range settings {
		// If the container is already part of this network, skip it.
		_, ok := existingNetworks[networkName]
		if ok {
			continue
		}

		err := idc.NetworkConnect(ctx, networkName, containerName, setting)
		if err != nil {
			return err
		}
	}

	return nil
}

func (idc *internalDockerClient) ConnectContainerToNetworksByNames(containerName string, networkNames []string) error {
	if len(networkNames) == 0 {
		return fmt.Errorf("provided network names list is empty")
	}

	existingNetworks, err := idc.ExtractNetworksForContainer(containerName)
	if err != nil {
		return fmt.Errorf("could not get existing networks for container %s", containerName)
	}

	ctx, cancel := context.WithTimeout(context.Background(), idc.timeoutForDockerQuery)
	defer cancel()

	for _, networkName := range networkNames {
		// If the container is already part of this network, skip it.
		_, ok := existingNetworks[networkName]
		if ok {
			continue
		}

		// As there are no specific settings, use nil for the settings parameter.
		err := idc.NetworkConnect(ctx, networkName, containerName, nil)
		if err != nil {
			return err
		}
	}

	return nil
}

// Stop and Remove the docker container
func (idc *internalDockerClient) StopDockerContainer(removeContainer bool) error {
	dockerClient := idc
	containerID := idc.containerID

	container, err := dockerClient.ContainerInspect(context.Background(), containerID)
	if err != nil {
		return fmt.Errorf("failed to inspect the docker container: %w", err)
	}

	if container.State.Status == "running" {
		err = dockerClient.ContainerStop(context.Background(), containerID, dockerContainerPkg.StopOptions{})
		if err != nil {
			return fmt.Errorf("failed to stop the docker container: %w", err)
		}
	}

	idc.logger.Debug("Docker Container stopped successfully.")

	if removeContainer {
		removeOptions := types.ContainerRemoveOptions{
			RemoveVolumes: true,
			Force:         true,
		}

		err = dockerClient.ContainerRemove(context.Background(), containerID, removeOptions)
		if err != nil {
			return fmt.Errorf("failed to remove the docker container: %w", err)
		}

		idc.logger.Debug("Docker Container removed successfully.")
	}

	return nil
}

// NetworkExists checks if the given network exists locally or not
func (idc *internalDockerClient) NetworkExists(networkName string) (bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), idc.timeoutForDockerQuery)
	defer cancel()

	// Retrieve all networks.
	networks, err := idc.NetworkList(ctx, types.NetworkListOptions{})
	if err != nil {
		return false, fmt.Errorf("error retrieving networks: %v", err)
	}

	// Check if the specified network is in the list.
	for _, network := range networks {
		if network.Name == networkName {
			return true, nil
		}
	}

	return false, nil
}

// CreateCustomNetwork creates a custom docker network of type bridge.
func (idc *internalDockerClient) CreateCustomNetwork(networkName string) error {
	ctx, cancel := context.WithTimeout(context.Background(), idc.timeoutForDockerQuery)
	defer cancel()

	_, err := idc.NetworkCreate(ctx, networkName, types.NetworkCreate{
		Driver: "bridge",
	})

	return err
}

// Compose structure to represent all the fields of a Docker Compose file
type Compose struct {
	Version  string    `yaml:"version,omitempty"`
	Services yaml.Node `yaml:"services,omitempty"`
	Networks yaml.Node `yaml:"networks,omitempty"`
	Volumes  yaml.Node `yaml:"volumes,omitempty"`
	Configs  yaml.Node `yaml:"configs,omitempty"`
	Secrets  yaml.Node `yaml:"secrets,omitempty"`
}

// CheckBindMounts returns information about whether bind mounts if they are being used contain relative file names or not
func (idc *internalDockerClient) CheckBindMounts(filePath string) bool {
	data, err := os.ReadFile(filePath)
	if err != nil {
		idc.logger.Error("error reading file", zap.Any("filePath", filePath), zap.Error(err))
		return false
	}

	var compose Compose
	err = yaml.Unmarshal(data, &compose)
	if err != nil {
		idc.logger.Error("error unmarshalling YAML into compose struct", zap.Error(err))
		return false
	}

	if compose.Services.Content == nil {
		return false
	}

	for _, service := range compose.Services.Content {
		for i, item := range service.Content {

			if i+1 >= len(service.Content) {
				break
			}

			if item.Value == "volumes" {
				// volumeKeyNode := service.Content[i] or item
				volumeValueNode := service.Content[i+1]

				// Loop over all the volume mounts
				for _, volumeMount := range volumeValueNode.Content {
					// If volume mount starts with ./ or ../ then it as a relative path so return true
					if volumeMount.Kind == yaml.ScalarNode && (volumeMount.Value[:2] == "./" || volumeMount.Value[:3] == "../") {
						return true
					}
				}
			}
		}
	}

	return false

}

// CheckNetworkInfo returns information about network name and also about whether the network is external or not in a docker-compose file.
func (idc *internalDockerClient) CheckNetworkInfo(filePath string) (bool, bool, string) {
	data, err := os.ReadFile(filePath)
	if err != nil {
		idc.logger.Error("error reading file", zap.Any("filePath", filePath), zap.Error(err))
		return false, false, ""
	}

	var compose Compose
	err = yaml.Unmarshal(data, &compose)
	if err != nil {
		idc.logger.Error("error unmarshalling YAML into compose struct", zap.Error(err))
		return false, false, ""
	}

	if compose.Networks.Content == nil {
		return false, false, ""
	}

	var defaultNetworkName string

	for i := 0; i < len(compose.Networks.Content); i += 2 {
		if i+1 >= len(compose.Networks.Content) {
			break
		}
		networkKeyNode := compose.Networks.Content[i]
		networkValueNode := compose.Networks.Content[i+1]

		if defaultNetworkName == "" {
			defaultNetworkName = networkKeyNode.Value
		}

		isExternal := false
		var externalName string

		for j := 0; j < len(networkValueNode.Content); j += 2 {
			if j+1 >= len(networkValueNode.Content) {
				break
			}
			propertyNode := networkValueNode.Content[j]
			valueNode := networkValueNode.Content[j+1]
			if propertyNode.Value == "external" {
				if valueNode.Kind == yaml.ScalarNode && valueNode.Value == "true" {
					isExternal = true
					break
				} else if valueNode.Kind == yaml.MappingNode {
					for k := 0; k < len(valueNode.Content); k += 2 {
						if k+1 >= len(valueNode.Content) {
							break
						}
						subPropertyNode := valueNode.Content[k]
						subValueNode := valueNode.Content[k+1]
						if subPropertyNode.Value == "name" {
							isExternal = true
							externalName = subValueNode.Value
							break
						}
					}
				}
				break
			}
		}

		if isExternal {
			if externalName != "" {
				return true, true, externalName
			}
			return true, true, networkKeyNode.Value
		}
	}

	if defaultNetworkName != "" {
		return true, false, defaultNetworkName
	}

	return false, false, ""
}

// Inspect Keploy docker container to get bind mount for current directory
func (idc *internalDockerClient) GetHostWorkingDirectory() (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), idc.timeoutForDockerQuery)
	defer cancel()

	curDir, err := os.Getwd()
	if err != nil {
		idc.logger.Error("failed to get current working directory", zap.Error(err))
		return "", err
	}

	container, err := idc.ContainerInspect(ctx, "keploy-v2")
	if err != nil {
		idc.logger.Error("error inspecting keploy-v2 container", zap.Error(err))
		return "", err
	}
	containerMounts := container.Mounts
	// Loop through container mounts and find the mount for current directory in the container
	for _, mount := range containerMounts {
		if mount.Destination == curDir {
			idc.logger.Debug(fmt.Sprintf("found mount for %s in keploy-v2 container", curDir), zap.Any("mount", mount))
			return mount.Source, nil
		}
	}
	return "", fmt.Errorf(fmt.Sprintf("could not find mount for %s in keploy-v2 container", curDir))
}

// ReplaceRelativePaths replaces relative paths in bind mounts with absolute paths
func (idc *internalDockerClient) ReplaceRelativePaths(dockerComposefilePath, newComposeFile string) error {
	data, err := os.ReadFile(dockerComposefilePath)
	if err != nil {
		return err
	}

	var compose Compose
	err = yaml.Unmarshal(data, &compose)
	if err != nil {
		return err
	}

	hostWorkingDirectory, err := idc.GetHostWorkingDirectory()
	if err != nil {
		return err
	}

	dockerComposeContext, err := filepath.Abs(filepath.Join(hostWorkingDirectory, dockerComposefilePath))
	if err != nil {
		idc.logger.Error("error getting absolute path for docker compose file", zap.Error(err))
		return err
	}
	dockerComposeContext = filepath.Dir(dockerComposeContext)
	idc.logger.Debug("docker compose file location in host filesystem", zap.Any("dockerComposeContext", dockerComposeContext))

	// Loop through all services in compose file
	for _, service := range compose.Services.Content {

		for i, item := range service.Content {

			if i+1 >= len(service.Content) {
				break
			}

			if item.Value == "volumes" {
				// volumeKeyNode := service.Content[i] or item
				volumeValueNode := service.Content[i+1]

				// Loop over all the volume mounts
				for _, volumeMount := range volumeValueNode.Content {
					// If volume mount starts with ./ or ../ then it is a relative path
					if volumeMount.Kind == yaml.ScalarNode && (volumeMount.Value[:2] == "./" || volumeMount.Value[:3] == "../") {

						// Replace the relative path with absolute path
						absPath, err := filepath.Abs(filepath.Join(dockerComposeContext, volumeMount.Value))
						if err != nil {
							return err
						}
						volumeMount.Value = absPath
					}
				}
			}
		}
	}

	newData, err := yaml.Marshal(&compose)
	if err != nil {
		return err
	}

	newFilePath := filepath.Join(filepath.Dir(dockerComposefilePath), newComposeFile)
	err = os.WriteFile(newFilePath, newData, 0644)
	if err != nil {
		return err
	}

	return nil
}

// MakeNetworkExternal makes the existing network of the user docker compose file external and save it to a new file
func (idc *internalDockerClient) MakeNetworkExternal(dockerComposefilePath, newComposeFile string) error {
	data, err := os.ReadFile(dockerComposefilePath)
	if err != nil {
		return err
	}

	var compose Compose
	err = yaml.Unmarshal(data, &compose)
	if err != nil {
		return err
	}

	// Iterate over all networks and check the 'external' flag.
	if compose.Networks.Content != nil {
		for i := 0; i < len(compose.Networks.Content); i += 2 {
			if i+1 >= len(compose.Networks.Content) {
				break
			}
			// networkKeyNode := compose.Networks.Content[i]
			networkValueNode := compose.Networks.Content[i+1]

			// If it's a shorthand notation or null value, initialize it as an empty mapping node
			if (networkValueNode.Kind == yaml.ScalarNode && networkValueNode.Value == "") || networkValueNode.Tag == "!!null" {
				networkValueNode.Kind = yaml.MappingNode
				networkValueNode.Tag = ""
				networkValueNode.Content = make([]*yaml.Node, 0)
			}

			externalFound := false
			for index, propertyNode := range networkValueNode.Content {
				if index+1 >= len(networkValueNode.Content) {
					break
				}
				if propertyNode.Value == "external" {
					externalFound = true
					valueNode := networkValueNode.Content[index+1]
					if valueNode.Kind == yaml.ScalarNode && (valueNode.Value == "false" || valueNode.Value == "") {
						valueNode.Value = "true"
					}
					break
				}
			}

			if !externalFound {
				networkValueNode.Content = append(networkValueNode.Content,
					&yaml.Node{Kind: yaml.ScalarNode, Value: "external"},
					&yaml.Node{Kind: yaml.ScalarNode, Value: "true"},
				)
			}
		}
	}

	newData, err := yaml.Marshal(&compose)
	if err != nil {
		return err
	}

	newFilePath := filepath.Join(filepath.Dir(dockerComposefilePath), newComposeFile)
	err = os.WriteFile(newFilePath, newData, 0644)
	if err != nil {
		return err
	}

	return nil
}

// AddNetworkToCompose adds the keploy-network network to the new docker compose file and copy rest of the contents from
// existing user docker compose file
func (idc *internalDockerClient) AddNetworkToCompose(dockerComposefilePath, newComposeFile string) error {
	data, err := os.ReadFile(dockerComposefilePath)
	if err != nil {
		return err
	}

	var compose Compose
	err = yaml.Unmarshal(data, &compose)
	if err != nil {
		return err
	}

	// Ensure that the top-level networks mapping exists.
	if compose.Networks.Content == nil {
		compose.Networks.Kind = yaml.MappingNode
		compose.Networks.Content = make([]*yaml.Node, 0)
	}

	// Check if "keploy-network" already exists
	exists := false
	for i := 0; i < len(compose.Networks.Content); i += 2 {
		if compose.Networks.Content[i].Value == "keploy-network" {
			exists = true
			break
		}
	}

	if !exists {
		// Add the keploy-network with external: true
		compose.Networks.Content = append(compose.Networks.Content,
			&yaml.Node{Kind: yaml.ScalarNode, Value: "keploy-network"},
			&yaml.Node{
				Kind: yaml.MappingNode,
				Content: []*yaml.Node{
					&yaml.Node{Kind: yaml.ScalarNode, Value: "external"},
					&yaml.Node{Kind: yaml.ScalarNode, Value: "true"},
				},
			},
		)
	}

	// Add or modify network for each service
	for _, service := range compose.Services.Content {
		networksFound := false
		for _, item := range service.Content {
			if item.Value == "networks" {
				networksFound = true
				break
			}
		}

		if !networksFound {
			service.Content = append(service.Content,
				&yaml.Node{Kind: yaml.ScalarNode, Value: "networks"},
				&yaml.Node{
					Kind: yaml.SequenceNode,
					Content: []*yaml.Node{
						&yaml.Node{Kind: yaml.ScalarNode, Value: "keploy-network"},
					},
				},
			)
		} else {
			for _, item := range service.Content {
				if item.Value == "networks" {
					item.Content = append(item.Content, &yaml.Node{Kind: yaml.ScalarNode, Value: "keploy-network"})
				}
			}
		}
	}

	newData, err := yaml.Marshal(&compose)
	if err != nil {
		return err
	}

	newFilePath := filepath.Join(filepath.Dir(dockerComposefilePath), newComposeFile)
	err = os.WriteFile(newFilePath, newData, 0644)
	if err != nil {
		return err
	}

	idc.logger.Debug("successfully created kdocker-compose.yaml file")
	return nil
}
