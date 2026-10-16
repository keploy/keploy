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
// A hostname is not an address to pin -- only a literal IP is accepted.
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
	return ip.String()
}
