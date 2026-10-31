package docker

import (
	"net"
	"net/url"
	"strings"

	"go.keploy.io/server/v3/config"
)

// presetAgentIP returns the IP the caller pinned the agent to, or "" when the
// caller did not choose an address and loopback plus a published port is used.
//
// Derived from Agent.AgentURI rather than carried as a second setting: one
// value then decides both where the agent IS and where it is DIALLED, and the
// two cannot drift into a stack whose agent is unreachable.
//
// A hostname is not an address to pin -- only a literal IP is accepted, and
// only one INSIDE InternalSubnet.
//
// The subnet check is not belt-and-braces: the pinned address is written into
// the generated compose file as `ipv4_address` on the keploy network, and
// Docker refuses an address the network has no subnet for --
// "user specified IP address is supported only when connecting to networks with
// user configured subnets". Accepting any non-loopback IPv4 meant an agentURI
// of http://10.0.0.5:16789/agent failed compose-up rather than falling back to
// the published-port path it was asking for, and 0.0.0.0 was accepted too.
//
// It also gives enterprise ONE predicate. Its fence keys on `== AgentInternalIP`
// while Test.Host, the port-remap skip and the readiness URL key on "is
// anything pinned", so an address outside the subnet took the pinned path
// everywhere except the fence -- the open thread in its own review. Inside the
// subnet, the two questions have the same answer.
func PresetAgentIP(conf *config.Config) string {
	if conf == nil {
		return ""
	}
	raw := strings.TrimSpace(conf.Agent.AgentURI)
	if raw == "" {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	host := u.Hostname()
	ip := net.ParseIP(host)
	if ip == nil || ip.To4() == nil {
		return ""
	}
	if ip.IsLoopback() {
		return ""
	}
	// Inside the network keploy actually creates, or it is not pinnable.
	_, internal, err := net.ParseCIDR(InternalSubnet)
	if err != nil || internal == nil || !internal.Contains(ip) {
		return ""
	}
	return ip.String()
}
