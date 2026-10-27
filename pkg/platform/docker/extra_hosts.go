package docker

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	"github.com/docker/docker/api/types/versions"
	"github.com/docker/docker/client"
	"gopkg.in/yaml.v3"
)

// An application keploy runs in docker mode joins the agent container's
// network namespace (`--network=container:<agent>`, or compose's
// `network_mode: service:keploy-agent`). Docker refuses host entries of the
// application's own there ("conflicting options: custom host-to-IP mapping and
// the network mode"), and the namespace's /etc/hosts is the agent container's,
// which the application then reads. So the application's entries
// (`docker run --add-host`, a compose service's `extra_hosts`, a container's
// ExtraHosts) go onto the agent container, and the application starts with
// them as it does without keploy.
//
// The agent container also maps host.docker.internal to host-gateway, the
// Docker host as its daemon resolves it, as Docker Desktop does for every
// container: an application written for Desktop reaches its host's services on
// Linux too. An application's own entry for that name wins.

// dockerHostName is the name Docker Desktop gives every container for the
// Docker host, and the one keploy maps to host-gateway.
const dockerHostName = "host.docker.internal"

// hostGatewayAPI is the first Docker API version (Engine 20.10) whose daemon
// resolves the address host-gateway.
const hostGatewayAPI = "1.41"

// podmanHostGateway is the first Podman whose --add-host takes the address
// host-gateway (4.7.0). Podman's Docker-compatible API has said 1.41 or later
// since 4.2, so under Podman the API version does not tell: 4.2 to 4.6 say 1.41
// and refuse the entry ("invalid IP address in add-host"), and the agent would
// not start.
const podmanHostGateway = "4.7.0"

// HostGatewaySupported reports whether the daemon api talks to resolves the
// address host-gateway in a host entry. A daemon that cannot be asked is taken
// for one that does not: an entry it cannot resolve would stop the agent
// container from starting. Time never decides the answer: when ctx ends before
// the daemon answers, err is ctx's cause, not a no, for a loaded daemon's slow
// answer must not change the app's /etc/hosts.
func HostGatewaySupported(ctx context.Context, api client.APIClient) (bool, error) {
	if api == nil {
		return false, nil
	}
	v, err := api.ServerVersion(ctx)
	if err != nil {
		if ctx.Err() != nil {
			return false, context.Cause(ctx)
		}
		return false, nil
	}
	for _, c := range v.Components {
		if strings.EqualFold(c.Name, "Podman Engine") {
			return versions.GreaterThanOrEqualTo(c.Version, podmanHostGateway), nil
		}
	}
	return versions.GreaterThanOrEqualTo(v.APIVersion, hostGatewayAPI), nil
}

// hostEntry is a host entry as Docker takes it: a name, then ":" or "=", then
// an address (IPv4, IPv6, or host-gateway), its brackets taken off
// (unbracketed). Nothing a shell would glob or expand, or the windows alias's
// split on spaces or cmd.exe's %VAR% would read differently.
var hostEntry = regexp.MustCompile(`^[A-Za-z0-9_.-]+[:=][A-Za-z0-9_.:-]+$`)

// unbracketed is entry with the brackets around its address taken off
// (v6.test:[::1] is v6.test:::1), as Docker's CLI takes them off before it
// sends the entry to the daemon. Every docker and podman CLI takes the bare
// form, and a bracket is a glob to the shell the alias runs through.
func unbracketed(entry string) string {
	i := strings.IndexAny(entry, ":=")
	if i < 0 {
		return entry
	}
	if a := entry[i+1:]; len(a) > 2 && a[0] == '[' && a[len(a)-1] == ']' {
		return entry[:i+1] + a[1:len(a)-1]
	}
	return entry
}

// hostEntryName is the name a host entry maps.
func hostEntryName(entry string) string {
	if i := strings.IndexAny(entry, ":="); i >= 0 {
		return entry[:i]
	}
	return entry
}

// agentHostEntries is the host entries the agent container carries: the
// application's own, then host.docker.internal at host-gateway when the daemon
// resolves it and the application does not map that name itself. With strict
// (the entries go into the agent's `docker run` command), an entry that is
// not NAME:ADDRESS is refused, naming it; a compose file's go to compose as
// they are written, for it to interpolate and read.
func agentHostEntries(appHosts []string, hostGateway, strict bool) ([]string, error) {
	var out []string
	mapsDockerHost := false
	for _, e := range appHosts {
		e = strings.TrimSpace(e)
		if strict {
			e = unbracketed(strings.Trim(e, `"'`))
		}
		if e == "" {
			continue
		}
		if strict && !hostEntry.MatchString(e) {
			return nil, fmt.Errorf("the application's host entry %q is not NAME:ADDRESS: keploy cannot give it to the agent container, whose network the application joins", e)
		}
		if strings.EqualFold(hostEntryName(e), dockerHostName) {
			mapsDockerHost = true
		}
		out = append(out, e)
	}
	if hostGateway && !mapsDockerHost {
		out = append(out, dockerHostName+":host-gateway")
	}
	return out, nil
}

// agentHostFlags is agentHostEntries as `docker run` flags, each led by a
// space.
func agentHostFlags(appHosts []string, hostGateway bool) (string, error) {
	entries, err := agentHostEntries(appHosts, hostGateway, true)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	for _, e := range entries {
		b.WriteString(" --add-host ")
		b.WriteString(e)
	}
	return b.String(), nil
}

// composeHostEntries reads a compose service's extra_hosts, in either of the
// spec's forms: a list of "name:address" (or "name=address"), or a map of name
// to an address or a list of them. Aliases are read through, never copied or
// rewritten (the anchored node may be another service's too), and a map's `<<`
// merge keys bring in the names it does not set itself, the earlier source
// winning. ok is false for anything else.
func composeHostEntries(n *yaml.Node) (entries []string, ok bool) {
	n = aliasTarget(n)
	if n == nil {
		return nil, false
	}
	switch n.Kind {
	case yaml.SequenceNode:
		for _, item := range n.Content {
			item = aliasTarget(item)
			if item == nil || item.Kind != yaml.ScalarNode {
				return nil, false
			}
			entries = append(entries, item.Value)
		}
		return entries, true
	case yaml.MappingNode:
		set := map[string]bool{}
		add := func(m *yaml.Node) bool {
			for i := 0; i+1 < len(m.Content); i += 2 {
				if isMergeKey(m.Content[i]) {
					continue // mergeSources follows it
				}
				name := aliasTarget(m.Content[i])
				if name == nil || name.Kind != yaml.ScalarNode {
					return false
				}
				if set[name.Value] {
					continue
				}
				set[name.Value] = true
				addrs, ok := mapHostAddresses(m.Content[i+1])
				if !ok {
					return false
				}
				for _, a := range addrs {
					entries = append(entries, name.Value+":"+a)
				}
			}
			return true
		}
		if !add(n) {
			return nil, false
		}
		for i := 0; i+1 < len(n.Content); i += 2 {
			if !isMergeKey(n.Content[i]) {
				continue
			}
			sources, usable := mergeSources(n.Content[i+1])
			if !usable {
				return nil, false
			}
			for _, src := range sources {
				if !add(src) {
					return nil, false
				}
			}
		}
		return entries, true
	}
	return nil, false
}

// mapHostAddresses is the address, or the list of them, a map-form
// extra_hosts gives one name.
func mapHostAddresses(n *yaml.Node) ([]string, bool) {
	n = aliasTarget(n)
	if n == nil {
		return nil, false
	}
	switch n.Kind {
	case yaml.ScalarNode:
		return []string{n.Value}, true
	case yaml.SequenceNode:
		var out []string
		for _, a := range n.Content {
			a = aliasTarget(a)
			if a == nil || a.Kind != yaml.ScalarNode {
				return nil, false
			}
			out = append(out, a.Value)
		}
		return out, true
	}
	return nil, false
}

// moveExtraHostsToAgent moves the app service's extra_hosts onto the
// keploy-agent service, and gives keploy-agent host.docker.internal at
// host-gateway when hostGateway and the app does not map that name. The
// entries go on as a list, after any keploy-agent already has.
func (idc *Impl) moveExtraHostsToAgent(appService, agentService *yaml.Node, hostGateway bool) error {
	var appHosts []string
	if n := idc.getServiceProperty(appService, "extra_hosts"); n != nil {
		hosts, ok := composeHostEntries(n)
		if !ok {
			return fmt.Errorf("the app service's extra_hosts is neither a list of NAME:ADDRESS nor a map of NAME to ADDRESS: keploy cannot give it to keploy-agent, whose network the app joins")
		}
		appHosts = hosts
		idc.removeServiceProperty(appService, "extra_hosts")
	}
	var have []string
	if n := idc.getServiceProperty(agentService, "extra_hosts"); n != nil {
		hosts, ok := composeHostEntries(n)
		if !ok {
			return fmt.Errorf("keploy-agent's extra_hosts is neither a list nor a map")
		}
		have = hosts
	}
	entries, err := agentHostEntries(appHosts, hostGateway && !mapsName(have, dockerHostName), false)
	if err != nil {
		return err
	}
	if len(entries) == 0 {
		return nil
	}
	list := &yaml.Node{Kind: yaml.SequenceNode}
	for _, e := range append(have, entries...) {
		list.Content = append(list.Content, &yaml.Node{Kind: yaml.ScalarNode, Value: e, Style: yaml.DoubleQuotedStyle})
	}
	idc.setServicePropertyNode(agentService, "extra_hosts", list)
	return nil
}

// mapsName reports whether any of entries maps name.
func mapsName(entries []string, name string) bool {
	for _, e := range entries {
		if strings.EqualFold(hostEntryName(e), name) {
			return true
		}
	}
	return false
}
