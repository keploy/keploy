package docker

// compose_model.go turns the FINAL (fully hook-modified) in-memory
// docker-compose document that `keploy cloud replay` produces into a focused,
// typed model the Docker-SDK orchestrator (pkg/client/app/compose_sdk.go)
// consumes to bring the stack up WITHOUT shelling out to the `docker compose`
// CLI.
//
// Scope: this parser deliberately covers ONLY the compose subset keploy itself
// emits for cloud replay — the injected keploy-agent service plus the app
// container(s) reconstructed from a single Kubernetes deployment. It is not a
// general-purpose compose loader. Everything here is pure (no docker daemon,
// no filesystem beyond os.Getenv for env inheritance) so it is unit-testable
// exactly like parseComposeServiceStates in pkg/client/app.

import (
	"fmt"
	"os"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// ComposeModel is the typed view of a cloud-replay compose document.
type ComposeModel struct {
	// Services in document order. The orchestrator starts keploy-agent first
	// (identified by key/container_name), then the app service(s).
	Services []ServiceSpec
	// Networks / Volumes are the top-level declarations. The orchestrator only
	// needs the named volumes (to pre-create them); user networks are not
	// recreated because cloud replay has no cross-container networking (the
	// app's dependencies are served from recorded mocks, and the app shares the
	// agent's network namespace).
	Networks map[string]NetworkSpec
	Volumes  map[string]VolumeSpec
}

// ServiceSpec is a single compose service reduced to the fields cloud replay
// uses. Anything compose supports but cloud replay never generates is omitted.
type ServiceSpec struct {
	Key            string              // compose service key (stable; --exit-code-from targets this)
	ContainerName  string              // container_name
	Image          string              // resolved (${VAR} expanded)
	Env            []string            // resolved "KEY=VALUE" (host-inherit + ${VAR} expanded)
	Ports          []string            // compose short-form, e.g. "16789:16789"
	Networks       []string            // network keys this service attaches to (informational)
	NetworkAliases map[string][]string // network key -> aliases
	Binds          []string            // bind AND named-volume mounts: "src:dst[:ro]"
	Tmpfs          map[string]string   // tmpfs target -> options
	CapAdd         []string
	SecurityOpt    []string
	Command        []string // CMD
	Entrypoint     []string // ENTRYPOINT
	WorkingDir     string
	DependsOn      []DependsOn
	Healthcheck    *Healthcheck
	PidMode        string // raw compose value, e.g. "service:keploy-agent"
	NetworkMode    string // raw compose value, e.g. "service:keploy-agent"
	DNS            []string
	DNSSearch      []string
	DNSOpt         []string
	Labels         map[string]string
	// Restart is intentionally NOT captured. cloud replay emits
	// `restart: unless-stopped`, but under `--abort-on-container-exit` the app
	// must NOT be auto-restarted by the daemon (it would break exit-code capture
	// and teardown), so the orchestrator always forces RestartPolicy=no.
}

// DependsOn is one entry of a service's depends_on. Condition is
// "service_healthy" / "service_started" / "service_completed_successfully"
// ("service_started" when the short (list) form is used).
type DependsOn struct {
	Service   string
	Condition string
}

// Healthcheck mirrors container.HealthConfig (durations already parsed).
type Healthcheck struct {
	Test        []string
	Interval    time.Duration
	Timeout     time.Duration
	Retries     int
	StartPeriod time.Duration
	Disable     bool
}

// NetworkSpec / VolumeSpec capture only what the orchestrator might act on.
type NetworkSpec struct {
	Driver   string
	External bool
	Name     string
}

type VolumeSpec struct {
	Driver   string
	External bool
	Name     string
}

// ServiceCondHealthy is the depends_on condition that gates a service's start
// on another service reporting healthy (compose `condition: service_healthy`).
const ServiceCondHealthy = "service_healthy"

// composeServicePrefix is compose's syntax for "share this namespace with
// another service" used by `pid:`/`network_mode:` (e.g. "service:keploy-agent").
const composeServicePrefix = "service:"

// ParseComposeForSDK parses the final cloud-replay compose YAML into a
// ComposeModel. Env values are resolved the way `docker compose up` would
// (bare-key / empty-value host inheritance and ${VAR} expansion) so the
// SDK path produces identical container environments to the CLI path.
func ParseComposeForSDK(content []byte) (*ComposeModel, error) {
	var c Compose
	if err := yaml.Unmarshal(content, &c); err != nil {
		return nil, fmt.Errorf("failed to parse compose YAML for SDK orchestration: %w", err)
	}

	m := &ComposeModel{
		Networks: map[string]NetworkSpec{},
		Volumes:  map[string]VolumeSpec{},
	}

	if c.Services.Kind == yaml.MappingNode {
		for i := 0; i+1 < len(c.Services.Content); i += 2 {
			key := c.Services.Content[i].Value
			svc, err := parseServiceNode(key, c.Services.Content[i+1])
			if err != nil {
				return nil, fmt.Errorf("service %q: %w", key, err)
			}
			m.Services = append(m.Services, svc)
		}
	}

	if len(m.Services) == 0 {
		return nil, fmt.Errorf("compose document has no services")
	}

	if c.Volumes.Kind == yaml.MappingNode {
		for i := 0; i+1 < len(c.Volumes.Content); i += 2 {
			name := c.Volumes.Content[i].Value
			m.Volumes[name] = parseVolumeNode(c.Volumes.Content[i+1])
		}
	}
	if c.Networks.Kind == yaml.MappingNode {
		for i := 0; i+1 < len(c.Networks.Content); i += 2 {
			name := c.Networks.Content[i].Value
			m.Networks[name] = parseNetworkNode(c.Networks.Content[i+1])
		}
	}

	return m, nil
}

func parseServiceNode(key string, n *yaml.Node) (ServiceSpec, error) {
	svc := ServiceSpec{Key: key}
	if n.Kind != yaml.MappingNode {
		return svc, fmt.Errorf("expected a mapping, got kind %d", n.Kind)
	}

	for i := 0; i+1 < len(n.Content); i += 2 {
		field := n.Content[i].Value
		val := n.Content[i+1]

		switch field {
		case "image":
			svc.Image = expandComposeValue(val.Value)
		case "container_name":
			svc.ContainerName = val.Value
		case "working_dir":
			svc.WorkingDir = val.Value
		case "pid":
			svc.PidMode = val.Value
		case "network_mode":
			svc.NetworkMode = val.Value
		case "environment":
			svc.Env = parseEnvNode(val)
		case "ports":
			svc.Ports = nodeToStringSlice(val)
		case "command":
			svc.Command = nodeToStringSlice(val)
		case "entrypoint":
			svc.Entrypoint = nodeToStringSlice(val)
		case "cap_add":
			svc.CapAdd = nodeToStringSlice(val)
		case "security_opt":
			svc.SecurityOpt = nodeToStringSlice(val)
		case "dns":
			svc.DNS = nodeToStringSlice(val)
		case "dns_search":
			svc.DNSSearch = nodeToStringSlice(val)
		case "dns_opt":
			svc.DNSOpt = nodeToStringSlice(val)
		case "volumes":
			svc.Binds = nodeToStringSlice(val)
		case "tmpfs":
			svc.Tmpfs = parseTmpfsNode(val)
		case "networks":
			svc.Networks, svc.NetworkAliases = parseNetworksNode(val)
		case "depends_on":
			svc.DependsOn = parseDependsOnNode(val)
		case "healthcheck":
			hc, err := parseHealthcheckNode(val)
			if err != nil {
				return svc, fmt.Errorf("healthcheck: %w", err)
			}
			svc.Healthcheck = hc
		case "labels":
			svc.Labels = parseLabelsNode(val)
		case "restart":
			// Intentionally dropped — see ServiceSpec.Restart note.
		default:
			// Ignored by design. The cases above are exactly what cloud replay's
			// K8s->compose reconstruction emits (enterprise DockerComposeService:
			// image/container_name/ports/environment/volumes/networks/entrypoint/
			// command/working_dir/restart/depends_on/dns_search) plus what the
			// injected keploy-agent service and the enterprise compose hooks add
			// (cap_add/security_opt/tmpfs/healthcheck/pid/network_mode). That
			// reconstruction has no `user`, ulimits, sysctls, stop_signal, etc.,
			// so no other compose field can reach here; if the generator ever
			// starts emitting one that affects the run, add a case for it (and the
			// matching container.Config/HostConfig field in buildContainerSpec).
		}
	}
	return svc, nil
}

// parseEnvNode resolves an `environment:` node (list or map form) into a slice
// of "KEY=VALUE" the way docker compose would at `up` time:
//   - list "KEY=VALUE"       -> value expanded
//   - list "KEY" (no '=')    -> inherited from the host env (os.Getenv)
//   - map  KEY: VALUE        -> value expanded
//   - map  KEY: (null/empty) -> inherited from the host env (os.Getenv)
func parseEnvNode(n *yaml.Node) []string {
	var out []string
	switch n.Kind {
	case yaml.SequenceNode:
		for _, item := range n.Content {
			entry := item.Value
			if k, v, ok := strings.Cut(entry, "="); ok {
				out = append(out, k+"="+expandComposeValue(v))
			} else {
				// Bare key -> inherit from host (the mechanism the enterprise
				// hook uses to pass KEPLOY_COUCHBASE_PASSWORD without writing the
				// cleartext into the YAML).
				out = append(out, entry+"="+os.Getenv(entry))
			}
		}
	case yaml.MappingNode:
		for i := 0; i+1 < len(n.Content); i += 2 {
			key := n.Content[i].Value
			valNode := n.Content[i+1]
			// A null/empty value is compose's inherit-from-host form.
			if valNode.Tag == "!!null" || (valNode.Kind == yaml.ScalarNode && valNode.Value == "" && valNode.Tag != "!!str") {
				out = append(out, key+"="+os.Getenv(key))
				continue
			}
			out = append(out, key+"="+expandComposeValue(valNode.Value))
		}
	}
	return out
}

// parseNetworksNode handles both the list form (["net"]) and the map form
// ({net: {aliases: [...]}}), returning the ordered network keys and any aliases.
func parseNetworksNode(n *yaml.Node) ([]string, map[string][]string) {
	var keys []string
	aliases := map[string][]string{}
	switch n.Kind {
	case yaml.SequenceNode:
		for _, item := range n.Content {
			keys = append(keys, item.Value)
		}
	case yaml.MappingNode:
		for i := 0; i+1 < len(n.Content); i += 2 {
			key := n.Content[i].Value
			keys = append(keys, key)
			cfg := n.Content[i+1]
			if cfg.Kind == yaml.MappingNode {
				for j := 0; j+1 < len(cfg.Content); j += 2 {
					if cfg.Content[j].Value == "aliases" {
						aliases[key] = nodeToStringSlice(cfg.Content[j+1])
					}
				}
			}
		}
	}
	if len(aliases) == 0 {
		aliases = nil
	}
	return keys, aliases
}

// parseDependsOnNode handles the list form (implicit service_started) and the
// map form ({svc: {condition: service_healthy}}).
func parseDependsOnNode(n *yaml.Node) []DependsOn {
	var deps []DependsOn
	switch n.Kind {
	case yaml.SequenceNode:
		for _, item := range n.Content {
			deps = append(deps, DependsOn{Service: item.Value, Condition: "service_started"})
		}
	case yaml.MappingNode:
		for i := 0; i+1 < len(n.Content); i += 2 {
			dep := DependsOn{Service: n.Content[i].Value, Condition: "service_started"}
			cfg := n.Content[i+1]
			if cfg.Kind == yaml.MappingNode {
				for j := 0; j+1 < len(cfg.Content); j += 2 {
					if cfg.Content[j].Value == "condition" {
						dep.Condition = cfg.Content[j+1].Value
					}
				}
			}
			deps = append(deps, dep)
		}
	}
	return deps
}

func parseTmpfsNode(n *yaml.Node) map[string]string {
	entries := nodeToStringSlice(n)
	if len(entries) == 0 {
		return nil
	}
	out := map[string]string{}
	for _, e := range entries {
		// Compose tmpfs short form: "/target[:opts]".
		if target, opts, ok := strings.Cut(e, ":"); ok {
			out[target] = opts
		} else {
			out[e] = ""
		}
	}
	return out
}

func parseLabelsNode(n *yaml.Node) map[string]string {
	out := map[string]string{}
	switch n.Kind {
	case yaml.SequenceNode:
		for _, item := range n.Content {
			if k, v, ok := strings.Cut(item.Value, "="); ok {
				out[k] = v
			} else {
				out[item.Value] = ""
			}
		}
	case yaml.MappingNode:
		for i := 0; i+1 < len(n.Content); i += 2 {
			out[n.Content[i].Value] = n.Content[i+1].Value
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// composeHealthcheckRaw is the intermediate form for a `healthcheck:` node so
// the test field (string or list) and duration strings can be normalised.
type composeHealthcheckRaw struct {
	Test        yaml.Node `yaml:"test"`
	Interval    string    `yaml:"interval"`
	Timeout     string    `yaml:"timeout"`
	Retries     int       `yaml:"retries"`
	StartPeriod string    `yaml:"start_period"`
	Disable     bool      `yaml:"disable"`
}

func parseHealthcheckNode(n *yaml.Node) (*Healthcheck, error) {
	var raw composeHealthcheckRaw
	if err := n.Decode(&raw); err != nil {
		return nil, err
	}

	hc := &Healthcheck{Retries: raw.Retries, Disable: raw.Disable}

	// test: a scalar is compose's shell form; a sequence is the explicit
	// ["CMD"|"CMD-SHELL", ...] form. keploy's agent uses the CMD (exec) form.
	switch raw.Test.Kind {
	case yaml.ScalarNode:
		if raw.Test.Value != "" {
			hc.Test = []string{"CMD-SHELL", raw.Test.Value}
		}
	case yaml.SequenceNode:
		hc.Test = nodeToStringSlice(&raw.Test)
	}

	for _, d := range []struct {
		raw string
		dst *time.Duration
	}{
		{raw.Interval, &hc.Interval},
		{raw.Timeout, &hc.Timeout},
		{raw.StartPeriod, &hc.StartPeriod},
	} {
		if d.raw == "" {
			continue
		}
		parsed, err := time.ParseDuration(d.raw)
		if err != nil {
			return nil, fmt.Errorf("invalid duration %q: %w", d.raw, err)
		}
		*d.dst = parsed
	}
	return hc, nil
}

func parseVolumeNode(n *yaml.Node) VolumeSpec {
	var v VolumeSpec
	if n.Kind != yaml.MappingNode {
		return v
	}
	for i := 0; i+1 < len(n.Content); i += 2 {
		switch n.Content[i].Value {
		case "driver":
			v.Driver = n.Content[i+1].Value
		case "external":
			v.External = n.Content[i+1].Value == "true"
		case "name":
			v.Name = n.Content[i+1].Value
		}
	}
	return v
}

func parseNetworkNode(n *yaml.Node) NetworkSpec {
	var net NetworkSpec
	if n.Kind != yaml.MappingNode {
		return net
	}
	for i := 0; i+1 < len(n.Content); i += 2 {
		switch n.Content[i].Value {
		case "driver":
			net.Driver = n.Content[i+1].Value
		case "external":
			net.External = n.Content[i+1].Value == "true"
		case "name":
			net.Name = n.Content[i+1].Value
		}
	}
	return net
}

// nodeToStringSlice normalises a scalar or sequence node into a string slice.
// A scalar becomes a single-element slice; a sequence yields its scalar values.
func nodeToStringSlice(n *yaml.Node) []string {
	switch n.Kind {
	case yaml.ScalarNode:
		if n.Value == "" {
			return nil
		}
		return []string{n.Value}
	case yaml.SequenceNode:
		out := make([]string, 0, len(n.Content))
		for _, item := range n.Content {
			out = append(out, item.Value)
		}
		return out
	default:
		return nil
	}
}

// expandComposeValue expands ${VAR}, $VAR and ${VAR:-default}/${VAR-default}
// from the host environment and unescapes $$ -> $, matching how docker compose
// interpolates a compose document at `up` time. It intentionally supports only
// the subset of compose interpolation the generated documents can contain;
// unknown ${...} constructs resolve to empty (compose's default) rather than
// erroring, keeping the SDK path strictly more lenient than the CLI.
func expandComposeValue(s string) string {
	if !strings.Contains(s, "$") {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); {
		if s[i] != '$' {
			b.WriteByte(s[i])
			i++
			continue
		}
		// "$$" -> literal "$"
		if i+1 < len(s) && s[i+1] == '$' {
			b.WriteByte('$')
			i += 2
			continue
		}
		// "${...}" braced form (supports :- / - default operators)
		if i+1 < len(s) && s[i+1] == '{' {
			end := strings.IndexByte(s[i+2:], '}')
			if end == -1 {
				// Unterminated — emit verbatim.
				b.WriteByte(s[i])
				i++
				continue
			}
			expr := s[i+2 : i+2+end]
			b.WriteString(resolveBracedVar(expr))
			i += 2 + end + 1
			continue
		}
		// "$VAR" bare form
		if j := scanVarName(s[i+1:]); j > 0 {
			b.WriteString(os.Getenv(s[i+1 : i+1+j]))
			i += 1 + j
			continue
		}
		// A lone '$' not starting a variable — emit verbatim.
		b.WriteByte(s[i])
		i++
	}
	return b.String()
}

// resolveBracedVar resolves the inside of a ${...} expression, honouring the
// :-default and -default operators (empty vs unset). Other operators fall back
// to a plain lookup.
func resolveBracedVar(expr string) string {
	if idx := strings.Index(expr, ":-"); idx != -1 {
		name, def := expr[:idx], expr[idx+2:]
		if v := os.Getenv(name); v != "" {
			return v
		}
		return def
	}
	if idx := strings.Index(expr, "-"); idx != -1 {
		name, def := expr[:idx], expr[idx+1:]
		if v, ok := os.LookupEnv(name); ok {
			return v
		}
		return def
	}
	// Strip a trailing :?msg / ?msg (required-var operator) — the SDK path does
	// not fail the run on it; it just resolves the value (possibly empty).
	if idx := strings.IndexAny(expr, ":?"); idx != -1 {
		expr = expr[:idx]
	}
	return os.Getenv(expr)
}

// scanVarName returns the length of the leading POSIX shell variable name in s
// ([A-Za-z_][A-Za-z0-9_]*), or 0 if s does not start with one.
func scanVarName(s string) int {
	if len(s) == 0 {
		return 0
	}
	c := s[0]
	if !(c == '_' || (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z')) {
		return 0
	}
	i := 1
	for i < len(s) {
		c := s[i]
		if c == '_' || (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') {
			i++
			continue
		}
		break
	}
	return i
}

// IsShareServiceNamespace reports whether a compose pid:/network_mode: value
// refers to another service's namespace ("service:<name>") and returns that
// service name. The SDK orchestrator translates this to the started container's
// id ("container:<id>").
func IsShareServiceNamespace(mode string) (string, bool) {
	if strings.HasPrefix(mode, composeServicePrefix) {
		return strings.TrimPrefix(mode, composeServicePrefix), true
	}
	return "", false
}
