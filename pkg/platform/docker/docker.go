// Package docker provides functionality for working with Docker containers.
package docker

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/docker/docker/api/types/filters"
	nativeDockerClient "github.com/docker/docker/client"
	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
	"gopkg.in/yaml.v3"

	"github.com/docker/docker/api/types/network"

	"github.com/docker/docker/api/types/volume"
)

const (
	defaultTimeoutForDockerQuery = 1 * time.Minute
)

type Impl struct {
	nativeDockerClient.APIClient
	timeoutForDockerQuery time.Duration
	logger                *zap.Logger
	conf                  *config.Config
}

func New(logger *zap.Logger, c *config.Config) (Client, error) {
	dockerClient, err := nativeDockerClient.NewClientWithOpts(nativeDockerClient.FromEnv,
		nativeDockerClient.WithAPIVersionNegotiation())
	if err != nil {
		return nil, err
	}
	return &Impl{
		APIClient:             dockerClient,
		timeoutForDockerQuery: defaultTimeoutForDockerQuery,
		logger:                logger,
		conf:                  c,
	}, nil
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

// volumeOptionsMatch compares existing volume options with desired options
// Returns true if they match, false otherwise
func (idc *Impl) volumeOptionsMatch(existingOpts, desiredOpts map[string]string) bool {
	// If both are empty or nil, they match
	if len(existingOpts) == 0 && len(desiredOpts) == 0 {
		return true
	}

	// If lengths are different, they don't match
	if len(existingOpts) != len(desiredOpts) {
		return false
	}

	// Compare each key-value pair
	for key, desiredValue := range desiredOpts {
		existingValue, exists := existingOpts[key]
		if !exists || existingValue != desiredValue {
			return false
		}
	}

	return true
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
		// Volume exists, check if it has the same options
		existingVolume := volumeList.Volumes[0]

		// Compare driver options
		if idc.volumeOptionsMatch(existingVolume.Options, driverOpts) {
			idc.logger.Info("volume already exists with the same options", zap.String("volume", volumeName))
			return nil
		}

		if !recreate {
			idc.logger.Info("volume already exists but with different options", zap.String("volume", volumeName))
			return fmt.Errorf("volume %s exists with different options", volumeName)
		}

		idc.logger.Debug("removing existing volume with different options", zap.String("volume", volumeName))
		err := idc.VolumeRemove(ctx, volumeName, false)
		if err != nil {
			idc.logger.Error("failed to remove existing volume", zap.String("volume", volumeName), zap.Error(err))
			cancel()
			return err
		}
		idc.logger.Info("removed existing volume", zap.String("volume", volumeName))
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

// generateKeployVolumes creates the standard volume mappings for Keploy containers
// This function extracts the common volume logic used by both getAlias and Docker Compose generation
func (idc *Impl) generateKeployVolumes() []string {
	osName := runtime.GOOS
	volumes := []string{}

	switch osName {
	case "linux":
		// Standard Linux volumes
		volumes = append(volumes,
			"/sys/fs/cgroup:/sys/fs/cgroup",
			"/sys/kernel/debug:/sys/kernel/debug",
			"/sys/fs/bpf:/sys/fs/bpf",
		)
	case "darwin":
		// macOS volumes
		volumes = append(volumes,
			"/sys/fs/cgroup:/sys/fs/cgroup",
			"/sys/kernel/debug:/sys/kernel/debug",
			"/sys/fs/bpf:/sys/fs/bpf",
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
					"/sys/kernel/debug:/sys/kernel/debug:rw",
					"/sys/fs/bpf:/sys/fs/bpf",
				)
			} else {
				// Colima context
				volumes = append(volumes,
					"/sys/fs/cgroup:/sys/fs/cgroup",
					"/sys/kernel/debug:/sys/kernel/debug",
					"/sys/fs/bpf:/sys/fs/bpf",
				)
			}
		}
	}
	return volumes
}

// GenerateKeployAgentService creates a Docker Compose service configuration for keploy-agent
// based on the SetupOptions and returns it as a yaml.Node that can be appended to a compose file
func (idc *Impl) GenerateKeployAgentService(opts models.SetupOptions) (*yaml.Node, error) {
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
	volumes := idc.generateKeployVolumes()

	clientPid := int(os.Getpid())
	// Build command arguments
	command := []string{
		"--port", fmt.Sprintf("%d", opts.AgentPort),
		"--proxy-port", fmt.Sprintf("%d", opts.ProxyPort),
		"--dns-port", strconv.Itoa(int(opts.DnsPort)),
		"--client-pid", strconv.Itoa(clientPid),
		"--mode", string(opts.Mode),
		"--is-docker",
	}

	if idc.conf.Debug {
		command = append(command, "--debug")
	}
	if opts.EnableTesting {
		command = append(command, "--enable-testing")
	}
	if opts.ConfigPath != "" && opts.ConfigPath != "." {
		command = append(command, "--config-path", opts.ConfigPath)
	}

	if opts.GlobalPassthrough {
		command = append(command, "--global-passthrough")
	}

	if opts.BuildDelay > 0 {
		command = append(command, "--build-delay", strconv.FormatUint(opts.BuildDelay, 10))
	}

	if len(opts.PassThroughPorts) > 0 {
		portStrings := make([]string, len(opts.PassThroughPorts))
		for i, port := range opts.PassThroughPorts {
			portStrings[i] = strconv.Itoa(int(port))
		}
		// Join them with a comma and add as a single argument
		command = append(command, "--pass-through-ports", strings.Join(portStrings, ","))
	}

	idc.logger.Debug("Generating agent service with command", zap.Strings("command", command))

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
				{Kind: yaml.ScalarNode, Value: "CMD-SHELL"},
				{Kind: yaml.ScalarNode, Value: "cat /tmp/agent.ready"},
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
			{Kind: yaml.ScalarNode, Value: "10s"},
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

// ModifyComposeForAgent modifies an existing Docker Compose file to integrate with Keploy agent
// It adds the keploy-agent service and modifies the specified app container to depend on it
func (idc *Impl) ModifyComposeForAgent(compose *Compose, opts models.SetupOptions, appContainerName string) error {
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
