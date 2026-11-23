package docker

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"go.uber.org/zap"
	"gopkg.in/yaml.v3"
)

// MoveComposeAgentPort moves keploy's agent, in compose (a document
// ModifyComposeForAgent wrote it into), from port from to port to: its
// publish on the host's loopback (127.0.0.1:from:from), the port its server
// listens on (its command's --port from), and, in mock mode, the address an
// application's tests mark their bounds at (KEPLOY_MOCK_AGENT
// http://localhost:from in a service's environment). A document in which the
// agent's service does not publish from or is not told --port from, exactly
// once each, is left as it was, and its error says so: keploy does not start
// an agent whose port it could not move whole.
func MoveComposeAgentPort(compose *Compose, from, to uint32) error {
	if compose == nil {
		return fmt.Errorf("there is no compose document to move keploy's agent in")
	}
	var agent *yaml.Node
	for _, e := range effectiveServiceEntries(&compose.Services) {
		if aliasTarget(e.key).Value == "keploy-agent" {
			agent = aliasTarget(e.value)
			break
		}
	}
	if agent == nil || agent.Kind != yaml.MappingNode {
		return fmt.Errorf("the compose document has no keploy-agent service to move off port %d", from)
	}

	fromS, toS := strconv.FormatUint(uint64(from), 10), strconv.FormatUint(uint64(to), 10)
	publish := fmt.Sprintf("127.0.0.1:%s:%s", fromS, fromS)
	var publishes, ports []**yaml.Node
	for i := 0; i+1 < len(agent.Content); i += 2 {
		value := aliasTarget(agent.Content[i+1])
		if value.Kind != yaml.SequenceNode {
			continue
		}
		switch agent.Content[i].Value {
		case "ports":
			for j := range value.Content {
				if aliasTarget(value.Content[j]).Value == publish {
					publishes = append(publishes, &value.Content[j])
				}
			}
		case "command":
			for j := 0; j+1 < len(value.Content); j++ {
				if aliasTarget(value.Content[j]).Value == "--port" && aliasTarget(value.Content[j+1]).Value == fromS {
					ports = append(ports, &value.Content[j+1])
				}
			}
		}
	}
	if len(publishes) != 1 || len(ports) != 1 {
		return fmt.Errorf("keploy-agent's service publishes %s %d times and is told --port %d %d times, not once each, so keploy cannot move it off port %d",
			publish, len(publishes), from, len(ports), from)
	}
	movedScalar(publishes[0], fmt.Sprintf("127.0.0.1:%s:%s", toS, toS))
	movedScalar(ports[0], toS)

	mockFrom, mockTo := "http://localhost:"+fromS, "http://localhost:"+toS
	for _, e := range effectiveServiceEntries(&compose.Services) {
		service := aliasTarget(e.value)
		if service == nil || service.Kind != yaml.MappingNode {
			continue
		}
		for i := 0; i+1 < len(service.Content); i += 2 {
			if service.Content[i].Value != "environment" {
				continue
			}
			env := aliasTarget(service.Content[i+1])
			switch env.Kind {
			case yaml.SequenceNode:
				for j := range env.Content {
					if aliasTarget(env.Content[j]).Value == "KEPLOY_MOCK_AGENT="+mockFrom {
						movedScalar(&env.Content[j], "KEPLOY_MOCK_AGENT="+mockTo)
					}
				}
			case yaml.MappingNode:
				for j := 0; j+1 < len(env.Content); j += 2 {
					if aliasTarget(env.Content[j]).Value == "KEPLOY_MOCK_AGENT" && aliasTarget(env.Content[j+1]).Value == mockFrom {
						movedScalar(&env.Content[j+1], mockTo)
					}
				}
			}
		}
	}
	return nil
}

// movedScalar puts value in the scalar at slot, written as the one there was
// (its tag, its style, its comments): a new node, so an anchor the old one
// was, or an alias led to, is not changed for whatever else names it.
func movedScalar(slot **yaml.Node, value string) {
	n := *aliasTarget(*slot)
	n.Value, n.Anchor = value, ""
	*slot = &n
}

// FindComposeFiles returns the compose file paths for cmd in Docker Compose
// precedence order:
//  1. Explicit -f / --file flags on cmd.
//  2. The COMPOSE_FILE environment variable, split by COMPOSE_PATH_SEPARATOR
//     (or filepath.ListSeparator when unset).
//  3. Default discovery in the working directory: the first existing base file
//     (docker-compose.yml, docker-compose.yaml, compose.yml, compose.yaml),
//     followed by the first existing override file
//     (docker-compose.override.yml, docker-compose.override.yaml,
//     compose.override.yml, compose.override.yaml) when a base file is present.
func FindComposeFiles(cmd string) []string {
	args := strings.Fields(cmd)
	var paths []string
	for i := 0; i < len(args); i++ {
		switch {
		case (args[i] == "-f" || args[i] == "--file") && i+1 < len(args):
			paths = append(paths, args[i+1])
			i++
		case strings.HasPrefix(args[i], "-f="):
			if p := strings.TrimPrefix(args[i], "-f="); p != "" {
				paths = append(paths, p)
			}
		case strings.HasPrefix(args[i], "--file="):
			if p := strings.TrimPrefix(args[i], "--file="); p != "" {
				paths = append(paths, p)
			}
		}
	}
	if len(paths) > 0 {
		return paths
	}
	if env := os.Getenv("COMPOSE_FILE"); env != "" {
		sep := os.Getenv("COMPOSE_PATH_SEPARATOR")
		if sep == "" {
			sep = string(filepath.ListSeparator)
		}
		for _, raw := range strings.Split(env, sep) {
			if p := strings.TrimSpace(raw); p != "" {
				paths = append(paths, p)
			}
		}
		if len(paths) > 0 {
			return paths
		}
	}
	var base string
	for _, name := range []string{"docker-compose.yml", "docker-compose.yaml", "compose.yml", "compose.yaml"} {
		if _, err := os.Stat(name); err == nil {
			base = name
			break
		}
	}
	if base == "" {
		return nil
	}
	paths = []string{base}
	for _, override := range []string{"docker-compose.override.yml", "docker-compose.override.yaml", "compose.override.yml", "compose.override.yaml"} {
		if _, err := os.Stat(override); err == nil {
			paths = append(paths, override)
			break
		}
	}
	return paths
}

// ComposePublishedHostPorts returns the sorted, deduplicated host ports
// statically published by non-agent services in compose (both short syntax
// such as "61500:80" or "127.0.0.1:61500-61502:8000-8002/tcp" and long syntax
// with a "published" field). Dynamic container-only publishes ("80", ":80")
// and keploy-agent's own service are ignored.
func ComposePublishedHostPorts(compose *Compose) []uint32 {
	if compose == nil {
		return nil
	}
	seen := map[uint32]struct{}{}
	addPortRange := func(spec string) {
		for _, p := range parseComposeHostPortRange(spec) {
			seen[p] = struct{}{}
		}
	}
	for _, e := range effectiveServiceEntries(&compose.Services) {
		if aliasTarget(e.key).Value == "keploy-agent" {
			continue
		}
		svc := aliasTarget(e.value)
		if svc == nil || svc.Kind != yaml.MappingNode {
			continue
		}
		for i := 0; i+1 < len(svc.Content); i += 2 {
			if aliasTarget(svc.Content[i]).Value != "ports" {
				continue
			}
			ports := aliasTarget(svc.Content[i+1])
			if ports == nil || ports.Kind != yaml.SequenceNode {
				continue
			}
			for _, item := range ports.Content {
				node := aliasTarget(item)
				switch node.Kind {
				case yaml.ScalarNode:
					addPortRange(shortPublishHostPort(node.Value))
				case yaml.MappingNode:
					for j := 0; j+1 < len(node.Content); j += 2 {
						if aliasTarget(node.Content[j]).Value == "published" {
							addPortRange(aliasTarget(node.Content[j+1]).Value)
						}
					}
				}
			}
		}
	}
	if len(seen) == 0 {
		return nil
	}
	out := make([]uint32, 0, len(seen))
	for p := range seen {
		out = append(out, p)
	}
	slices.Sort(out)
	return out
}

// ComposePublishedHostPortsFromYAML parses a compose YAML document and returns
// ComposePublishedHostPorts, or nil if content is empty or unparseable.
func ComposePublishedHostPortsFromYAML(content []byte) []uint32 {
	if len(content) == 0 {
		return nil
	}
	var c Compose
	if err := yaml.Unmarshal(content, &c); err != nil {
		return nil
	}
	return ComposePublishedHostPorts(&c)
}

// ComposePublishedHostPortsFromFiles reads each compose file in paths
// (logging at Debug and skipping any file that cannot be read or parsed) and
// returns the sorted, deduplicated host ports statically published across them.
func ComposePublishedHostPortsFromFiles(logger *zap.Logger, paths []string) []uint32 {
	if len(paths) == 0 {
		return nil
	}
	seen := map[uint32]struct{}{}
	for _, p := range paths {
		if p == "" || p == "-" {
			continue
		}
		b, err := os.ReadFile(p) //nolint:gosec // user-supplied compose file path from the compose command
		if err != nil {
			if logger != nil {
				logger.Debug("skipping unreadable compose file while collecting published host ports", zap.String("path", p), zap.Error(err))
			}
			continue
		}
		var c Compose
		if err := yaml.Unmarshal(b, &c); err != nil {
			if logger != nil {
				logger.Debug("skipping unparseable compose file while collecting published host ports", zap.String("path", p), zap.Error(err))
			}
			continue
		}
		for _, port := range ComposePublishedHostPorts(&c) {
			seen[port] = struct{}{}
		}
	}
	if len(seen) == 0 {
		return nil
	}
	out := make([]uint32, 0, len(seen))
	for port := range seen {
		out = append(out, port)
	}
	slices.Sort(out)
	return out
}

// shortPublishHostPort extracts the host port (or start-end range) from a
// compose short-syntax port string ([ip:]host[-end]:container[-end][/proto]).
// Returns "" for container-only or dynamic host ports ("80", ":80", "127.0.0.1::80").
func shortPublishHostPort(spec string) string {
	s := strings.TrimSpace(spec)
	if s == "" {
		return ""
	}
	s, _, _ = strings.Cut(s, "/")
	colon := strings.LastIndexByte(s, ':')
	if colon <= 0 {
		return ""
	}
	hostPart := s[:colon]
	if ipSep := strings.LastIndexByte(hostPart, ':'); ipSep >= 0 {
		return hostPart[ipSep+1:]
	}
	return hostPart
}

func parseComposeHostPortRange(spec string) []uint32 {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return nil
	}
	if a, b, ok := strings.Cut(spec, "-"); ok {
		start, err1 := strconv.ParseUint(strings.TrimSpace(a), 10, 16)
		end, err2 := strconv.ParseUint(strings.TrimSpace(b), 10, 16)
		if err1 != nil || err2 != nil || start == 0 || end == 0 || start > end {
			return nil
		}
		out := make([]uint32, 0, end-start+1)
		for p := start; p <= end; p++ {
			out = append(out, uint32(p))
		}
		return out
	}
	p, err := strconv.ParseUint(spec, 10, 16)
	if err != nil || p == 0 {
		return nil
	}
	return []uint32{uint32(p)}
}
