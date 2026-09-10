// Package docker provides functionality for working with Docker containers.
package docker

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
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
	// The name of the volume used to share certs
	KeployTLSVolumeName = "keploy-tls-certs"
	// The path inside the container where certs are mounted
	KeployTLSMountPath = "/tmp/keploy-tls"

	// AgentReadyFile is the readiness marker file written by the agent once
	// setup is complete. Used by the Docker Compose healthcheck and cleared
	// on agent startup to prevent stale state from passing the healthcheck.
	AgentReadyFile = "/tmp/agent.ready"

	defaultTimeoutForDockerQuery = 1 * time.Minute
)

// agentHealthcheckStartPeriod is the docker-compose healthcheck start_period for
// the keploy-agent service: the window in which a FAILING healthcheck neither
// counts toward `retries` nor marks the container unhealthy, so a dependent app
// (depends_on: service_healthy) simply keeps waiting.
//
// WHAT IT IS NOT. For a LITERAL `docker compose` command it is NOT the
// mechanism that detects a dead agent: keploy rewrites such commands to inject
// `--abort-on-container-exit` (ensureComposeExitOnAppFailure, called from
// modifyDockerComposeCommand / ensureInMemoryComposeFlags in pkg/client/app),
// so a crashed or OOM-killed agent container EXITS and aborts the whole run
// immediately, independent of this budget. That covers the go-memory-load lanes
// this fix targets (they run `-c "docker compose up"`). For a WRAPPER command
// (make / npm / a shell script) keploy passes its compose file via COMPOSE_FILE
// and cannot splice the flag in — it warns about exactly this at app.go:~288 —
// so there this window genuinely IS the backstop, which is a further reason to
// keep it generous. In both cases the window's job is the same: avoid
// false-failing an agent that is ALIVE and still doing its one-time setup.
//
// WHAT IT BOUNDS. The agent writes AgentReadyFile only after the CLI has read
// and decoded the test-set's mock corpus (GetFilteredMocks) and streamed it to
// the agent (StoreMocks -> MakeAgentReady). Measured, the stream+park itself is
// not the cost — gob encode/decode + on-disk parking runs at ~380 MB/s, so even
// a multi-hundred-MB corpus lands in seconds (measured with a throwaway
// benchmark, since removed). What actually stretches the
// wall-clock is the CLI-side corpus DECODE racing everything else on a
// contended shared runner: on the go-memory-load lanes a ~80 MB / ~1.1k-mock
// set decoded while the app's own DB seeded on the same 2 vCPUs took ~60s, and
// on a slower run crossed the old fixed 10s+60x5s=310s budget — flipping a
// healthy-but-still-setting-up agent to unhealthy and failing the app's
// depends_on. That is the flake.
//
// WHY GENEROUS IS THE RIGHT SHAPE, NOT A GUESS. The container flips healthy the
// instant the ready file appears, so in the common case the app starts in
// seconds no matter how large this window is — a generous value costs the fast
// path nothing. For literal compose, agent death is caught by container-exit
// (--abort-on-container-exit), not by this window. So this
// is a floor sized to "comfortably longer than any legitimate setup," not a
// delicate estimate of load time; its exact value is not correctness-critical.
// The only case it still bounds is an agent that is alive but wedged (never
// ready, never exits): retries*interval past this window it is declared
// unhealthy and the run fails with a clear cause rather than hanging to the CI
// job timeout. 600s default keeps that backstop while giving the observed
// worst case ~10x headroom.
//
// Overridable via KEPLOY_AGENT_HEALTHCHECK_START_PERIOD_SECONDS. Non-positive or
// unparsable falls back to the default.
func agentHealthcheckStartPeriod() time.Duration {
	const def = 600 * time.Second
	if v := strings.TrimSpace(os.Getenv("KEPLOY_AGENT_HEALTHCHECK_START_PERIOD_SECONDS")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return time.Duration(n) * time.Second
		}
	}
	return def
}

// ComposeServiceHook is called for each keploy-managed Docker Compose service
// node during generation so a downstream caller (the enterprise low-latency hook)
// can mutate the right one. E.g. it adds the deterministic JVM agent (-javaagent
// in JAVA_TOOL_OPTIONS, jar delivered via the shared keploy-tls volume) to the APP
// service, and low-latency caps/tmpfs to the AGENT service.
//
// The identifier passed is what the downstream matches on, NOT the compose map
// key: "keploy-agent" for the agent service, and the caller-supplied
// appContainerName (the --container-name value) for the recorded app service. This
// matters because the app service can be selected by its `container_name` rather
// than its service key, so passing the map key would make the app hook silently
// miss (and Java TLS would go uncaptured) whenever the two differ.
var ComposeServiceHook func(serviceIdentifier string, serviceNode *yaml.Node)

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

	// raw is the original top-level mapping, kept so that a read/modify/write
	// round trip does not silently discard the keys this struct does not name.
	//
	// It matters most for `x-*` extension fields. The Compose spec designates
	// them as the place to put reusable fragments, and the conventional way to
	// reuse one is a YAML anchor:
	//
	//	x-mysql-common: &mysql-common
	//	  healthcheck: {...}
	//	services:
	//	  db:
	//	    <<: *mysql-common
	//
	// Services is a yaml.Node, so it preserves `<<: *mysql-common` verbatim.
	// Without raw, the anchor DEFINITION was dropped while that reference
	// survived, and the compose file keploy generates to run the user's app
	// failed to load with "unknown anchor 'mysql-common' referenced" -- the
	// user's app never started, on a compose file docker itself accepts.
	//
	// Appending the missing keys instead of keeping the original node is not
	// enough: an alias must follow its anchor in the document, and go-yaml
	// emits inline/extra keys after the named struct fields, which puts the
	// definition after the reference. Preserving the original mapping keeps
	// the original order, and with it the comments and any other tooling's
	// top-level keys.
	raw *yaml.Node
}

// UnmarshalYAML captures the original mapping alongside the decoded fields, so
// every path that parses a Compose (file or in-memory) keeps raw without having
// to remember to do it.
func (c *Compose) UnmarshalYAML(value *yaml.Node) error {
	// plain drops the methods, so decoding below does not recurse into this one.
	type plain Compose
	var p plain
	if err := value.Decode(&p); err != nil {
		// Rewrite the shim's name out of the message; a user seeing this is
		// looking at their own malformed compose file, not at keploy internals.
		return fmt.Errorf("%s", strings.ReplaceAll(err.Error(), "docker.plain", "docker.Compose"))
	}
	*c = Compose(p)
	if value.Kind == yaml.MappingNode {
		node := *value
		c.raw = &node
	}
	return nil
}

// MarshalYAML puts the round-trip fix on the TYPE rather than on individual
// call sites, so `yaml.Marshal(compose)` is correct wherever it appears --
// including callers outside this package, such as enterprise's --dump-compose
// hook. Fixing only WriteComposeFile/MarshalCompose would leave the file keploy
// RUNS and the file it hands an operator for debugging disagreeing, in exactly
// the situation this bug shows up in.
// The receiver is a VALUE, not a pointer, on purpose: a pointer-receiver method
// is absent from the method set of a `Compose`, so `yaml.Marshal(*compose)`
// would silently fall back to encoding the struct and emit the same unloadable
// document this fixes. A value receiver is in both method sets, and copying is
// harmless because the result is only ever marshalled.
func (c Compose) MarshalYAML() (interface{}, error) {
	if c.raw == nil {
		// plain sheds the method set; returning c here would re-enter this
		// method and recurse until the stack blows.
		type plain Compose
		return plain(c), nil
	}
	return composeDocument(&c), nil
}

// composeDocument rebuilds the document to serialise: the original mapping with
// the (possibly modified) sections spliced back in. Only ever called with a
// non-nil raw (see MarshalYAML).
//
// The result is for marshalling only. Its top-level node is a copy, but the
// children it did not replace are the SAME pointers as raw's, so mutating the
// returned document would reach back into the Compose.
func composeDocument(compose *Compose) interface{} {
	doc := *compose.raw
	doc.Content = append([]*yaml.Node(nil), compose.raw.Content...)
	// Version is a string rather than a node, so it needs its own splice to be
	// write-through like the other five named fields.
	//
	// Only when it actually CHANGED, though. Splicing unconditionally replaces
	// the original node with a fresh scalar, which drops any line comment on
	// `version:` and re-quotes the value -- making it the one key in the file
	// that loses the fidelity the rest of this function exists to preserve, on
	// every compose that declares a version, to serve a write path no caller
	// currently uses.
	if compose.Version != "" && !hasSectionValue(&doc, "version", compose.Version) {
		setComposeSection(&doc, "version",
			&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: compose.Version})
	}
	for _, section := range []struct {
		key  string
		node *yaml.Node
	}{
		{"services", &compose.Services},
		{"networks", &compose.Networks},
		{"volumes", &compose.Volumes},
		{"configs", &compose.Configs},
		{"secrets", &compose.Secrets},
	} {
		setComposeSection(&doc, section.key, section.node)
	}
	return &doc
}

// hasSectionValue reports whether key already maps to exactly this scalar, so a
// splice that would change nothing can be skipped and the original node kept.
func hasSectionValue(mapping *yaml.Node, key, value string) bool {
	for i := 0; i+1 < len(mapping.Content); i += 2 {
		if mapping.Content[i].Value == key {
			return mapping.Content[i+1].Value == value
		}
	}
	return false
}

// setComposeSection replaces key's value in the mapping, appending the pair when
// the key is absent. A zero node means the section was neither present nor added,
// so it is skipped rather than written out as an explicit null.
func setComposeSection(mapping *yaml.Node, key string, val *yaml.Node) {
	if val == nil || val.Kind == 0 {
		return
	}
	for i := 0; i+1 < len(mapping.Content); i += 2 {
		if mapping.Content[i].Value == key {
			mapping.Content[i+1] = val
			return
		}
	}
	mapping.Content = append(mapping.Content,
		&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key}, val)
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

// MarshalCompose serialises the Compose struct to YAML bytes without writing to disk.
func (idc *Impl) MarshalCompose(compose *Compose) ([]byte, error) {
	data, err := yaml.Marshal(compose)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal compose to YAML: %w", err)
	}
	return data, nil
}

// FindContainerInCompose searches for a container within an already-parsed Compose
// structure. This is the in-memory equivalent of FindContainerInComposeFiles.
func (idc *Impl) FindContainerInCompose(compose *Compose, containerName string) (*ComposeServiceInfo, error) {
	networks, ports, service, found := idc.findContainerInServices(compose, containerName)
	if !found {
		return nil, fmt.Errorf("container '%s' not found in compose", containerName)
	}
	return &ComposeServiceInfo{
		// ComposePath is intentionally empty — content lives in memory.
		Networks:       networks,
		Ports:          ports,
		Compose:        compose,
		AppServiceName: service,
	}, nil
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
			idc.logger.Debug("volume already exists with the same options", zap.String("volume", volumeName))
			return nil
		}

		if !recreate {
			idc.logger.Debug("volume already exists but with different options", zap.String("volume", volumeName))
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
		networks, ports, service, found := idc.findContainerInServices(compose, containerName)
		if found {
			return &ComposeServiceInfo{
				ComposePath:    composePath,
				Networks:       networks,
				Ports:          ports,
				Compose:        compose,
				AppServiceName: service,
			}, nil
		}
	}

	return nil, fmt.Errorf("container '%s' not found in any of the provided docker-compose files", containerName)
}

// findContainerInServices searches for a container within the services of a compose file
// This reuses the same iteration pattern as existing functions like SetPidContainer
func (idc *Impl) findContainerInServices(compose *Compose, containerName string) ([]string, []string, string, bool) {
	// This gate runs BEFORE ModifyComposeForAgent and its ports/networks become
	// opts.AppPorts/AppNetworks. Read off an unresolved alias they come back
	// empty, so the app's published port never reaches keploy-agent -- which
	// publishes on the app's behalf under network_mode: service:keploy-agent --
	// and the app is unreachable from the host.
	resolveServiceAlias(&compose.Services)
	if compose.Services.Content == nil {
		return nil, nil, "", false
	}

	// Use the same iteration pattern as existing compose functions (services are key-value pairs)
	for i := 0; i < len(compose.Services.Content); i += 2 {
		if i+1 >= len(compose.Services.Content) {
			break
		}

		serviceNameNode := compose.Services.Content[i]
		serviceContentNode := resolveServiceAlias(compose.Services.Content[i+1])
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
			return networks, ports, serviceName, true
		}
	}

	return nil, nil, "", false
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
	envVars = append(envVars, fmt.Sprintf("CERT_EXPORT_PATH=%s", KeployTLSMountPath))

	// Add installation ID if available
	if installationID := os.Getenv("INSTALLATION_ID"); installationID != "" {
		envVars = append(envVars, fmt.Sprintf("INSTALLATION_ID=%s", installationID))
	}

	// When the operator's keploy folder is resolved, bind-mount it into
	// the agent container at /keploy-host and point KEPLOY_DEBUG_FILE at
	// a file inside it. The agent process honors that env var (see
	// main.maybeAttachDebugFileSink) and tees its debug-level log
	// records into the file. Because the file lives inside the host
	// keploy folder, downstream tooling — most importantly the
	// `keploy cloud replay` support-bundle pipeline that walks
	// cfg.Path — picks it up automatically without any extra plumbing.
	keployHostPath := strings.TrimSpace(idc.conf.Path)

	// Generate ports
	var ports []string
	if opts.AgentPort != 0 {
		// The agent control-plane HTTP server is unauthenticated (it streams
		// live TLS session keys on /agent/pcap/keylog and accepts
		// unauthenticated /agent/stop and /agent/storemocks). Only the local
		// keploy CLI needs to reach it, so publish it to the host's own
		// loopback rather than every host-network interface.
		ports = append(ports, fmt.Sprintf("127.0.0.1:%d:%d", opts.AgentPort, opts.AgentPort))
	}
	if opts.ProxyPort != 0 {
		ports = append(ports, fmt.Sprintf("%d:%d", opts.ProxyPort, opts.ProxyPort))
	}

	ports = append(ports, opts.AppPorts...)

	// Generate volumes using the extracted function
	volumes := idc.generateKeployVolumes()
	volumes = append(volumes, fmt.Sprintf("%s:%s", KeployTLSVolumeName, KeployTLSMountPath))

	// Bind-mount the host keploy folder + wire KEPLOY_DEBUG_FILE so the
	// agent's debug-level log lands on the host. Skip when the host
	// path is empty (e.g. very early bootstrap before Validate runs)
	// or relative (Docker rejects relative bind-source paths).
	const agentDebugMount = "/keploy-host"
	if keployHostPath != "" && filepath.IsAbs(keployHostPath) {
		volumes = append(volumes, fmt.Sprintf("%s:%s", keployHostPath, agentDebugMount))
		envVars = append(envVars, fmt.Sprintf("KEPLOY_DEBUG_FILE=%s/agent-debug.log", agentDebugMount))
	}

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
	if opts.MockMode {
		// `keploy mock record|replay` — the containerised agent must skip
		// ingress/bind relocation (the wrapped process is a test runner).
		command = append(command, "--mock-mode")
	}
	if idc.conf.Record.Synchronous {
		command = append(command, "--sync")
	}
	// Forward the operator's --disable-mapping (root-level config) into
	// the agent container. The keploy.yml directory isn't bind-mounted
	// into the agent's filesystem, so viper inside the container would
	// otherwise default to true and disable record-side mapping
	// production regardless of the host config.
	command = append(command, fmt.Sprintf("--disable-mapping=%t", idc.conf.DisableMapping))
	if idc.conf.Record.EnableSampling > 0 {
		command = append(command, fmt.Sprintf("--enable-sampling=%d", idc.conf.Record.EnableSampling))
	}
	if opts.EnableTesting {
		command = append(command, "--enable-testing")
	}
	if opts.ConfigPath != "" && opts.ConfigPath != "." {
		command = append(command, "--config-path", opts.ConfigPath)
	}
	if len(opts.ExtraArgs) > 0 {
		command = append(command, opts.ExtraArgs...)
	}

	if opts.GlobalPassthrough {
		command = append(command, "--global-passthrough")
	}
	if opts.CapturePackets {
		command = append(command, "--capture-packets")
	}
	if opts.OpportunisticTLSIntercept {
		command = append(command, "--opportunistic-tls-intercept")
	}
	if opts.ChannelBindingShim {
		command = append(command, "--channel-binding-shim")
	}
	// Upstream TLS verification. Forwarded unconditionally as =%t, matching
	// --disable-mapping above and the native launcher, so that the precedence
	// flag > yaml > default resolves IDENTICALLY here and on a native run —
	// see proxy.resolveUpstreamTLSConfig. The CA path is resolved inside the
	// agent container, so the operator must bind-mount the PEM (or point at a
	// path that already exists in the image); the host's keploy.yml is not
	// visible here either, which is why this travels over argv at all.
	command = append(command, fmt.Sprintf("--upstream-tls-verify=%t", opts.UpstreamTLSVerify))
	command = append(command, fmt.Sprintf("--upstream-tls-ca-cert=%s", opts.UpstreamTLSCACert))

	if opts.BuildDelay > 0 {
		command = append(command, "--build-delay", strconv.FormatUint(opts.BuildDelay, 10))
	}
	if opts.MemoryLimit > 0 {
		command = append(command, "--memory-limit", strconv.FormatUint(opts.MemoryLimit, 10))
	}
	if models.IsAnsiDisabled {
		command = append(command, "--disable-ansi")
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

	capAdd := []*yaml.Node{
		{Kind: yaml.ScalarNode, Value: "BPF"},
		{Kind: yaml.ScalarNode, Value: "PERFMON"},
		{Kind: yaml.ScalarNode, Value: "NET_ADMIN", LineComment: "required for network traffic capture (scoped to container's own namespace)"},
		{Kind: yaml.ScalarNode, Value: "SYS_RESOURCE"},
		{Kind: yaml.ScalarNode, Value: "SYS_PTRACE"},
		{Kind: yaml.ScalarNode, Value: "SYS_NICE"},
	}
	if opts.ChannelBindingShim {
		// The SCRAM-SHA-256-PLUS channel-binding shim rewrites the client's
		// tls-server-end-point digest with bpf_probe_write_user, which the
		// verifier gates behind CAP_SYS_ADMIN — CAP_BPF/CAP_PERFMON alone are
		// insufficient. Only grant it when the shim is explicitly enabled.
		capAdd = appendCapIfMissing(capAdd, "SYS_ADMIN",
			"required by the channel-binding shim (bpf_probe_write_user)")
	}

	// On a host without a cgroup2 (unified) hierarchy (legacy cgroup v1), the
	// agent mounts one itself at runtime for its eBPF cgroup hooks
	// (agent.DetectCgroupPath). mount(2) needs CAP_SYS_ADMIN and an unconfined
	// seccomp/AppArmor profile, so grant them — but only in that case, to keep
	// the agent least-privileged on the common cgroup v2 host.
	needsCgroupV2Mount := !cgroupV2AvailableOnHost()
	if needsCgroupV2Mount {
		capAdd = appendCapIfMissing(capAdd, "SYS_ADMIN",
			"required to mount a cgroup2 hierarchy for eBPF hooks (host uses legacy cgroup v1)")
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

			// cap_add — these privileges are required for eBPF interception.
			{Kind: yaml.ScalarNode, Value: "cap_add", HeadComment: "Capabilities required by keploy-agent for eBPF interception.\n" +
				"Review and allow only what your security policy permits."},
			{Kind: yaml.SequenceNode, Content: capAdd},
		},
	}

	if needsCgroupV2Mount {
		// Docker's default seccomp profile already permits mount(2) once
		// CAP_SYS_ADMIN is granted (the mount rule includes CAP_SYS_ADMIN), so
		// seccomp need not be relaxed. Its default AppArmor profile, however,
		// denies mount outright, so AppArmor must be unconfined for the agent to
		// mount a cgroup2 hierarchy on a legacy cgroup v1 host.
		serviceNode.Content = append(serviceNode.Content,
			&yaml.Node{Kind: yaml.ScalarNode, Value: "security_opt", HeadComment: "Required to mount a cgroup2 hierarchy for eBPF hooks on a legacy cgroup v1 host."},
			&yaml.Node{Kind: yaml.SequenceNode, Content: []*yaml.Node{
				{Kind: yaml.ScalarNode, Value: "apparmor:unconfined"},
			}},
		)
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
	if len(opts.NetworkAliases) > 0 {
		networksMapNode := &yaml.Node{Kind: yaml.MappingNode}

		for netName, aliases := range opts.NetworkAliases {
			// Create the aliases list
			aliasListNode := &yaml.Node{Kind: yaml.SequenceNode}
			for _, alias := range aliases {
				aliasListNode.Content = append(aliasListNode.Content, &yaml.Node{Kind: yaml.ScalarNode, Value: alias})
			}

			// Create the config for this network
			netConfigNode := &yaml.Node{
				Kind: yaml.MappingNode,
				Content: []*yaml.Node{
					{Kind: yaml.ScalarNode, Value: "aliases"},
					aliasListNode,
				},
			}

			// Add to map: networkName -> config
			networksMapNode.Content = append(networksMapNode.Content,
				&yaml.Node{Kind: yaml.ScalarNode, Value: netName},
				netConfigNode,
			)
		}

		serviceNode.Content = append(serviceNode.Content,
			&yaml.Node{Kind: yaml.ScalarNode, Value: "networks"},
			networksMapNode,
		)
	} else if len(opts.AppNetworks) > 0 {
		networksNode := &yaml.Node{Kind: yaml.SequenceNode}
		for _, appNetwork := range opts.AppNetworks {
			networksNode.Content = append(networksNode.Content, &yaml.Node{Kind: yaml.ScalarNode, Value: appNetwork})
		}
		serviceNode.Content = append(serviceNode.Content, &yaml.Node{Kind: yaml.ScalarNode, Value: "networks"}, networksNode)
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

	// Add command ... existed code
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

	// Add healthcheck ... existed code
	healthcheckNode := &yaml.Node{
		Kind: yaml.MappingNode,
		Content: []*yaml.Node{
			// test
			{Kind: yaml.ScalarNode, Value: "test"},
			{Kind: yaml.SequenceNode, Content: []*yaml.Node{
				{Kind: yaml.ScalarNode, Value: "CMD-SHELL"},
				{Kind: yaml.ScalarNode, Value: "cat " + AgentReadyFile},
			}},

			// interval
			{Kind: yaml.ScalarNode, Value: "interval"},
			{Kind: yaml.ScalarNode, Value: "5s"},

			// timeout
			{Kind: yaml.ScalarNode, Value: "timeout"},
			{Kind: yaml.ScalarNode, Value: "5s"},

			// retries
			{Kind: yaml.ScalarNode, Value: "retries"},
			{Kind: yaml.ScalarNode, Value: "60"},

			// start_period — a generous, env-tunable readiness floor. For a
			// literal `docker compose` command it is not the death detector (a
			// dead agent exits and --abort-on-container-exit aborts the run); it
			// only keeps a live, still-setting-up agent from being false-failed
			// while the CLI decodes and streams the mock corpus. See
			// agentHealthcheckStartPeriod for the full rationale.
			{Kind: yaml.ScalarNode, Value: "start_period"},
			{Kind: yaml.ScalarNode, Value: fmt.Sprintf("%ds", int(agentHealthcheckStartPeriod().Seconds()))},
		},
	}

	serviceNode.Content = append(serviceNode.Content,
		&yaml.Node{Kind: yaml.ScalarNode, Value: "healthcheck"},
		healthcheckNode,
	)

	// Inject Pprof Environment Variables and Mounts if enabled on host
	// This logic has been automated to support offline profiling via file dumps.
	var pprofEnv []string
	var pprofMount string

	// Check for CPU_PROFILE
	if cpuProfile := os.Getenv("CPU_PROFILE"); cpuProfile != "" {
		// We want the docker container to write to a specific file that is synced to host.
		// We use a fixed directory /tmp/pprof_output in the container and map PWD to it.
		// For the filename, we prepend "docker-" to avoid conflict if the user runs it in the same dir.
		baseName := filepath.Base(cpuProfile)
		dockerCpuProfile := fmt.Sprintf("/tmp/pprof_output/docker-%s", baseName)
		pprofEnv = append(pprofEnv, fmt.Sprintf("CPU_PROFILE=%s", dockerCpuProfile))
		pprofMount = "/tmp/pprof_output"
	}

	// Check for HEAP_PROFILE
	if heapProfile := os.Getenv("HEAP_PROFILE"); heapProfile != "" {
		baseName := filepath.Base(heapProfile)
		dockerHeapProfile := fmt.Sprintf("/tmp/pprof_output/docker-%s", baseName)
		pprofEnv = append(pprofEnv, fmt.Sprintf("HEAP_PROFILE=%s", dockerHeapProfile))
		pprofMount = "/tmp/pprof_output"
	}

	// If either profiling is enabled, updated environment and volumes
	if len(pprofEnv) > 0 {
		// 1. Append Env Vars
		// Find environment node
		var envNode *yaml.Node
		for i := 0; i < len(serviceNode.Content); i += 2 {
			if serviceNode.Content[i].Value == "environment" {
				envNode = serviceNode.Content[i+1]
				break
			}
		}
		// If environment node doesn't exist (unlikely given previous logic, but safe to check), create it?
		// The previous logic guarantees environment node creation because BINARY_TO_DOCKER is always added.
		if envNode != nil {
			for _, env := range pprofEnv {
				envNode.Content = append(envNode.Content, &yaml.Node{
					Kind: yaml.ScalarNode, Value: env,
				})
			}
		}

		// 2. Append Volume
		// Mount current working directory to /tmp/pprof_output
		cwd, err := os.Getwd()
		if err == nil && pprofMount != "" {
			mount := fmt.Sprintf("%s:%s", cwd, pprofMount)
			// Find volume node
			var volNode *yaml.Node
			for i := 0; i < len(serviceNode.Content); i += 2 {
				if serviceNode.Content[i].Value == "volumes" {
					volNode = serviceNode.Content[i+1]
					break
				}
			}
			// Volumes node also guaranteed to exist
			if volNode != nil {
				volNode.Content = append(volNode.Content, &yaml.Node{
					Kind: yaml.ScalarNode, Value: mount,
				})
			}
		} else {
			idc.logger.Debug("Failed to get current working directory for pprof mount", zap.Error(err))
		}
	}

	// Allow callers to mutate the fully-built service node. This runs last
	// so the hook can see and modify all fields including volumes.
	if ComposeServiceHook != nil {
		ComposeServiceHook("keploy-agent", serviceNode)
	}

	return serviceNode, nil
}

// AddKeployAgentToCompose adds the keploy-agent service to an existing Docker Compose file
// This is a convenience function that shows how to use GenerateKeployAgentService
func (idc *Impl) AddKeployAgentToCompose(compose *Compose, opts models.SetupOptions) error {
	// Ensure services section exists. Resolve as well as normalise: an aliased
	// `services:` would otherwise take the append into a node that is never
	// emitted, and this must not depend on findServiceNodeAndName happening to
	// run first.
	ensureMapping(&compose.Services)
	resolveServiceAlias(&compose.Services)

	// A section that STILL cannot hold entries -- an alias to a scalar or a
	// sequence -- would swallow the append and leave this returning nil with the
	// agent service simply absent from the generated file.
	if compose.Services.Kind != yaml.MappingNode {
		return fmt.Errorf("`services:` is not a mapping (%s), so keploy cannot add "+
			"its agent service to it", yamlKindName(compose.Services.Kind))
	}

	// Refuse to add a second keploy-agent, by service KEY or by container_name.
	// A user service by that name is unusual but legal, and appending beside it
	// produced a generated file that does not parse at all -- `mapping key
	// "keploy-agent" already defined` -- reading as a keploy bug rather than the
	// name collision it is. The container_name variant is worse than a duplicate
	// key: findServiceNodeAndName matches on container_name too, so keploy would
	// treat the USER's service as its agent and move the app's dns onto it, and
	// compose rejects the file with `container name "keploy-agent" is already in
	// use`.
	//
	// Checked before GenerateKeployAgentService so a doomed run does not first
	// fire the enterprise compose hook.
	for i := 0; i+1 < len(compose.Services.Content); i += 2 {
		if compose.Services.Content[i].Value == "keploy-agent" {
			return fmt.Errorf("the compose file already defines a service named " +
				"\"keploy-agent\"; rename it so keploy can add its own")
		}
		svc := compose.Services.Content[i+1]
		for j := 0; j+1 < len(svc.Content); j += 2 {
			if svc.Content[j].Value == "container_name" &&
				svc.Content[j+1].Value == "keploy-agent" {
				return fmt.Errorf("service %q already uses container_name "+
					"\"keploy-agent\"; rename it so keploy can add its own",
					compose.Services.Content[i].Value)
			}
		}
	}

	// Generate the keploy-agent service configuration
	keployServiceNode, err := idc.GenerateKeployAgentService(opts)
	if err != nil {
		return fmt.Errorf("failed to generate keploy-agent service: %w", err)
	}

	// Add the keploy-agent service to the compose file
	compose.Services.Content = append(compose.Services.Content,
		&yaml.Node{Kind: yaml.ScalarNode, Value: "keploy-agent"},
		keployServiceNode,
	)

	return nil
}

// Helper: findServiceNodeAndName finds the YAML node and the Service Key (name)
func (idc *Impl) findServiceNodeAndName(compose *Compose, appIdentifier string) (*yaml.Node, string, error) {
	// `services: *svcs` is legal compose, and an alias holds no entries of its
	// own, so the scan below saw nothing and reported "no services found".
	resolveServiceAlias(&compose.Services)
	if compose.Services.Content == nil {
		return nil, "", fmt.Errorf("no services found")
	}

	for i := 0; i < len(compose.Services.Content); i += 2 {
		if i+1 >= len(compose.Services.Content) {
			break // odd Content: guarded like the sibling loops rather than panicking
		}
		serviceNameNode := compose.Services.Content[i]
		// Resolved at the loop top, not at either return, so the container_name
		// match below scans real content instead of an alias's empty Content.
		serviceContentNode := resolveServiceAlias(compose.Services.Content[i+1])
		serviceName := serviceNameNode.Value

		// Check 1: Does the Service Key match?
		if serviceName == appIdentifier {
			return serviceContentNode, serviceName, nil
		}

		// Check 2: Does the container_name match?
		for j := 0; j < len(serviceContentNode.Content)-1; j += 2 {
			if serviceContentNode.Content[j].Value == "container_name" {
				if serviceContentNode.Content[j+1].Value == appIdentifier {
					return serviceContentNode, serviceName, nil
				}
			}
		}
	}
	return nil, "", fmt.Errorf("service not found")
}

// ModifyComposeForAgent modifies an existing Docker Compose file to integrate with Keploy agent
// It adds the keploy-agent service and modifies the specified app container to depend on it
func (idc *Impl) ModifyComposeForAgent(compose *Compose, opts models.SetupOptions, appContainerName string) error {

	targetServiceNode, serviceName, err := idc.findServiceNodeAndName(compose, appContainerName)
	if err != nil {
		return fmt.Errorf("failed to find target service '%s': %w", appContainerName, err)
	}

	existingNetworks := idc.extractServiceNetworks(targetServiceNode, serviceName)
	if len(existingNetworks) == 0 {
		existingNetworks = []string{"default"}
	}

	if opts.NetworkAliases == nil {
		opts.NetworkAliases = make(map[string][]string)
	}

	for _, net := range existingNetworks {
		opts.NetworkAliases[net] = []string{serviceName}
	}
	// First, add the keploy-agent service
	err = idc.AddKeployAgentToCompose(compose, opts)
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

	keployServiceNode, _, err := idc.findServiceNodeAndName(compose, "keploy-agent")
	if err != nil {
		return fmt.Errorf("keploy-agent service not found: %w", err)
	}

	// Find the app service by container name or service name
	for i := 0; i < len(compose.Services.Content); i += 2 {
		if i+1 >= len(compose.Services.Content) {
			break
		}

		serviceNameNode := compose.Services.Content[i]
		serviceContentNode := resolveServiceAlias(compose.Services.Content[i+1])
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
			// The app will share the keploy-agent network namespace, so resolver
			// settings must move to keploy-agent. Leaving them on the app either
			// makes Compose reject the config (dns + network_mode conflict) or
			// causes the namespace owner to use the wrong /etc/resolv.conf.
			for _, key := range []string{"dns", "dns_search", "dns_opt"} {
				if valueNode := idc.getServiceProperty(serviceContentNode, key); valueNode != nil {
					idc.setServicePropertyNode(keployServiceNode, key, cloneYAMLNode(valueNode))
					idc.removeServiceProperty(serviceContentNode, key)
				}
			}

			// Remove networks and ports from the app service
			idc.removeServiceProperty(serviceContentNode, "networks")
			idc.removeServiceProperty(serviceContentNode, "ports")

			// Add or modify depends_on
			idc.addOrUpdateDependsOn(serviceContentNode)
			idc.addServiceListProperty(serviceContentNode, "volumes", fmt.Sprintf("%s:%s:ro", KeployTLSVolumeName, KeployTLSMountPath))
			certPath := fmt.Sprintf("%s/ca.crt", KeployTLSMountPath)
			trustStorePath := fmt.Sprintf("%s/truststore.jks", KeployTLSMountPath)
			idc.addServiceEnvVar(serviceContentNode, "NODE_EXTRA_CA_CERTS", certPath)
			idc.addServiceEnvVar(serviceContentNode, "REQUESTS_CA_BUNDLE", certPath)
			idc.addServiceEnvVar(serviceContentNode, "SSL_CERT_FILE", certPath)
			idc.addServiceEnvVar(serviceContentNode, "CARGO_HTTP_CAINFO", certPath)

			javaOpts := fmt.Sprintf("-Djavax.net.ssl.trustStore=%s -Djavax.net.ssl.trustStorePassword=changeit", trustStorePath)
			idc.appendServiceEnvVar(serviceContentNode, "JAVA_TOOL_OPTIONS", javaOpts)
			// Add PID namespace sharing
			idc.addServiceProperty(serviceContentNode, "pid", fmt.Sprintf("service:%s", "keploy-agent"))

			// Add network mode sharing
			idc.addServiceProperty(serviceContentNode, "network_mode", fmt.Sprintf("service:%s", "keploy-agent"))

			// Let a downstream caller mutate the APP service too — the enterprise
			// low-latency hook uses this to append the deterministic JVM agent
			// (-javaagent in JAVA_TOOL_OPTIONS). The app shares keploy-agent's PID
			// AND network namespace (set above), so the JVM reaches the JSSE
			// listener on 127.0.0.1 and its PID is directly resolvable — no jattach.
			// Pass appContainerName (the --container-name value the downstream
			// matches on), NOT serviceName (the compose map key): this service may
			// have been selected by its container_name, in which case the key differs
			// and the app hook would silently miss.
			if ComposeServiceHook != nil {
				ComposeServiceHook(appContainerName, serviceContentNode)
			}

			break
		}
	}
	idc.addTopLevelVolume(compose, KeployTLSVolumeName)
	return nil
}

// Helper to add a list item (like volumes) to a service
func (idc *Impl) addServiceListProperty(serviceNode *yaml.Node, key, value string) {
	var valueNode *yaml.Node

	// Check if key exists
	for i := 0; i+1 < len(serviceNode.Content); i += 2 {
		if serviceNode.Content[i].Value == key {
			// An aliased list (`volumes: *appvols`) holds no entries, so the
			// append below would land in a node the encoder never emits and the
			// TLS-cert mount would simply not appear.
			valueNode = resolveAliasByCopy(serviceNode.Content[i+1])
			break
		}
	}

	// If not found, create it
	if valueNode == nil {
		valueNode = &yaml.Node{Kind: yaml.SequenceNode}
		serviceNode.Content = append(serviceNode.Content,
			&yaml.Node{Kind: yaml.ScalarNode, Value: key},
			valueNode,
		)
	}

	// Append the value
	valueNode.Content = append(valueNode.Content, &yaml.Node{Kind: yaml.ScalarNode, Value: value})
}

// getOrCreateEnvNode finds the 'environment' node inside a service, creating
// a SequenceNode if none exists. It centralizes the lookup/create logic for
// environment mutations performed by helper methods.
func (idc *Impl) getOrCreateEnvNode(serviceNode *yaml.Node) *yaml.Node {
	for i := 0; i+1 < len(serviceNode.Content); i += 2 {
		if serviceNode.Content[i].Value == "environment" {
			// `environment: *appenv` is an alias: it holds no entries, so the
			// callers' type switch matches nothing and keploy's variables are
			// silently dropped. Resolving by copy also keeps a sibling service
			// sharing that fragment from inheriting keploy's CA paths while
			// living outside the agent's network namespace.
			return resolveAliasByCopy(serviceNode.Content[i+1])
		}
	}

	// Not found — create it.
	envNode := &yaml.Node{Kind: yaml.SequenceNode}
	serviceNode.Content = append(serviceNode.Content,
		&yaml.Node{Kind: yaml.ScalarNode, Value: "environment"},
		envNode,
	)
	return envNode
}

// Helper to add/update an environment variable.
// If the key already exists, its value is replaced (upsert semantics).
func (idc *Impl) addServiceEnvVar(serviceNode *yaml.Node, envKey, envValue string) {
	envNode := idc.getOrCreateEnvNode(serviceNode)

	// Handle Sequence (array) style environment: ["KEY=VAL"]
	if envNode.Kind == yaml.SequenceNode {
		prefix := envKey + "="
		for _, node := range envNode.Content {
			if strings.HasPrefix(node.Value, prefix) {
				// Key already exists — update in place.
				node.Value = fmt.Sprintf("%s=%s", envKey, envValue)
				return
			}
		}
		envNode.Content = append(envNode.Content, &yaml.Node{
			Kind:  yaml.ScalarNode,
			Value: fmt.Sprintf("%s=%s", envKey, envValue),
		})
		return
	}
	// Handle Mapping (dict) style environment: { KEY: VAL }
	if envNode.Kind == yaml.MappingNode {
		for i := 0; i < len(envNode.Content)-1; i += 2 {
			if envNode.Content[i].Value == envKey {
				// Key already exists — update in place.
				envNode.Content[i+1].Value = envValue
				return
			}
		}
		envNode.Content = append(envNode.Content,
			&yaml.Node{Kind: yaml.ScalarNode, Value: envKey},
			&yaml.Node{Kind: yaml.ScalarNode, Value: envValue},
		)
	}
}

// Helper to append to an environment variable (for JAVA_TOOL_OPTIONS).
// If the key already exists, the new value is appended (space-separated).
// If it does not exist, a new entry is created.
func (idc *Impl) appendServiceEnvVar(serviceNode *yaml.Node, envKey, appendValue string) {
	envNode := idc.getOrCreateEnvNode(serviceNode)

	// Handle Sequence (array) style: ["KEY=VAL", ...]
	if envNode.Kind == yaml.SequenceNode {
		prefix := envKey + "="
		for _, node := range envNode.Content {
			if strings.HasPrefix(node.Value, prefix) {
				existingVal := strings.TrimPrefix(node.Value, prefix)
				existingTokens := strings.Fields(existingVal)
				newTokens := strings.Fields(appendValue)
				var missing []string
				for _, t := range newTokens {
					if !slices.Contains(existingTokens, t) {
						missing = append(missing, t)
					}
				}
				if len(missing) == 0 {
					// All tokens already present — skip to avoid double-injection.
					return
				}
				node.Value = node.Value + " " + strings.Join(missing, " ")
				return
			}
		}
		// Key not found — add new entry.
		idc.addServiceEnvVar(serviceNode, envKey, appendValue)
		return
	}

	// Handle Mapping (dict) style: { KEY: VAL }
	if envNode.Kind == yaml.MappingNode {
		for i := 0; i < len(envNode.Content)-1; i += 2 {
			if envNode.Content[i].Value == envKey {
				existingVal := envNode.Content[i+1].Value
				existingTokens := strings.Fields(existingVal)
				newTokens := strings.Fields(appendValue)
				var missing []string
				for _, t := range newTokens {
					if !slices.Contains(existingTokens, t) {
						missing = append(missing, t)
					}
				}
				if len(missing) == 0 {
					// All tokens already present — skip to avoid double-injection.
					return
				}
				envNode.Content[i+1].Value = existingVal + " " + strings.Join(missing, " ")
				return
			}
		}
		// Key not found — add new entry.
		idc.addServiceEnvVar(serviceNode, envKey, appendValue)
		return
	}
}

// Helper to ensure top-level volumes exists
func (idc *Impl) addTopLevelVolume(compose *Compose, volumeName string) {
	vols := sectionForAppend(&compose.Volumes)
	if vols.Kind != yaml.MappingNode {
		// Reached in three shapes: a populated scalar, a populated sequence, or
		// an ALIAS whose target is one of those (kind 16 in the log below, not
		// the scalar/sequence a reader might expect). None can hold entries, and
		// none is reshaped -- rewriting a fragment the user authored to suit us
		// is worse than declining.
		//
		// The append cannot land, so the generated file declares no volume while
		// the agent service mounts one, and docker compose rejects it with
		// "refers to undefined volume". That error names the agent, not the
		// user's `volumes:` line, so say here what actually has to change.
		idc.logger.Warn("top-level `volumes:` cannot hold a volume entry, so keploy "+
			"cannot declare its own there and docker compose will reject the "+
			"generated file; `volumes:` must be a mapping (or an anchor naming one)",
			zap.String("volume", volumeName),
			zap.String("volumesShape", yamlKindName(compose.Volumes.Kind)),
			zap.String("resolvesTo", yamlKindName(resolvedKind(&compose.Volumes))))
	}

	// Check if volume exists
	for i := 0; i < len(vols.Content); i += 2 {
		if vols.Content[i].Value == volumeName {
			return // Already exists
		}
	}

	// Add it (empty content is fine for local driver)
	vols.Content = append(vols.Content,
		&yaml.Node{Kind: yaml.ScalarNode, Value: volumeName},
		&yaml.Node{Kind: yaml.MappingNode, Content: []*yaml.Node{}}, // {}
	)
}

// yamlKindName renders a yaml.Kind for a log line. The raw enum means nothing to
// an operator reading it.
func yamlKindName(k yaml.Kind) string {
	switch k {
	case 0:
		return "absent"
	case yaml.DocumentNode:
		return "document"
	case yaml.SequenceNode:
		return "sequence"
	case yaml.MappingNode:
		return "mapping"
	case yaml.ScalarNode:
		return "scalar"
	case yaml.AliasNode:
		return "alias"
	}
	return "unknown"
}

// resolvedKind reports what a node ultimately names, following one alias hop, so
// a diagnostic can point at the anchor the user has to fix rather than at the
// `volumes:` line that merely references it.
func resolvedKind(n *yaml.Node) yaml.Kind {
	if n == nil {
		return 0
	}
	if n.Kind == yaml.AliasNode && n.Alias != nil {
		return n.Alias.Kind
	}
	return n.Kind
}

// resolveServiceAlias replaces an alias node with a DEEP copy of what it names,
// so the resolved node can be edited without reaching back into the shared
// fragment.
//
// sectionForAppend's shallow copy is right for `volumes:`/`networks:`, whose
// entries are inert and only ever appended to. It is wrong for a SERVICE,
// because keploy does not only append to one: modifyAppServiceForKeploy DELETES
// keys (networks, ports, dns/dns_search/dns_opt) and OVERWRITES values in place
// (addServiceEnvVar), and enterprise's ComposeServiceHook rewrites
// JAVA_TOOL_OPTIONS unconditionally on every replay.
//
// With children shared, a sibling service aliasing the same fragment inherits
// all of it -- including a TLS environment pointing SSL_CERT_FILE,
// REQUESTS_CA_BUNDLE and NODE_EXTRA_CA_CERTS at keploy's CA, inside a container
// that is NOT in the agent's network namespace, so its outbound TLS fails
// verification. Silently. A mapping-style `environment:` is worse: the user's
// own value is overwritten inside their own fragment.
//
// Anchors are stripped from the copy. Keeping them would emit a SECOND
// definition of the same anchor, and because this copy is about to be mutated
// the two would diverge -- a later `*name` or `<<: *name` binds to the nearest
// preceding definition, so an unrelated service would silently pick up keploy's
// edits. Stripping leaves the user's fragment as the only definition, which is
// what every other reference should still resolve to.
//
// Deliberately unlike sectionForAppend, which KEEPS the anchor: a copied volumes
// fragment is only ever appended to, so a duplicate definition cannot diverge
// from the original. A copied service is edited, so it can.
func resolveServiceAlias(n *yaml.Node) *yaml.Node {
	if n != nil && n.Kind == yaml.AliasNode && n.Alias != nil &&
		n.Alias.Kind != yaml.MappingNode {
		// A service must be a mapping. Leave anything else exactly as written
		// and let the caller report it.
		return n
	}
	return resolveAliasByCopy(n)
}

// resolveAliasByCopy is resolveServiceAlias without the mapping-only
// restriction, for the nodes INSIDE a service.
//
// `environment:` and `volumes:` are aliased at least as often as a whole
// service, and keploy writes into both. Unresolved, the two helpers below fail
// in opposite directions and neither says anything: addServiceEnvVar's type
// switch matches neither SequenceNode nor MappingNode for an AliasNode and falls
// straight through, so keploy's CA and JAVA_TOOL_OPTIONS are dropped and the app
// runs uninstrumented; addServiceListProperty appends into the alias, so the
// TLS-cert mount never appears. Both leave a file docker compose happily accepts.
//
// A sequence target is allowed here because `environment: - K=V` and
// `volumes: - a:b` are the ordinary shapes.
func resolveAliasByCopy(n *yaml.Node) *yaml.Node {
	if n == nil || n.Kind != yaml.AliasNode || n.Alias == nil {
		return n
	}
	if n.Alias.Kind != yaml.MappingNode && n.Alias.Kind != yaml.SequenceNode {
		// Cannot hold entries. Left exactly as the user wrote it rather than
		// reshaping a fragment they authored; the caller reports it.
		return n
	}
	clone := cloneYAMLNode(n.Alias)
	stripAnchors(clone)
	*n = *clone
	return n
}

// stripAnchors clears anchor names throughout a subtree. See resolveServiceAlias
// for why a copy must not carry them.
func stripAnchors(n *yaml.Node) {
	if n == nil {
		return
	}
	n.Anchor = ""
	for _, c := range n.Content {
		stripAnchors(c)
	}
}

// sectionForAppend returns the mapping a section's entries must actually be
// appended to, normalising an empty section and resolving an alias.
//
// `volumes: *vols` is legal compose and is the same `x-*` + anchor idiom this
// file works to preserve elsewhere, but an alias node is a REFERENCE -- it holds
// no entries of its own, so appending to it writes into a node the encoder never
// emits. The generated file then declared no volume while the agent service
// mounted one, and docker compose rejected it.
//
// The fragment is RESOLVED BY COPY, not by reference. Appending into the
// anchored node directly is the obvious move and it is wrong: an `x-*` fragment
// is shared, so every other alias to it silently inherits keploy's volume.
// Reproduced, on a file docker compose accepts:
//
//	x-common: &common
//	  shared: {}
//	volumes: *common
//	networks: *common
//
// Appending in place gives the user a `keploy-tls-certs` NETWORK. Corrupting an
// unrelated part of their file to fix our own is a bad trade, and it leaves no
// trace. It can also produce a file that no longer loads at all: when the shared
// fragment is a sequence aliased by a service's `volumes:` list, reshaping it to
// a mapping makes compose reject that service.
//
// Copying costs the alias on this one key in the GENERATED file: `volumes:` is
// emitted expanded rather than as `*vols`. The anchor and every other reference
// to it are untouched, the resolved volume set is identical, and the file is
// transient -- keploy writes it to run the app, it is not the user's source.
//
// One consequence worth naming, since a MISSING anchor definition is what this
// file's other half exists to prevent: an anchor nested INSIDE the fragment is
// now emitted twice, once in the fragment and once in the copy. That is safe
// rather than merely tolerated -- the copy shares the same child nodes, so the
// two definitions cannot diverge -- and the file still loads.
func sectionForAppend(n *yaml.Node) *yaml.Node {
	if n == nil {
		return nil
	}
	ensureMapping(n)
	if n.Kind != yaml.AliasNode || n.Alias == nil {
		return n
	}
	target := n.Alias
	if target.Kind != yaml.MappingNode && !isEmptySection(target) {
		// Resolves to something that cannot hold entries (a populated scalar or
		// sequence). Leave the alias exactly as written -- the caller warns --
		// rather than reshaping a fragment the user wrote.
		return n
	}
	// Replace the alias with a copy of what it named.
	//
	// The slice is CLONED rather than shared, and that is load-bearing, not
	// defensive: a mapping node's Content grows two entries per key, so a
	// three-key fragment sits at len 6 / cap 8 -- exactly the two spare slots
	// one append consumes. Sharing the slice would write keploy's entry into
	// the user's fragment backing array without reallocating, and a second
	// section resolved through the same fragment would then overwrite the
	// first's entry instead of appending after it.
	//
	// Child NODES are still shared, which is safe: every writer here appends,
	// and nothing mutates an existing volume entry in place (checked in OSS and
	// in the enterprise BeforeDockerComposeSetup hook).
	replaced := yaml.Node{Kind: yaml.MappingNode}
	if target.Kind == yaml.MappingNode {
		replaced.Content = append([]*yaml.Node(nil), target.Content...)
	}
	*n = replaced
	return n
}

// ensureMapping turns an EMPTY section node into an empty mapping so callers can
// append key/value pairs to it.
//
// A section reaches us in several empty shapes and only two used to be handled,
// each at a different call site with a different predicate. An absent key
// decodes to a zero node; an explicit `volumes:` with nothing under it decodes
// to a null SCALAR; `volumes: []` decodes to an empty sequence; `volumes: ""` to
// an empty string. Appending to any of the latter three writes into a node the
// encoder will not emit as a mapping, so the generated compose declared nothing
// while keploy's agent service referenced it, and docker compose refused the
// file with "volumes must be a mapping" -- on a compose file it otherwise
// accepts. Normalising on the target shape closes all of them at once instead of
// enumerating spellings.
//
// A POPULATED non-mapping is deliberately left alone. Reshaping it would
// silently discard what the user wrote, and docker compose's own error is a far
// better outcome than losing their data.
//
// The tag is cleared because a MappingNode still carrying `!!null` emits
// `volumes: !!null` above the entries. Both docker compose and go-yaml accept
// that, so it is cosmetic -- but it is a stray null tag in a file operators are
// handed to debug. Value is deliberately NOT cleared: the encoder never reads it
// for a mapping, so clearing it would be an unobservable no-op.
func ensureMapping(n *yaml.Node) {
	if n == nil || n.Kind == yaml.MappingNode || !isEmptySection(n) {
		return
	}
	n.Kind = yaml.MappingNode
	n.Tag = ""
	// Style carries the flow/quote bits of the shape being replaced, so an
	// empty `volumes: []` would otherwise normalise into a flow-style mapping
	// sitting in an otherwise block-style file. Valid either way; consistent
	// reads better.
	n.Style = 0
	n.Content = []*yaml.Node{}
}

// isEmptySection reports whether a section node holds nothing, in any of the
// shapes YAML can express that: an absent key (zero node), a null in any
// spelling, an empty string, or an empty sequence.
//
// A node carrying content is NOT empty, and that includes shapes whose payload
// does not live in Content: a scalar keeps its text in Value, and an alias
// resolves elsewhere entirely. Treating those as empty would silently discard
// what the user wrote.
func isEmptySection(n *yaml.Node) bool {
	switch n.Kind {
	case 0:
		return true
	case yaml.ScalarNode:
		return n.Tag == "!!null" || n.Value == ""
	case yaml.SequenceNode:
		return len(n.Content) == 0
	default:
		return false
	}
}

func cloneYAMLNode(node *yaml.Node) *yaml.Node {
	if node == nil {
		return nil
	}

	cloned := *node
	if node.Content != nil {
		cloned.Content = make([]*yaml.Node, len(node.Content))
		for i, child := range node.Content {
			cloned.Content[i] = cloneYAMLNode(child)
		}
	}

	return &cloned
}

func (idc *Impl) getServiceProperty(serviceNode *yaml.Node, propertyName string) *yaml.Node {
	if serviceNode == nil || serviceNode.Content == nil {
		return nil
	}

	for i := 0; i+1 < len(serviceNode.Content); i += 2 {
		if serviceNode.Content[i].Kind == yaml.ScalarNode && serviceNode.Content[i].Value == propertyName {
			return serviceNode.Content[i+1]
		}
	}

	return nil
}

func (idc *Impl) setServicePropertyNode(serviceNode *yaml.Node, key string, valueNode *yaml.Node) {
	if serviceNode.Content == nil {
		serviceNode.Content = make([]*yaml.Node, 0)
	}

	for i := 0; i+1 < len(serviceNode.Content); i += 2 {
		if serviceNode.Content[i].Kind == yaml.ScalarNode && serviceNode.Content[i].Value == key {
			serviceNode.Content[i+1] = valueNode
			return
		}
	}

	serviceNode.Content = append(serviceNode.Content,
		&yaml.Node{Kind: yaml.ScalarNode, Value: key},
		valueNode,
	)
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
