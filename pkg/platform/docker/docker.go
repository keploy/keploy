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
		cn := serviceKeyThroughMerge(serviceContentNode, "container_name")
		containerNameMatch := cn != nil && cn.Value == containerName

		// If explicit container_name matches or service name matches, extract networks and ports
		if containerNameMatch || serviceName == containerName {
			// `ports:` and `networks:` are routinely put in a shared `x-*`
			// fragment. Read off the unmerged node they come back empty, and the
			// app is published on nothing and joins the wrong network.
			idc.flattenMergeKeys(serviceContentNode)
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
			return idc.parseNetworksNode(aliasTarget(valueNode))
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
			// `ports: *appports` matches no arm of the switch below, so the app's
			// published ports came back empty. keploy-agent publishes on the app's
			// behalf under `network_mode: service:keploy-agent`, so the app then
			// has no published port at all and is unreachable from the host.
			return idc.parsePortsNode(aliasTarget(valueNode))
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
			// An element can be an alias too, and dropping it is worse than
			// reading nothing: the fallback below then puts keploy-agent on
			// `default` while the app, sharing the agent's netns, silently loses
			// the network its database is on.
			networkNode = aliasTarget(networkNode)
			if networkNode.Kind == yaml.ScalarNode && !isEmptyNode(networkNode) {
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
		// Single network as string. `networks:` with nothing under it is also a
		// scalar, and taking its empty Value produced a network named "" -- which
		// keploy then copied onto keploy-agent, where compose rejects it with
		// "additional properties '' not allowed".
		if !isEmptyNode(networksNode) {
			networks = []string{networksNode.Value}
		}
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
			portNode = aliasTarget(portNode)
			if portNode.Kind == yaml.ScalarNode && !isEmptyNode(portNode) {
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
		// Single port as string: ports: "80:80". An empty `ports:` is a scalar
		// too, and publishing "" on keploy-agent fails with "invalid proto: ".
		if !isEmptyNode(portsNode) {
			ports = []string{portsNode.Value}
		}
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
		valueNode := aliasTarget(portNode.Content[i+1])

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
			if cn := serviceKeyThroughMerge(svc, "container_name"); cn != nil &&
				cn.Value == "keploy-agent" {
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

		// Check 2: Does the container_name match? Read through an alias and
		// through `<<`: on the alias node's own Value the comparison is against
		// the anchor NAME, and an inherited container_name is not in Content at
		// all -- either way the lookup misses and keploy aborts with "failed to
		// find target service" on a file docker compose accepts.
		if cn := serviceKeyThroughMerge(serviceContentNode, "container_name"); cn != nil &&
			cn.Value == appIdentifier {
			return serviceContentNode, serviceName, nil
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

	// Detach before anything is written. keploy deletes, moves and overwrites
	// keys on this service, and every one of those corrupts a node the rest of
	// the user's file reads through an alias -- see detachSubtree. The top-level
	// sections keploy appends to need the same treatment, one node deep: it adds
	// a key to `services:` and a volume to `volumes:`, and an anchored section
	// shared with, say, `networks: *v` would carry those into it.
	// Before detaching: materialise anything the service inherits through `<<`,
	// so the writers below see real keys and detach sees the final subtree.
	if n := idc.flattenMergeKeys(targetServiceNode); n > 0 {
		idc.logger.Debug("materialised keys the app service inherits through a "+
			"YAML merge key, so keploy's edits apply to them",
			zap.Int("keys", n), zap.String("service", serviceName))
	}

	roots := compose.documentRoots()
	owned := map[*yaml.Node]bool{}
	collectNodes(targetServiceNode, owned)
	// The sections themselves, one node deep -- keploy adds a key to `services:`
	// and a volume to `volumes:`. Both the struct's copy and the live node the
	// aliases actually point at, since they are not the same pointer.
	for _, key := range []string{"services", "volumes"} {
		if n := compose.rawSection(key); n != nil {
			owned[n] = true
		}
	}
	owned[&compose.Services] = true
	owned[&compose.Volumes] = true
	if n := detachSubtree(roots, owned); n > 0 {
		idc.logger.Debug("expanded YAML aliases that read through nodes keploy edits, "+
			"so the generated compose file carries keploy's changes without them "+
			"reaching services keploy was not asked to touch",
			zap.Int("references", n), zap.String("service", serviceName))
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
			// TLS-cert mount would simply not appear. A present-but-empty
			// `volumes:` fails the same way; unlike environment, a service list
			// has exactly one legal shape, so an empty mapping is reshaped too.
			valueNode = resolveAliasByCopy(serviceNode.Content[i+1])
			ensureSequence(valueNode)
			if valueNode.Kind != yaml.SequenceNode {
				idc.logger.Warn("a service list cannot hold keploy's entry, so it will "+
					"not appear in the generated compose file",
					zap.String("key", key), zap.String("entry", value),
					zap.String("shape", yamlKindName(serviceNode.Content[i+1].Kind)),
					zap.String("resolvesTo", yamlKindName(resolvedKind(serviceNode.Content[i+1]))),
					zap.String("anchor", serviceNode.Content[i+1].Anchor))
			}
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

// setScalarValue replaces a value node with a scalar, rather than writing
// through the node it finds.
//
// `SSL_CERT_FILE: *ca` is an alias, and assigning to its Value overwrites the
// alias NAME -- marshalling then fails with "alias value must contain
// alphanumerical characters only" and keploy aborts on a compose file docker
// accepts. `x-*` plus an anchor is the conventional way to share config, so this
// is not an exotic shape.
//
// detachSubtree cannot help here: the anchor lives outside the app service, so
// it is not one of the nodes keploy claims ownership of.
func setScalarValue(slot **yaml.Node, value string) {
	old := *slot
	*slot = &yaml.Node{
		Kind:  yaml.ScalarNode,
		Value: value,
		// Head and line comments both attach to a VALUE node in real files --
		// `KEY:` with the value on the next line under a comment, and a trailing
		// `# note` -- and both are pinned by a test. FootComment is not carried:
		// no shape I could construct puts one on a value node, so copying it
		// would be code no test could justify.
		HeadComment: old.HeadComment,
		LineComment: old.LineComment,
	}
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
			env := resolveAliasByCopy(serviceNode.Content[i+1])
			// A present-but-empty `environment:` fails the same way. An empty
			// MAPPING is exempt: it already holds entries fine, and reshaping it
			// would rewrite the user's chosen style for no gain.
			if env.Kind != yaml.MappingNode {
				ensureSequence(env)
			}
			if env.Kind != yaml.SequenceNode && env.Kind != yaml.MappingNode {
				// Nothing below this can land. Say so here: the symptom is a TLS
				// verification failure at replay, which points nowhere near
				// compose generation.
				idc.logger.Warn("`environment:` cannot hold an entry, so keploy cannot "+
					"give the app its CA paths (NODE_EXTRA_CA_CERTS, "+
					"REQUESTS_CA_BUNDLE, SSL_CERT_FILE, CARGO_HTTP_CAINFO) or "+
					"JAVA_TOOL_OPTIONS; expect TLS verification failures at replay",
					zap.String("environmentShape", yamlKindName(serviceNode.Content[i+1].Kind)),
					zap.String("resolvesTo", yamlKindName(resolvedKind(serviceNode.Content[i+1]))),
					zap.String("anchor", serviceNode.Content[i+1].Anchor))
			}
			return env
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
		for i, node := range envNode.Content {
			// An element can be an alias, whose own Value is the anchor NAME --
			// so `- *jopts` naming "JAVA_TOOL_OPTIONS=-Xmx1g" matched nothing and
			// keploy appended a SECOND JAVA_TOOL_OPTIONS entry. compose takes the
			// last, so the user's setting is silently gone.
			if strings.HasPrefix(aliasTarget(node).Value, prefix) {
				// Key already exists — replace the element rather than writing
				// through it, for the reason setScalarValue exists.
				setScalarValue(&envNode.Content[i], fmt.Sprintf("%s=%s", envKey, envValue))
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
				// Key already exists — replace its value. See setScalarValue for
				// why this is not an in-place write.
				setScalarValue(&envNode.Content[i+1], envValue)
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
		for i, node := range envNode.Content {
			node = aliasTarget(node)
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
				setScalarValue(&envNode.Content[i], node.Value+" "+strings.Join(missing, " "))
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
				// Read through an alias: its own Value is the anchor NAME, so
				// `JAVA_TOOL_OPTIONS: *jopts` would otherwise compare keploy's
				// tokens against "jopts" and append to that.
				existingVal := aliasTarget(envNode.Content[i+1]).Value
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
				setScalarValue(&envNode.Content[i+1], existingVal+" "+strings.Join(missing, " "))
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

// flattenMergeKeys materialises `<<:` into explicit keys on a service node, so
// the rest of this file can treat it as an ordinary mapping.
//
// keploy's writers all scan Content for a literal key name, and a key inherited
// through a merge is not there -- it lives in the fragment `<<` names. Three
// separate things went wrong as a result, on a file docker compose accepts:
//
//   - `removeServiceProperty(app, "networks")` removed nothing, so the generated
//     file carried the inherited `networks:` alongside the `network_mode:` keploy
//     adds, and compose rejects that pair as mutually exclusive
//   - `getOrCreateEnvNode` did not find the inherited `environment:`, so keploy
//     appended a NEW one -- an explicit key overrides a merged key, so every
//     variable the user had inherited was silently gone
//   - the gate read no `ports:`, so keploy-agent published nothing on the app's
//     behalf and the app was unreachable from the host
//
// Reading through the merge instead of materialising it would fix only the third:
// a key that is inherited cannot be DELETED, which is what keploy needs to do to
// `networks` and `ports`. YAML has no way to unset a merged key.
//
// Key and value are deep-cloned rather than referenced, for the same reason
// detachSubtree copies: keploy edits what it takes, and the fragment belongs to
// the user.
//
// A clone can carry an anchor the fragment defined, which would be a second
// definition of that name. This does NOT strip it, because ModifyComposeForAgent
// runs detachSubtree over the same subtree immediately afterwards and that
// clears every anchor in it -- which is the reason flattening comes first there,
// and a test pins that order.
//
// The other caller, the read gate, does NOT detach, so a service it flattens can
// carry a duplicate anchor until the rewrite runs. That is fine only because
// nothing writes a compose file off the gate alone; a caller that did would have
// to detach itself.
//
// Precedence follows the YAML merge spec: a key the node states explicitly wins
// over any merged one, and within `<<: [*a, *b]` the earlier entry wins. Merges
// nested inside a merged fragment are followed too.
//
// Idempotent: it removes the `<<` key it consumed, so a second call does nothing.
func (idc *Impl) flattenMergeKeys(n *yaml.Node) int {
	// The Kind check states the contract rather than defending a known failure:
	// no test distinguishes it, because the scan below finds no `<<` key on a
	// scalar or a sequence and returns early anyway. It stays so that a future
	// change to the scan cannot start rewriting Content on a node that does not
	// hold key/value pairs.
	if n == nil || n.Kind != yaml.MappingNode {
		return 0
	}

	// Rebuilt without the `<<` entries: each is being replaced by what it stood
	// for, and leaving one would re-merge the fragment over keploy's edits when
	// compose loads the generated file.
	present := map[string]bool{}
	var mergeValues []*yaml.Node
	kept := n.Content[:0]
	for i := 0; i+1 < len(n.Content); i += 2 {
		if isMergeKey(n.Content[i]) {
			// EVERY merge key is consumed, not just the first.
			//
			// A mapping cannot legally define `<<` twice, and docker compose
			// rejects such a file -- but decoding into a yaml.Node does not,
			// which is how one reaches keploy at all. Consuming only the first
			// would leave the keys the second brings in invisible to keploy's
			// writers, which is exactly the bug this function exists to fix, just
			// in a rarer file.
			//
			// Earlier wins between them, by analogy with `<<: [*a, *b]`. The spec
			// states no rule here because the shape is invalid, and go-yaml's own
			// decoder would do the opposite if it accepted it -- so this is a
			// choice, not a standard being followed.
			sources, usable := mergeSources(n.Content[i+1])
			if !usable {
				// go-yaml rejects this with "map merge requires map or sequence
				// of maps as the value", so docker compose does too. Consuming it
				// would delete the key and hand back a file that loads, hiding an
				// error the user needs to see -- keep it, and say why.
				idc.logger.Warn("a `<<:` merge key does not name a mapping, so keploy "+
					"cannot apply what it inherits; docker compose will reject this "+
					"file for the same reason",
					zap.String("mergeShape", yamlKindName(n.Content[i+1].Kind)),
					zap.String("resolvesTo", yamlKindName(resolvedKind(n.Content[i+1]))))
				kept = append(kept, n.Content[i], n.Content[i+1])
				continue
			}
			mergeValues = append(mergeValues, sources...)
			continue
		}
		present[n.Content[i].Value] = true
		kept = append(kept, n.Content[i], n.Content[i+1])
	}
	if len(mergeValues) == 0 {
		return 0
	}
	n.Content = kept

	added := 0
	for _, src := range mergeValues {
		for i := 0; i+1 < len(src.Content); i += 2 {
			key := src.Content[i].Value
			if key == "<<" || present[key] {
				continue
			}
			present[key] = true
			n.Content = append(n.Content,
				cloneYAMLNode(src.Content[i]), cloneYAMLNode(src.Content[i+1]))
			added++
		}
	}
	return added
}

// mergeSources returns the mappings a `<<` value names, in precedence order.
//
// `<<: *a` is one mapping; `<<: [*a, *b]` is several, and the YAML spec gives
// the EARLIER entry precedence -- the opposite of what "later overrides" reads
// like, and the reason the caller records keys as it goes.
//
// A fragment may merge in turn, so its own `<<` is followed here rather than by
// recursing into flattenMergeKeys, which would edit a node the user owns.
func mergeSources(v *yaml.Node) ([]*yaml.Node, bool) {
	var out []*yaml.Node
	usable := true
	var add func(*yaml.Node, int)
	add = func(n *yaml.Node, depth int) {
		if n == nil || depth > 16 {
			usable = false
			return
		}
		switch n.Kind {
		case yaml.AliasNode:
			add(n.Alias, depth+1)
		case yaml.SequenceNode:
			for _, e := range n.Content {
				add(e, depth+1)
			}
		case yaml.MappingNode:
			out = append(out, n)
			// A fragment may merge in turn. Every `<<` inside one is followed,
			// for the same reason the top level consumes every one: a key reached
			// only through the second would otherwise stay invisible.
			for i := 0; i+1 < len(n.Content); i += 2 {
				if isMergeKey(n.Content[i]) {
					add(n.Content[i+1], depth+1)
				}
			}
		default:
			// A null, a scalar, or anything else that cannot be merged.
			usable = false
		}
	}
	add(v, 0)
	return out, usable
}

// serviceKeyThroughMerge finds a key on a service, following `<<` but modifying
// nothing.
//
// The scans that SELECT the app service run before keploy has decided to touch
// the file, and they cannot flatten first: flattening is keyed on having already
// found the service. An inherited `container_name` was therefore invisible to
// selection, and keploy aborted outright with "failed to find target service" on
// a compose file docker accepts -- the one member of this bug class that fails
// loudly rather than silently.
func serviceKeyThroughMerge(service *yaml.Node, key string) *yaml.Node {
	if service == nil || service.Kind != yaml.MappingNode {
		return nil
	}
	var merges []*yaml.Node
	for i := 0; i+1 < len(service.Content); i += 2 {
		if isMergeKey(service.Content[i]) {
			merges = append(merges, service.Content[i+1])
			continue
		}
		// An explicit key wins over anything inherited.
		if service.Content[i].Value == key {
			return aliasTarget(service.Content[i+1])
		}
	}
	for _, m := range merges {
		sources, _ := mergeSources(m)
		for _, src := range sources {
			for i := 0; i+1 < len(src.Content); i += 2 {
				if src.Content[i].Value == key {
					return aliasTarget(src.Content[i+1])
				}
			}
		}
	}
	return nil
}

// isMergeKey reports whether a key node is the YAML merge key rather than a
// string that happens to read like one.
//
// `<<: *a` carries the `!!merge` tag; `"<<": *a` is a plain `!!str` that go-yaml
// keeps as an ordinary key. Matching on Value alone would silently delete that
// key -- bizarre input, but deleting a user's data over it is not defensible.
func isMergeKey(key *yaml.Node) bool {
	return key.Value == "<<" && key.Tag == "!!merge"
}

// detachSubtree makes every node in a subtree safe for keploy to edit in place,
// by giving each alias that reads through it an inline copy of what it names
// TODAY and then dropping the anchor.
//
// keploy does not merely append to the app service. It DELETES `networks` and
// `ports`, MOVES `dns*` onto keploy-agent, OVERWRITES `network_mode` and `pid`,
// and appends to `environment` and `volumes`. Every one of those is wrong on a
// node the user's file reads through an alias, and the failures are not subtle:
//
//	networks: &n [default]    deleting the app's key deletes the anchor
//	sidecar: {networks: *n}   DEFINITION, and the generated file then fails to
//	                          load at all -- "unknown anchor 'n' referenced"
//
//	environment: &e [MINE=1]  appending hands the sidecar keploy's CA paths
//	sidecar: {environment: *e}  while it lives OUTSIDE the agent's network
//	                          namespace, so its TLS fails verification against a
//	                          certificate file its container does not have
//
// Expanding a reference costs its reader nothing -- it names exactly the content
// it named before -- and the file this produces is the temporary one keploy
// runs, not the user's source. Declining to edit instead was tried and is worse:
// it leaves the app half-wired (pid set, network_mode not), which docker compose
// accepts and which records nothing at all.
//
// Returns the number of references expanded, for the caller to log.
func detachSubtree(roots []*yaml.Node, owned map[*yaml.Node]bool) int {
	if len(owned) == 0 {
		return 0
	}

	expanded := 0
	// Expanding one reference can copy an alias naming another node inside the
	// subtree, so repeat until the document holds none.
	//
	// No fixture I could build needs a second pass, and no test here
	// distinguishes this loop from a single pass. The loop stays regardless.
	//
	// The tempting argument for dropping it -- YAML requires an alias to follow
	// its anchor, so a reference nested inside a fragment is always reached
	// first -- does not actually hold: the walk below runs over `roots` in list
	// order, not document order, so an alias in an `x-*` key written ABOVE
	// `services:` is visited last. Getting that wrong leaves a dangling alias,
	// which does not degrade the generated file, it makes it unloadable. The
	// bound is a termination guarantee, not a policy.
	for range 64 {
		refs := aliasRefsInto(roots, owned)
		if len(refs) == 0 {
			break
		}
		for _, ref := range refs {
			clone := cloneYAMLNode(ref.Alias)
			stripAnchors(clone)
			*ref = *clone
			expanded++
		}
	}

	// Nothing reads through them any more, so the anchors are free to go. Left
	// in place they would be duplicated by any later copy of this subtree.
	for n := range owned {
		n.Anchor = ""
	}
	return expanded
}

func collectNodes(n *yaml.Node, into map[*yaml.Node]bool) {
	if n == nil || into[n] {
		return
	}
	into[n] = true
	for _, c := range n.Content {
		collectNodes(c, into)
	}
}

// aliasRefsInto finds every alias in the document that resolves to a node the
// caller is about to mutate.
func aliasRefsInto(roots []*yaml.Node, owned map[*yaml.Node]bool) []*yaml.Node {
	var found []*yaml.Node
	seen := map[*yaml.Node]bool{}
	var walk func(*yaml.Node)
	walk = func(n *yaml.Node) {
		if n == nil || seen[n] {
			return
		}
		seen[n] = true
		if n.Kind == yaml.AliasNode && n.Alias != nil && owned[n.Alias] {
			found = append(found, n)
			return
		}
		for _, c := range n.Content {
			walk(c)
		}
	}
	for _, r := range roots {
		walk(r)
	}
	return found
}

// rawSection returns the LIVE node for a top-level key.
//
// The Compose struct's section fields are value copies made when the file was
// decoded -- their Content children are the same pointers as the original's, but
// the section nodes themselves are not. An alias to `volumes: &v` therefore
// points at raw's node, never at `&compose.Volumes`, so anything matching on
// node identity has to look here or it silently matches nothing.
func (c *Compose) rawSection(key string) *yaml.Node {
	if c.raw == nil {
		return nil
	}
	for i := 0; i+1 < len(c.raw.Content); i += 2 {
		if c.raw.Content[i].Value == key {
			return c.raw.Content[i+1]
		}
	}
	return nil
}

// documentRoots returns the nodes to search for aliases: the struct's section
// fields AND raw, always. Neither alone is enough.
//
// raw is the original mapping and its children are the same pointers as the live
// tree, but MarshalCompose splices the struct's own section fields back over it,
// and those are value copies made at decode time. An alias sitting directly
// under `volumes:` therefore exists as two separate nodes; expanding only raw's
// leaves the copy -- the one that gets emitted -- pointing at an anchor about to
// be cleared, and the generated file fails to load with "unknown anchor".
func (c *Compose) documentRoots() []*yaml.Node {
	// Networks, Configs and Secrets are here for the same reason even though
	// keploy never writes to them: a service's `volumes: &v` can be named by
	// `networks: *v`, and that reference has to be expanded like any other.
	roots := []*yaml.Node{&c.Services, &c.Networks, &c.Volumes, &c.Configs, &c.Secrets}
	if c.raw != nil {
		roots = append(roots, c.raw)
	}
	return roots
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

// aliasTarget follows an alias to what it names, WITHOUT mutating anything.
//
// The read paths must not use resolveAliasByCopy: that rewrites the node in
// place. The gate around them already resolves SERVICE aliases in place -- it
// has to, or the lookup finds nothing -- but that is a deliberate, narrow
// exception, and reading a port list is no reason to widen it.
func aliasTarget(n *yaml.Node) *yaml.Node {
	// Bounded rather than recursive: an anchor cannot be cyclic, but the node
	// comes from a user's file and this is a cheap way not to depend on that.
	for i := 0; i < 16; i++ {
		if n == nil || n.Kind != yaml.AliasNode || n.Alias == nil {
			return n
		}
		n = n.Alias
	}
	return n
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
	if target.Kind != yaml.MappingNode {
		// Resolves to something that cannot hold entries (a populated scalar or
		// sequence). Leave the alias exactly as written -- the caller warns --
		// rather than reshaping a fragment the user wrote.
		//
		// An alias to an EMPTY fragment never reaches here: ensureMapping above
		// has already reshaped the reference, so n is no longer an alias.
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
	if n == nil || n.Kind == yaml.MappingNode || !isEmptyNode(n) {
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

// isEmptyNode reports whether a node holds nothing, in any of the shapes YAML
// can express that: an absent key (zero node), a null in any spelling, an empty
// string, an empty sequence or mapping, or an alias naming any of those.
//
// A node carrying content is NOT empty, and that includes shapes whose payload
// does not live in Content: a scalar keeps its text in Value, so `len(Content)`
// is the tempting test and the wrong one -- it would report `volumes: appdata`
// as empty and discard what the user wrote. An alias is judged by what it names,
// for the same reason.
func isEmptyNode(n *yaml.Node) bool {
	switch n.Kind {
	case 0:
		return true
	case yaml.ScalarNode:
		return n.Tag == "!!null" || n.Value == ""
	case yaml.SequenceNode, yaml.MappingNode:
		return len(n.Content) == 0
	case yaml.AliasNode:
		// An alias to an empty fragment holds nothing either. Reshaping the
		// REFERENCE is safe -- the anchored fragment itself is a separate node
		// and stays exactly as the user wrote it.
		return n.Alias == nil || isEmptyNode(n.Alias)
	default:
		return false
	}
}

// ensureSequence reshapes a present-but-empty node into an empty sequence so an
// append lands somewhere the encoder emits.
//
// `environment:` or `volumes:` with nothing under them -- what a commented-out
// block leaves behind -- is not a zero node: it decodes to a `!!null` scalar, or
// to an alias naming an empty fragment. The writers' type switch matches neither
// shape, so every variable and mount keploy adds was dropped without a word and
// the app ran without keploy's CA. A node carrying anything is left untouched.
//
// Reshaping in place is safe for a node the app OWNS, because detachSubtree has
// already given away copies of anything shared. It is also safe for an alias
// naming an `x-*` fragment, which detach deliberately leaves alone: what gets
// reshaped there is the app's own reference node, not the fragment.
func ensureSequence(n *yaml.Node) {
	if n == nil || n.Kind == yaml.SequenceNode || !isEmptyNode(n) {
		return
	}
	n.Kind = yaml.SequenceNode
	// A leftover `!!null` tag is emitted verbatim onto the sequence, and Style
	// carries the flow bits of the shape being replaced, so `volumes: {}` would
	// otherwise normalise into a flow-style list in a block-style file. Both are
	// pinned by a test.
	//
	// Value and Alias are deliberately not cleared: the encoder reads neither
	// once Kind is a sequence. Content is left as isEmptyNode found it -- empty
	// for every shape that reaches here today, since yaml.v3 never gives an alias
	// node children. Re-check that if isEmptyNode ever admits a node with
	// children of its own.
	n.Tag = ""
	n.Style = 0
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
			// Replace rather than write through. Setting .Value only works when
			// the node is already a
			// scalar. On a `!!null` (`network_mode:` with nothing after it) the
			// tag survives and the file is emitted as `network_mode: !!null
			// service:keploy-agent`, which does not parse; on a mapping or
			// sequence the write is ignored and interception is silently off; on
			// an alias it overwrites the alias NAME and marshalling fails with
			// "alias value must contain alphanumerical characters only".
			//
			// Replacing the node is what "set this property" means in every one
			// of those cases. Comments are carried over so a `# keep me` above
			// the value is not dropped.
			setScalarValue(&serviceNode.Content[i+1], value)
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
			// Update existing depends_on.
			//
			// An aliased or present-but-empty `depends_on:` matches neither
			// branch below, and falling through drops the `keploy-agent:
			// {condition: service_healthy}` gate without a word -- so the app
			// container starts before the agent's proxy is listening and races
			// it, which surfaces as a handful of unrecorded calls at the head of
			// the session rather than as an error.
			dependsOnNode := resolveAliasByCopy(serviceNode.Content[i+1])
			ensureSequence(dependsOnNode)
			if dependsOnNode.Kind != yaml.SequenceNode && dependsOnNode.Kind != yaml.MappingNode {
				idc.logger.Warn("`depends_on:` cannot hold an entry, so keploy cannot "+
					"make the app wait for keploy-agent to become healthy; the app "+
					"may start before interception is ready",
					zap.String("dependsOnShape", yamlKindName(serviceNode.Content[i+1].Kind)),
					zap.String("resolvesTo", yamlKindName(resolvedKind(serviceNode.Content[i+1]))))
				return
			}

			// Check if it's a simple array or extended format
			if dependsOnNode.Kind == yaml.SequenceNode {
				// Store existing dependencies first
				existingDeps := make([]string, 0)
				for _, dep := range dependsOnNode.Content {
					// `- *db` is an alias; dropping it here silently removes a
					// real dependency, and the app stops waiting for it.
					dep = aliasTarget(dep)
					if dep.Kind == yaml.ScalarNode && !isEmptyNode(dep) &&
						dep.Value != "keploy-agent" {
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
