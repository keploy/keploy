// Package docker provides functionality for working with Docker containers.
package docker

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	nativeDockerClient "github.com/docker/docker/client"
	"go.keploy.io/server/v2/pkg/models"
	"go.keploy.io/server/v2/utils"
	"go.uber.org/zap"
	"gopkg.in/yaml.v3"

	"github.com/docker/docker/api/types/network"

	"github.com/docker/docker/api/types"
	dockerContainerPkg "github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/volume"
)

const (
	defaultTimeoutForDockerQuery = 1 * time.Minute
)

type Impl struct {
	nativeDockerClient.APIClient
	timeoutForDockerQuery time.Duration
	logger                *zap.Logger
	containerID           string
}

func New(logger *zap.Logger) (Client, error) {
	dockerClient, err := nativeDockerClient.NewClientWithOpts(nativeDockerClient.FromEnv,
		nativeDockerClient.WithAPIVersionNegotiation())
	if err != nil {
		return nil, err
	}
	return &Impl{
		APIClient:             dockerClient,
		timeoutForDockerQuery: defaultTimeoutForDockerQuery,
		logger:                logger,
	}, nil
}

// GetContainerID is a Getter function for containerID
func (idc *Impl) GetContainerID() string {
	return idc.containerID
}

// SetContainerID is a Setter function for containerID
func (idc *Impl) SetContainerID(containerID string) {
	idc.containerID = containerID
}

func (idc *Impl) SetInitPid(c *Compose, containerName string) error {
	for _, service := range c.Services.Content {
		var containerNameMatch bool
		var pidFound bool

		for i := 0; i < len(service.Content)-1; i++ {
			if service.Content[i].Kind == yaml.ScalarNode && service.Content[i].Value == "container_name" &&
				service.Content[i+1].Kind == yaml.ScalarNode && service.Content[i+1].Value == containerName {
				containerNameMatch = true
				break
			}
		}

		if containerNameMatch {
			for _, item := range service.Content {
				if item.Value == "pid" {
					pidFound = true
					break
				}
			}

			// Add `pid: container:keploy-init` only if not already present
			if !pidFound {
				service.Content = append(service.Content,
					&yaml.Node{Kind: yaml.ScalarNode, Value: "pid"},
					&yaml.Node{
						Kind:  yaml.ScalarNode,
						Value: "container:keploy-init",
					},
				)
			}
		}
	}
	return nil
}

func (idc *Impl) SetPidContainer(c *Compose, appContainerName string, agentContainerName string) error {
	// Construct the value for the 'pid' field, e.g., "container:keploy-v2"
	pidValue := fmt.Sprintf("container:%s", agentContainerName)

	// Iterate over all services defined in the docker-compose file.
	for _, service := range c.Services.Content {
		var containerNameMatch bool
		var pidFound bool

		// This loop finds the service that matches the application's container name.
		for i := 0; i < len(service.Content)-1; i++ {
			if service.Content[i].Kind == yaml.ScalarNode && service.Content[i].Value == "container_name" &&
				service.Content[i+1].Kind == yaml.ScalarNode && service.Content[i+1].Value == appContainerName {
				containerNameMatch = true
				break
			}
		}

		// If we found the correct service for the application...
		if containerNameMatch {
			// ...check if a 'pid' key already exists.
			for _, item := range service.Content {
				if item.Value == "pid" {
					pidFound = true
					break
				}
			}

			// If the 'pid' key is not already present, add it.
			if !pidFound {
				service.Content = append(service.Content,
					&yaml.Node{Kind: yaml.ScalarNode, Value: "pid"},
					&yaml.Node{
						Kind:  yaml.ScalarNode,
						Value: pidValue, // Use the new dynamic value here.
					},
				)
			}
		}
	}
	return nil
}

func (idc *Impl) SetAgentNamespacesInCompose(c *Compose, appContainerName string, agentContainerName string) error {
	pidValue := fmt.Sprintf("container:%s", agentContainerName)
	networkValue := fmt.Sprintf("container:%s", agentContainerName)

	for _, service := range c.Services.Content {
		var containerNameMatch bool
		pidFound := false
		networkModeFound := false

		// Find the service that matches the application's container name.
		for i := 0; i < len(service.Content)-1; i++ {
			if service.Content[i].Kind == yaml.ScalarNode && service.Content[i].Value == "container_name" &&
				service.Content[i+1].Kind == yaml.ScalarNode && service.Content[i+1].Value == appContainerName {
				containerNameMatch = true
				break
			}
		}

		if containerNameMatch {
			// Check if 'pid' and 'network_mode' keys already exist.
			for i := 0; i < len(service.Content); i++ {
				if service.Content[i].Value == "pid" {
					pidFound = true
				}
				if service.Content[i].Value == "network_mode" {
					networkModeFound = true
				}
			}

			// If 'pid' is not present, add it.
			if !pidFound {
				service.Content = append(service.Content,
					&yaml.Node{Kind: yaml.ScalarNode, Value: "pid"},
					&yaml.Node{Kind: yaml.ScalarNode, Value: pidValue},
				)
			}

			// If 'network_mode' is not present, add it.
			if !networkModeFound {
				service.Content = append(service.Content,
					&yaml.Node{Kind: yaml.ScalarNode, Value: "network_mode"},
					&yaml.Node{Kind: yaml.ScalarNode, Value: networkValue},
				)
			}
		}
	}
	return nil
}

// ExtractNetworksForContainer returns the list of all the networks that the container is a part of.
// Note that if a user did not explicitly attach the container to a network, the Docker daemon attaches it
// to a network called "bridge".
func (idc *Impl) ExtractNetworksForContainer(containerName string) (map[string]*network.EndpointSettings, error) {
	ctx, cancel := context.WithTimeout(context.Background(), idc.timeoutForDockerQuery)
	defer cancel()

	containerJSON, err := idc.ContainerInspect(ctx, containerName)
	if err != nil {
		utils.LogError(idc.logger, err, "couldn't inspect container via the Docker API", zap.String("containerName", containerName))
		return nil, err
	}

	if settings := containerJSON.NetworkSettings; settings != nil {
		return settings.Networks, nil
	}
	// Docker attaches the container to "bridge" network by default.
	// If the network list is empty, the docker daemon is possibly misbehaving,
	// or the container is in a bad state.
	utils.LogError(idc.logger, nil, "The network list for the given container is empty. This is unexpected.", zap.String("containerName", containerName))
	return nil, fmt.Errorf("the container is not attached to any network")
}

func (idc *Impl) ConnectContainerToNetworks(containerName string, settings map[string]*network.EndpointSettings) error {
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

func (idc *Impl) AttachNetwork(containerName string, networkNames []string) error {
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

// StopAndRemoveDockerContainer will Stop and Remove the docker container
func (idc *Impl) StopAndRemoveDockerContainer() error {
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

	removeOptions := types.ContainerRemoveOptions{
		RemoveVolumes: true,
		Force:         true,
	}

	err = dockerClient.ContainerRemove(context.Background(), containerID, removeOptions)
	if err != nil {
		return fmt.Errorf("failed to remove the docker container: %w", err)
	}

	idc.logger.Debug("Docker Container stopped and removed successfully.")

	return nil
}

// NetworkExists checks if the given network exists locally or not
func (idc *Impl) NetworkExists(networkName string) (bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), idc.timeoutForDockerQuery)
	defer cancel()

	// Retrieve all networks.
	networks, err := idc.NetworkList(ctx, types.NetworkListOptions{})
	if err != nil {
		return false, fmt.Errorf("error retrieving networks: %v", err)
	}

	// Check if the specified network is in the list.
	for _, net := range networks {
		if net.Name == networkName {
			return true, nil
		}
	}

	return false, nil
}

// CreateNetwork creates a custom docker network of type bridge.
func (idc *Impl) CreateNetwork(networkName string) error {
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

func (idc *Impl) ReadComposeFile(filePath string) (*Compose, error) {
	data, err := os.ReadFile(filePath)
	if err != nil {
		return nil, err
	}

	var compose Compose
	err = yaml.Unmarshal(data, &compose)
	if err != nil {
		return nil, err
	}

	return &compose, nil
}

func (idc *Impl) WriteComposeFile(compose *Compose, path string) error {
	data, err := yaml.Marshal(compose)
	if err != nil {
		return err
	}

	// write data to file

	err = os.WriteFile(path, data, 0644)
	if err != nil {
		return err
	}
	return nil
}

// HasRelativePath returns information about whether bind mounts if they are being used contain relative file names or not
func (idc *Impl) HasRelativePath(compose *Compose) bool {
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

// GetNetworkInfo CheckNetworkInfo returns information about network name and also about whether the network is external or not in a docker-compose file.
func (idc *Impl) GetNetworkInfo(compose *Compose) *NetworkInfo {
	if compose.Networks.Content == nil {
		return nil
	}

	var defaultNetwork string

	for i := 0; i < len(compose.Networks.Content); i += 2 {
		if i+1 >= len(compose.Networks.Content) {
			break
		}
		networkKeyNode := compose.Networks.Content[i]
		networkValueNode := compose.Networks.Content[i+1]

		if defaultNetwork == "" {
			defaultNetwork = networkKeyNode.Value
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
			n := &NetworkInfo{External: true, Name: networkKeyNode.Value}
			if externalName != "" {
				n.Name = externalName
			}
			return n
		}
	}

	if defaultNetwork != "" {
		return &NetworkInfo{External: false, Name: defaultNetwork}
	}

	return nil
}

// GetHostWorkingDirectory Inspects Keploy docker container to get bind mount for current directory
func (idc *Impl) GetHostWorkingDirectory() (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), idc.timeoutForDockerQuery)
	defer cancel()

	curDir, err := os.Getwd()
	if err != nil {
		utils.LogError(idc.logger, err, "failed to get current working directory")
		return "", err
	}

	container, err := idc.ContainerInspect(ctx, "keploy-v2")
	if err != nil {
		utils.LogError(idc.logger, err, "error inspecting keploy-v2 container")
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
	return "", fmt.Errorf("%s", fmt.Sprintf("could not find mount for %s in keploy-v2 container", curDir))
}

// ForceAbsolutePath replaces relative paths in bind mounts with absolute paths
func (idc *Impl) ForceAbsolutePath(c *Compose, basePath string) error {
	hostWorkingDirectory, err := idc.GetHostWorkingDirectory()
	if err != nil {
		return err
	}

	dockerComposeContext, err := filepath.Abs(filepath.Join(hostWorkingDirectory, basePath))
	if err != nil {
		utils.LogError(idc.logger, err, "error getting absolute path for docker compose file")
		return err
	}
	dockerComposeContext = filepath.Dir(dockerComposeContext)
	idc.logger.Debug("docker compose file location in host filesystem", zap.String("dockerComposeContext", dockerComposeContext))

	// Loop through all services in compose file
	for _, service := range c.Services.Content {

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
	return nil
}

// MakeNetworkExternal makes the existing network of the user docker compose file external and save it to a new file
func (idc *Impl) MakeNetworkExternal(c *Compose) error {
	// Iterate over all networks and check the 'external' flag.
	if c.Networks.Content != nil {
		for i := 0; i < len(c.Networks.Content); i += 2 {
			if i+1 >= len(c.Networks.Content) {
				break
			}
			// networkKeyNode := compose.Networks.Content[i]
			networkValueNode := c.Networks.Content[i+1]

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
	return nil
}

// SetKeployNetwork adds the keploy-network network to the new docker compose file and copy rest of the contents from
// existing user docker compose file
func (idc *Impl) SetKeployNetwork(c *Compose) (*NetworkInfo, error) {

	// Ensure that the top-level networks mapping exists.
	if c.Networks.Content == nil {
		c.Networks.Kind = yaml.MappingNode
		c.Networks.Content = make([]*yaml.Node, 0)
	}
	networkInfo := &NetworkInfo{
		Name:     "keploy-network",
		External: true,
	}
	// Check if "keploy-network" already exists
	exists := false
	for i := 0; i < len(c.Networks.Content); i += 2 {
		if c.Networks.Content[i].Value == "keploy-network" {
			exists = true
			break
		}
	}

	if !exists {
		// Add the keploy-network with external: true
		c.Networks.Content = append(c.Networks.Content,
			&yaml.Node{Kind: yaml.ScalarNode, Value: "keploy-network"},
			&yaml.Node{Kind: yaml.MappingNode, Content: []*yaml.Node{
				{Kind: yaml.ScalarNode, Value: "external"},
				{Kind: yaml.ScalarNode, Value: "true"},
			}},
		)
	}

	// Add or modify network for each service
	for _, service := range c.Services.Content {
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
						{Kind: yaml.ScalarNode, Value: "keploy-network"},
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
	return networkInfo, nil
}

// IsContainerRunning check if the container is already running or not, required for docker start command.
func (idc *Impl) IsContainerRunning(containerName string) (bool, error) {

	ctx, cancel := context.WithTimeout(context.Background(), idc.timeoutForDockerQuery)
	defer cancel()

	containerJSON, err := idc.ContainerInspect(ctx, containerName)
	if err != nil {
		return false, err
	}

	if containerJSON.State.Running {
		return true, nil
	}
	return false, nil
}

func (idc *Impl) CreateVolume(ctx context.Context, volumeName string, recreate bool, driverOpts map[string]string) error {
	// Set a timeout for the context
	ctx, cancel := context.WithTimeout(ctx, idc.timeoutForDockerQuery)
	defer cancel()

	// Check if the 'debugfs' volume exists
	filter := filters.NewArgs()
	filter.Add("name", volumeName)
	volumeList, err := idc.VolumeList(ctx, volume.ListOptions{Filters: filter})
	if err != nil {
		idc.logger.Error("failed to list docker volumes", zap.Error(err))
		return err
	}

	if len(volumeList.Volumes) > 0 {
		if !recreate {
			idc.logger.Info("volume already exists", zap.String("volume", volumeName))
			return err
		}

		err := idc.VolumeRemove(ctx, volumeName, false)
		if err != nil {
			idc.logger.Error("failed to delete volume "+volumeName, zap.Error(err))
			return err
		}
	}

	// Create the volume,
	// Create volume with provided driver options or default
	createOptions := volume.CreateOptions{
		Name:   volumeName,
		Driver: "local",
	}

	// If driverOpts is provided and not empty, use them; otherwise use default
	if len(driverOpts) > 0 {
		createOptions.DriverOpts = driverOpts
	}

	_, err = idc.VolumeCreate(ctx, createOptions)
	if err != nil {
		idc.logger.Error("failed to create volume", zap.String("volume", volumeName), zap.Error(err))
		return err
	}

	idc.logger.Debug("volume created", zap.String("volume", volumeName))
	return nil
}

// ServiceConfig represents a service configuration in docker-compose for container searching
type ServiceConfig struct {
	ContainerName string      `yaml:"container_name,omitempty"`
	Networks      interface{} `yaml:"networks,omitempty"`
	Ports         interface{} `yaml:"ports,omitempty"`
}

// FindContainerInComposeFiles searches through multiple Docker Compose files to find a specific container
// and returns the compose file path along with the networks of that service.
// It searches for containers by both explicit container_name and service name.
// This function integrates with the existing Compose structure and reuses existing parsing logic.
func (idc *Impl) FindContainerInComposeFiles(composePaths []string, containerName string) (*ComposeServiceInfo, error) {
	for _, composePath := range composePaths {
		// Use the existing ReadComposeFile method
		compose, err := idc.ReadComposeFile(composePath)
		if err != nil {
			idc.logger.Debug("failed to read compose file, skipping", zap.String("path", composePath), zap.Error(err))
			continue // Skip files that can't be read
		}

		// Search through services using the existing Compose structure
		networks, ports, found := idc.findContainerInServices(compose, containerName)
		if found {
			return &ComposeServiceInfo{
				ComposePath: composePath,
				Networks:    networks,
				Ports:       ports,
				Compose:     compose,
			}, nil
		}
	}

	return nil, fmt.Errorf("container '%s' not found in any of the provided docker-compose files", containerName)
}

// findContainerInServices searches for a container within the services of a compose file
// This reuses the same iteration pattern as existing functions like SetPidContainer
func (idc *Impl) findContainerInServices(compose *Compose, containerName string) ([]string, []string, bool) {
	if compose.Services.Content == nil {
		return nil, nil, false
	}

	// Use the same iteration pattern as existing compose functions (services are key-value pairs)
	for i := 0; i < len(compose.Services.Content); i += 2 {
		if i+1 >= len(compose.Services.Content) {
			break
		}

		serviceNameNode := compose.Services.Content[i]
		serviceContentNode := compose.Services.Content[i+1]
		serviceName := serviceNameNode.Value

		// Check for explicit container_name using the same pattern as existing functions
		var containerNameMatch bool
		for j := 0; j < len(serviceContentNode.Content)-1; j++ {
			if serviceContentNode.Content[j].Kind == yaml.ScalarNode && serviceContentNode.Content[j].Value == "container_name" &&
				serviceContentNode.Content[j+1].Kind == yaml.ScalarNode && serviceContentNode.Content[j+1].Value == containerName {
				containerNameMatch = true
				break
			}
		}

		// If explicit container_name matches or service name matches, extract networks and ports
		if containerNameMatch || serviceName == containerName {
			networks := idc.extractServiceNetworks(serviceContentNode, serviceName)
			ports := idc.extractServicePorts(serviceContentNode)
			return networks, ports, true
		}
	}

	return nil, nil, false
}

// extractServiceNetworks extracts network names from a service's network configuration
func (idc *Impl) extractServiceNetworks(serviceNode *yaml.Node, serviceName string) []string {
	if serviceNode.Content == nil {
		return []string{"default"}
	}

	// Find the networks property using the same pattern as existing functions
	for i := 0; i < len(serviceNode.Content); i += 2 {
		if i+1 >= len(serviceNode.Content) {
			break
		}

		keyNode := serviceNode.Content[i]
		valueNode := serviceNode.Content[i+1]

		if keyNode.Value == "networks" {
			return idc.parseNetworksNode(valueNode)
		}
	}

	// If no networks are specified, the service joins the default network
	return []string{"default"}
}

// extractServicePorts extracts port mappings from a service's port configuration
func (idc *Impl) extractServicePorts(serviceNode *yaml.Node) []string {
	if serviceNode.Content == nil {
		return []string{}
	}

	// Find the ports property using the same pattern as existing functions
	for i := 0; i < len(serviceNode.Content); i += 2 {
		if i+1 >= len(serviceNode.Content) {
			break
		}

		keyNode := serviceNode.Content[i]
		valueNode := serviceNode.Content[i+1]

		if keyNode.Value == "ports" {
			return idc.parsePortsNode(valueNode)
		}
	}

	// If no ports are specified, return empty slice
	return []string{}
}

// parseNetworksNode parses different network configuration formats from yaml.Node
func (idc *Impl) parseNetworksNode(networksNode *yaml.Node) []string {
	var networks []string

	switch networksNode.Kind {
	case yaml.SequenceNode:
		// Array format: networks: [network1, network2]
		for _, networkNode := range networksNode.Content {
			if networkNode.Kind == yaml.ScalarNode {
				networks = append(networks, networkNode.Value)
			}
		}
	case yaml.MappingNode:
		// Extended format: networks: { network1: {}, network2: {} }
		for i := 0; i < len(networksNode.Content); i += 2 {
			keyNode := networksNode.Content[i]
			if keyNode.Kind == yaml.ScalarNode {
				networks = append(networks, keyNode.Value)
			}
		}
	case yaml.ScalarNode:
		// Single network as string
		networks = []string{networksNode.Value}
	}

	// If no networks are specified, use default
	if len(networks) == 0 {
		networks = []string{"default"}
	}

	return networks
}

// parsePortsNode parses different port configuration formats from yaml.Node
func (idc *Impl) parsePortsNode(portsNode *yaml.Node) []string {
	var ports []string

	switch portsNode.Kind {
	case yaml.SequenceNode:
		// Array format: ports: ["80:80", "443:443"] or ports: [8080, "9000:9000"]
		for _, portNode := range portsNode.Content {
			if portNode.Kind == yaml.ScalarNode {
				ports = append(ports, portNode.Value)
			} else if portNode.Kind == yaml.MappingNode {
				// Extended format within array: ports: [{ target: 80, published: 8080 }]
				portMapping := idc.parseExtendedPortMapping(portNode)
				if portMapping != "" {
					ports = append(ports, portMapping)
				}
			}
		}
	case yaml.MappingNode:
		// Extended format: ports: { target: 80, published: 8080 }
		portMapping := idc.parseExtendedPortMapping(portsNode)
		if portMapping != "" {
			ports = []string{portMapping}
		}
	case yaml.ScalarNode:
		// Single port as string: ports: "80:80"
		ports = []string{portsNode.Value}
	}

	return ports
}

// parseExtendedPortMapping parses extended port mapping format { target: 80, published: 8080, protocol: tcp }
func (idc *Impl) parseExtendedPortMapping(portNode *yaml.Node) string {
	var target, published, protocol string

	for i := 0; i < len(portNode.Content); i += 2 {
		if i+1 >= len(portNode.Content) {
			break
		}

		keyNode := portNode.Content[i]
		valueNode := portNode.Content[i+1]

		if keyNode.Kind == yaml.ScalarNode && valueNode.Kind == yaml.ScalarNode {
			switch keyNode.Value {
			case "target":
				target = valueNode.Value
			case "published":
				published = valueNode.Value
			case "protocol":
				protocol = valueNode.Value
			}
		}
	}

	// Build the port mapping string
	if target != "" && published != "" {
		mapping := fmt.Sprintf("%s:%s", published, target)
		if protocol != "" && protocol != "tcp" {
			mapping = fmt.Sprintf("%s/%s", mapping, protocol)
		}
		return mapping
	} else if target != "" {
		// Only target specified (internal port)
		return target
	}

	return ""
}

// FindContainerInComposeCommand is a convenience function that extracts compose file paths from a docker-compose command
// and then searches for a container within those files. This integrates with existing findComposeFile logic.
// Example usage: FindContainerInComposeCommand("docker-compose -f custom.yml up", "my-app")
func (idc *Impl) FindContainerInComposeCommand(dockerComposeCmd, containerName string) (*ComposeServiceInfo, error) {
	composePaths := idc.extractComposeFilesFromCommand(dockerComposeCmd)
	if len(composePaths) == 0 {
		return nil, fmt.Errorf("no docker-compose files found in command: %s", dockerComposeCmd)
	}

	return idc.FindContainerInComposeFiles(composePaths, containerName)
}

// extractComposeFilesFromCommand extracts docker-compose file paths from a docker-compose command
// This integrates with the existing findComposeFile logic from util.go
func (idc *Impl) extractComposeFilesFromCommand(cmd string) []string {
	cmdArgs := strings.Fields(cmd)
	composePaths := []string{}
	haveMultipleComposeFiles := false

	// Look for -f flags in the command
	for i := 0; i < len(cmdArgs); i++ {
		if cmdArgs[i] == "-f" && i+1 < len(cmdArgs) {
			composePaths = append(composePaths, cmdArgs[i+1])
			haveMultipleComposeFiles = true
		}
	}

	if haveMultipleComposeFiles {
		return composePaths
	}

	// If no -f flags found, look for default compose files
	filenames := []string{"docker-compose.yml", "docker-compose.yaml", "compose.yml", "compose.yaml"}

	for _, filename := range filenames {
		if _, err := os.Stat(filename); !os.IsNotExist(err) {
			return []string{filename}
		}
	}

	return []string{}
}

// generateKeployVolumes creates the standard volume mappings for Keploy containers
// This function extracts the common volume logic used by both getAlias and Docker Compose generation
func (idc *Impl) generateKeployVolumes(workingDir, homeDir string) []string {
	osName := runtime.GOOS
	volumes := []string{}

	// Working directory mount
	volumes = append(volumes, fmt.Sprintf("%s:%s", workingDir, workingDir))

	switch osName {
	case "linux":
		// Standard Linux volumes
		volumes = append(volumes,
			"/sys/fs/cgroup:/sys/fs/cgroup",
			"/sys/kernel/debug:/sys/kernel/debug",
			"/sys/fs/bpf:/sys/fs/bpf",
			"/var/run/docker.sock:/var/run/docker.sock",
		)
	case "darwin":
		// macOS volumes
		volumes = append(volumes,
			"/sys/fs/cgroup:/sys/fs/cgroup",
			"/sys/kernel/debug:/sys/kernel/debug",
			"/sys/fs/bpf:/sys/fs/bpf",
			"/var/run/docker.sock:/var/run/docker.sock",
		)
	case "windows":
		// Windows volumes - check if using default context or colima
		cmd := exec.Command("docker", "context", "ls", "--format", "{{.Name}}\t{{.Current}}")
		out, err := cmd.Output()
		if err == nil {
			dockerContext := strings.Split(strings.TrimSpace(string(out)), "\n")[0]
			if dockerContext != "colima" {
				// Default Docker context on Windows
				volumes = append(volumes,
					"/sys/fs/cgroup:/sys/fs/cgroup",
					"debugfs:/sys/kernel/debug:rw",
					"/sys/fs/bpf:/sys/fs/bpf",
					"/var/run/docker.sock:/var/run/docker.sock",
				)
			} else {
				// Colima context
				volumes = append(volumes,
					"/sys/fs/cgroup:/sys/fs/cgroup",
					"/sys/kernel/debug:/sys/kernel/debug",
					"/sys/fs/bpf:/sys/fs/bpf",
					"/var/run/docker.sock:/var/run/docker.sock",
				)
			}
		}
	}

	// Keploy config and data directories
	volumes = append(volumes,
		fmt.Sprintf("%s/.keploy-config:/root/.keploy-config", homeDir),
		fmt.Sprintf("%s/.keploy:/root/.keploy", homeDir),
	)

	return volumes
}

// GenerateKeployAgentService creates a Docker Compose service configuration for keploy-agent
// based on the SetupOptions and returns it as a yaml.Node that can be appended to a compose file
func (idc *Impl) GenerateKeployAgentService(opts models.SetupOptions) (*yaml.Node, error) {
	osName := runtime.GOOS

	// Get working directory and home directory
	workingDir := os.Getenv("PWD")
	if workingDir == "" {
		var err error
		workingDir, err = os.Getwd()
		if err != nil {
			return nil, fmt.Errorf("failed to get working directory: %w", err)
		}
	}

	homeDir := os.Getenv("HOME")
	if osName == "windows" {
		homeDir = os.Getenv("USERPROFILE")
		if homeDir != "" {
			homeDir = strings.ReplaceAll(homeDir, "\\", "/")
		}
		// Convert working directory for Windows
		workingDir = convertPathToUnixStyleForCompose(workingDir)
	}

	// Build the Docker image name
	img := DockerConfig.DockerImage + ":v" + utils.Version

	// Generate environment variables
	envVars := []string{
		"BINARY_TO_DOCKER=true",
	}

	// Add installation ID if available
	if installationID := os.Getenv("INSTALLATION_ID"); installationID != "" {
		envVars = append(envVars, fmt.Sprintf("INSTALLATION_ID=%s", installationID))
	}

	// Generate ports
	ports := []string{
		fmt.Sprintf("%d:%d", opts.AgentPort, opts.AgentPort),
		fmt.Sprintf("%d:%d", opts.ProxyPort, opts.ProxyPort),
	}

	ports = append(ports, opts.AppPorts...)

	// Generate volumes using the extracted function
	volumes := idc.generateKeployVolumes(workingDir, homeDir)

	clientPid := int(os.Getpid())
	fmt.Println("SENDING THIS CLIENT ID : ", clientPid)
	// Build command arguments
	command := []string{
		"--port", fmt.Sprintf("%d", opts.AgentPort),
		"--proxy-port", fmt.Sprintf("%d", opts.ProxyPort),
		"--dns-port", strconv.Itoa(int(opts.DnsPort)),
		"--client-pid", strconv.Itoa(clientPid),
		"--docker-network", opts.DockerNetwork,
		"--agent-ip", opts.AgentIP,
		"--mode", string(opts.Mode),
		"--is-docker",
		"--debug",
	}

	if opts.EnableTesting {
		command = append(command, "--enable-testing")
	}

	// Create the service YAML node structure
	serviceNode := &yaml.Node{
		Kind: yaml.MappingNode,
		Content: []*yaml.Node{
			// image
			{Kind: yaml.ScalarNode, Value: "image"},
			{Kind: yaml.ScalarNode, Value: img},

			// container_name
			{Kind: yaml.ScalarNode, Value: "container_name"},
			{Kind: yaml.ScalarNode, Value: opts.KeployContainer},

			// privileged
			{Kind: yaml.ScalarNode, Value: "privileged"},
			{Kind: yaml.ScalarNode, Value: "true"},

			// working_dir
			{Kind: yaml.ScalarNode, Value: "working_dir"},
			{Kind: yaml.ScalarNode, Value: workingDir},
		},
	}

	// Add environment variables
	if len(envVars) > 0 {
		envNode := &yaml.Node{Kind: yaml.SequenceNode}
		for _, env := range envVars {
			envNode.Content = append(envNode.Content, &yaml.Node{
				Kind: yaml.ScalarNode, Value: env,
			})
		}
		serviceNode.Content = append(serviceNode.Content,
			&yaml.Node{Kind: yaml.ScalarNode, Value: "environment"},
			envNode,
		)
	}

	// Add ports
	if len(ports) > 0 {
		portsNode := &yaml.Node{Kind: yaml.SequenceNode}
		for _, port := range ports {
			portsNode.Content = append(portsNode.Content, &yaml.Node{
				Kind:  yaml.ScalarNode,
				Value: port,
				Style: yaml.DoubleQuotedStyle, // Force double quotes for port strings
			})
		}
		serviceNode.Content = append(serviceNode.Content,
			&yaml.Node{Kind: yaml.ScalarNode, Value: "ports"},
			portsNode,
		)
	}

	// Add networks if specified
	if len(opts.AppNetworks) > 0 {
		networksNode := &yaml.Node{
			Kind: yaml.SequenceNode,
		}
		for _, appNetwork := range opts.AppNetworks {
			networksNode.Content = append(networksNode.Content, &yaml.Node{
				Kind: yaml.ScalarNode, Value: appNetwork,
			})
		}
		serviceNode.Content = append(serviceNode.Content,
			&yaml.Node{Kind: yaml.ScalarNode, Value: "networks"},
			networksNode,
		)
	}

	// Add volumes
	if len(volumes) > 0 {
		volumesNode := &yaml.Node{Kind: yaml.SequenceNode}
		for _, volume := range volumes {
			volumesNode.Content = append(volumesNode.Content, &yaml.Node{
				Kind: yaml.ScalarNode, Value: volume,
			})
		}
		serviceNode.Content = append(serviceNode.Content,
			&yaml.Node{Kind: yaml.ScalarNode, Value: "volumes"},
			volumesNode,
		)
	}

	// Add command
	if len(command) > 0 {
		commandNode := &yaml.Node{Kind: yaml.SequenceNode}
		for _, cmd := range command {
			commandNode.Content = append(commandNode.Content, &yaml.Node{
				Kind:  yaml.ScalarNode,
				Value: cmd,
				Tag:   "!!str", // Explicitly mark as string
			})
		}
		serviceNode.Content = append(serviceNode.Content,
			&yaml.Node{Kind: yaml.ScalarNode, Value: "command"},
			commandNode,
		)
	}

	// Add healthcheck
	healthcheckNode := &yaml.Node{
		Kind: yaml.MappingNode,
		Content: []*yaml.Node{
			// test
			{Kind: yaml.ScalarNode, Value: "test"},
			{Kind: yaml.SequenceNode, Content: []*yaml.Node{
				// {Kind: yaml.ScalarNode, Value: "CMD"},
				// {Kind: yaml.ScalarNode, Value: "curl"},
				// {Kind: yaml.ScalarNode, Value: "-f"},
				// {Kind: yaml.ScalarNode, Value: fmt.Sprintf("http://localhost:%d/agent/health", opts.AgentPort)},
				// {Kind: yaml.ScalarNode, Value: "CMD-SHELL"},
				// {Kind: yaml.ScalarNode, Value: "exit 0"},
				{Kind: yaml.ScalarNode, Value: "CMD-SHELL"},
				{Kind: yaml.ScalarNode, Value: fmt.Sprintf("ss -tuln | grep :%d || exit 1", opts.AgentPort)},
			}},

			// interval
			{Kind: yaml.ScalarNode, Value: "interval"},
			{Kind: yaml.ScalarNode, Value: "5s"},

			// timeout
			{Kind: yaml.ScalarNode, Value: "timeout"},
			{Kind: yaml.ScalarNode, Value: "5s"},

			// retries
			{Kind: yaml.ScalarNode, Value: "retries"},
			{Kind: yaml.ScalarNode, Value: "6"},

			// start_period
			{Kind: yaml.ScalarNode, Value: "start_period"},
			{Kind: yaml.ScalarNode, Value: "30s"},
		},
	}

	serviceNode.Content = append(serviceNode.Content,
		&yaml.Node{Kind: yaml.ScalarNode, Value: "healthcheck"},
		healthcheckNode,
	)

	return serviceNode, nil
}

// convertPathToUnixStyleForCompose converts Windows paths to Unix style for Docker Compose
func convertPathToUnixStyleForCompose(path string) string {
	// Replace backslashes with forward slashes
	unixPath := strings.ReplaceAll(path, "\\", "/")
	// Remove 'C:' and similar drive letters
	if len(unixPath) > 1 && unixPath[1] == ':' {
		unixPath = unixPath[2:]
	}
	return unixPath
}

// AddKeployAgentToCompose adds the keploy-agent service to an existing Docker Compose file
// This is a convenience function that shows how to use GenerateKeployAgentService
func (idc *Impl) AddKeployAgentToCompose(compose *Compose, opts models.SetupOptions) error {
	// Generate the keploy-agent service configuration
	keployServiceNode, err := idc.GenerateKeployAgentService(opts)
	if err != nil {
		return fmt.Errorf("failed to generate keploy-agent service: %w", err)
	}

	// Ensure services section exists
	if compose.Services.Content == nil {
		compose.Services.Kind = yaml.MappingNode
		compose.Services.Content = make([]*yaml.Node, 0)
	}

	// Add the keploy-agent service to the compose file
	compose.Services.Content = append(compose.Services.Content,
		&yaml.Node{Kind: yaml.ScalarNode, Value: "keploy-agent"},
		keployServiceNode,
	)

	return nil
}

// CreateKeployComposeFile creates a complete Docker Compose file with keploy-agent service
// This demonstrates how to create a new compose file from scratch with the keploy-agent
func (idc *Impl) CreateKeployComposeFile(opts models.SetupOptions, version string) (*Compose, error) {
	if version == "" {
		version = "3.9" // Default version
	}

	// Create a new compose structure
	compose := &Compose{
		Version:  version,
		Services: yaml.Node{Kind: yaml.MappingNode, Content: make([]*yaml.Node, 0)},
	}

	// Add keploy-agent service
	err := idc.AddKeployAgentToCompose(compose, opts)
	if err != nil {
		return nil, err
	}

	// If a network is specified, create the networks section
	if opts.AppNetwork != "" {
		compose.Networks = yaml.Node{
			Kind: yaml.MappingNode,
			Content: []*yaml.Node{
				{Kind: yaml.ScalarNode, Value: opts.AppNetwork},
				{Kind: yaml.MappingNode, Content: []*yaml.Node{
					{Kind: yaml.ScalarNode, Value: "external"},
					{Kind: yaml.ScalarNode, Value: "false"},
				}},
			},
		}
	}

	return compose, nil
}

// ModifyComposeForKeployIntegration modifies an existing Docker Compose file to integrate with Keploy agent
// It adds the keploy-agent service and modifies the specified app container to depend on it
func (idc *Impl) ModifyComposeForKeployIntegration(compose *Compose, opts models.SetupOptions, appContainerName string) error {
	// First, add the keploy-agent service
	err := idc.AddKeployAgentToCompose(compose, opts)
	if err != nil {
		return fmt.Errorf("failed to add keploy-agent service: %w", err)
	}

	// Now modify the app container to integrate with keploy-agent
	err = idc.modifyAppServiceForKeploy(compose, appContainerName)
	if err != nil {
		return fmt.Errorf("failed to modify app service: %w", err)
	}

	// Extract the app's original ports and add them to keploy-agent
	// err = idc.moveAppPortsToKeployAgent(compose, appContainerName, opts.KeployContainer)
	// if err != nil {
	// 	return fmt.Errorf("failed to move app ports to keploy-agent: %w", err)
	// }

	return nil
}

// modifyAppServiceForKeploy modifies the app service to depend on keploy-agent and share namespaces
func (idc *Impl) modifyAppServiceForKeploy(compose *Compose, appContainerName string) error {
	if compose.Services.Content == nil {
		return fmt.Errorf("no services found in compose file")
	}

	// Find the app service by container name or service name
	for i := 0; i < len(compose.Services.Content); i += 2 {
		if i+1 >= len(compose.Services.Content) {
			break
		}

		serviceNameNode := compose.Services.Content[i]
		serviceContentNode := compose.Services.Content[i+1]
		serviceName := serviceNameNode.Value

		// Check if this is the target app service
		var isTargetService bool
		for j := 0; j < len(serviceContentNode.Content)-1; j++ {
			if serviceContentNode.Content[j].Kind == yaml.ScalarNode &&
				serviceContentNode.Content[j].Value == "container_name" &&
				serviceContentNode.Content[j+1].Kind == yaml.ScalarNode &&
				serviceContentNode.Content[j+1].Value == appContainerName {
				isTargetService = true
				break
			}
		}

		// If no explicit container_name, check service name
		if !isTargetService && serviceName == appContainerName {
			isTargetService = true
		}

		if isTargetService {
			// Remove networks and ports from the app service
			idc.removeServiceProperty(serviceContentNode, "networks")
			idc.removeServiceProperty(serviceContentNode, "ports")

			// Add or modify depends_on
			idc.addOrUpdateDependsOn(serviceContentNode)

			// Add PID namespace sharing
			idc.addServiceProperty(serviceContentNode, "pid", fmt.Sprintf("service:%s", "keploy-agent"))

			// Add network mode sharing
			idc.addServiceProperty(serviceContentNode, "network_mode", fmt.Sprintf("service:%s", "keploy-agent"))

			break
		}
	}

	return nil
}

// moveAppPortsToKeployAgent extracts ports from app service and adds them to keploy-agent
func (idc *Impl) moveAppPortsToKeployAgent(compose *Compose, appContainerName, keployContainerName string) error {
	var appPorts []string

	// First, extract ports from the app service
	for i := 0; i < len(compose.Services.Content); i += 2 {
		if i+1 >= len(compose.Services.Content) {
			break
		}

		serviceNameNode := compose.Services.Content[i]
		serviceContentNode := compose.Services.Content[i+1]
		serviceName := serviceNameNode.Value

		// Check if this is the target app service
		var isTargetService bool
		for j := 0; j < len(serviceContentNode.Content)-1; j++ {
			if serviceContentNode.Content[j].Kind == yaml.ScalarNode &&
				serviceContentNode.Content[j].Value == "container_name" &&
				serviceContentNode.Content[j+1].Kind == yaml.ScalarNode &&
				serviceContentNode.Content[j+1].Value == appContainerName {
				isTargetService = true
				break
			}
		}

		if !isTargetService && serviceName == appContainerName {
			isTargetService = true
		}

		if isTargetService {
			// Extract ports from this service
			appPorts = idc.extractServicePorts(serviceContentNode)
			break
		}
	}

	// Now add these ports to keploy-agent service
	if len(appPorts) > 0 {
		for i := 0; i < len(compose.Services.Content); i += 2 {
			if i+1 >= len(compose.Services.Content) {
				break
			}

			serviceNameNode := compose.Services.Content[i]
			serviceContentNode := compose.Services.Content[i+1]

			if serviceNameNode.Value == "keploy-agent" {
				idc.addPortsToService(serviceContentNode, appPorts)
				break
			}
		}
	}

	return nil
}

// removeServiceProperty removes a property from a service node
func (idc *Impl) removeServiceProperty(serviceNode *yaml.Node, propertyName string) {
	if serviceNode.Content == nil {
		return
	}

	for i := 0; i < len(serviceNode.Content); i += 2 {
		if i+1 >= len(serviceNode.Content) {
			break
		}

		if serviceNode.Content[i].Kind == yaml.ScalarNode && serviceNode.Content[i].Value == propertyName {
			// Remove both key and value nodes
			serviceNode.Content = append(serviceNode.Content[:i], serviceNode.Content[i+2:]...)
			break
		}
	}
}

// addServiceProperty adds or updates a property in a service node
func (idc *Impl) addServiceProperty(serviceNode *yaml.Node, key, value string) {
	if serviceNode.Content == nil {
		serviceNode.Content = make([]*yaml.Node, 0)
	}

	// Check if property already exists
	for i := 0; i < len(serviceNode.Content); i += 2 {
		if i+1 >= len(serviceNode.Content) {
			break
		}

		if serviceNode.Content[i].Kind == yaml.ScalarNode && serviceNode.Content[i].Value == key {
			// Update existing property
			serviceNode.Content[i+1].Value = value
			return
		}
	}

	// Add new property
	serviceNode.Content = append(serviceNode.Content,
		&yaml.Node{Kind: yaml.ScalarNode, Value: key},
		&yaml.Node{Kind: yaml.ScalarNode, Value: value},
	)
}

// addOrUpdateDependsOn adds or updates the depends_on configuration
func (idc *Impl) addOrUpdateDependsOn(serviceNode *yaml.Node) {
	if serviceNode.Content == nil {
		serviceNode.Content = make([]*yaml.Node, 0)
	}

	// Check if depends_on already exists
	for i := 0; i < len(serviceNode.Content); i += 2 {
		if i+1 >= len(serviceNode.Content) {
			break
		}

		if serviceNode.Content[i].Kind == yaml.ScalarNode && serviceNode.Content[i].Value == "depends_on" {
			// Update existing depends_on
			dependsOnNode := serviceNode.Content[i+1]

			// Check if it's a simple array or extended format
			if dependsOnNode.Kind == yaml.SequenceNode {
				// Store existing dependencies first
				existingDeps := make([]string, 0)
				for _, dep := range dependsOnNode.Content {
					if dep.Kind == yaml.ScalarNode && dep.Value != "keploy-agent" {
						existingDeps = append(existingDeps, dep.Value)
					}
				}

				// Convert to extended format (MappingNode)
				dependsOnNode.Kind = yaml.MappingNode
				dependsOnNode.Tag = ""  // Clear the !!seq tag
				dependsOnNode.Style = 0 // Reset style
				dependsOnNode.Content = []*yaml.Node{}

				// Add keploy-agent first
				dependsOnNode.Content = append(dependsOnNode.Content,
					&yaml.Node{Kind: yaml.ScalarNode, Value: "keploy-agent"},
					&yaml.Node{Kind: yaml.MappingNode, Content: []*yaml.Node{
						{Kind: yaml.ScalarNode, Value: "condition"},
						{Kind: yaml.ScalarNode, Value: "service_healthy"},
					}},
				)

				// Add existing dependencies with service_started condition
				for _, depName := range existingDeps {
					dependsOnNode.Content = append(dependsOnNode.Content,
						&yaml.Node{Kind: yaml.ScalarNode, Value: depName},
						&yaml.Node{Kind: yaml.MappingNode, Content: []*yaml.Node{
							{Kind: yaml.ScalarNode, Value: "condition"},
							{Kind: yaml.ScalarNode, Value: "service_started"},
						}},
					)
				}
			} else if dependsOnNode.Kind == yaml.MappingNode {
				// Add keploy-agent to existing mapping
				keployExists := false
				for j := 0; j < len(dependsOnNode.Content); j += 2 {
					if j < len(dependsOnNode.Content) && dependsOnNode.Content[j].Kind == yaml.ScalarNode && dependsOnNode.Content[j].Value == "keploy-agent" {
						keployExists = true
						break
					}
				}

				if !keployExists {
					dependsOnNode.Content = append([]*yaml.Node{
						{Kind: yaml.ScalarNode, Value: "keploy-agent"},
						{Kind: yaml.MappingNode, Content: []*yaml.Node{
							{Kind: yaml.ScalarNode, Value: "condition"},
							{Kind: yaml.ScalarNode, Value: "service_healthy"},
						}},
					}, dependsOnNode.Content...)
				}
			}
			return
		}
	}

	// Add new depends_on
	serviceNode.Content = append(serviceNode.Content,
		&yaml.Node{Kind: yaml.ScalarNode, Value: "depends_on"},
		&yaml.Node{Kind: yaml.MappingNode, Content: []*yaml.Node{
			{Kind: yaml.ScalarNode, Value: "keploy-agent"},
			{Kind: yaml.MappingNode, Content: []*yaml.Node{
				{Kind: yaml.ScalarNode, Value: "condition"},
				{Kind: yaml.ScalarNode, Value: "service_healthy"},
			}},
		}},
	)
}

// addPortsToService adds ports to an existing service's ports array
func (idc *Impl) addPortsToService(serviceNode *yaml.Node, ports []string) {
	if serviceNode.Content == nil || len(ports) == 0 {
		return
	}

	// Find the ports property
	for i := 0; i < len(serviceNode.Content); i += 2 {
		if i+1 >= len(serviceNode.Content) {
			break
		}

		if serviceNode.Content[i].Kind == yaml.ScalarNode && serviceNode.Content[i].Value == "ports" {
			portsNode := serviceNode.Content[i+1]
			if portsNode.Kind == yaml.SequenceNode {
				// Add new ports to existing ports array
				for _, port := range ports {
					portsNode.Content = append(portsNode.Content, &yaml.Node{
						Kind: yaml.ScalarNode, Value: port,
					})
				}
			}
			return
		}
	}
}
