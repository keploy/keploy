package config

import "math"

// KeployListener is what of keploy's listens on one of its own ports.
type KeployListener string

const (
	KeployAPI   KeployListener = "API"        // the agent's API (agent.port)
	KeployProxy KeployListener = "proxy"      // the proxy (proxyPort)
	KeployDNS   KeployListener = "DNS server" // the proxy's DNS server (dnsPort)
)

// KeployPorts are the TCP ports keploy itself listens on in the keploy agent's
// network namespace, each with what listens there: the agent's API
// (agent.port), the proxy (proxyPort) and the proxy's DNS server (dnsPort), as
// conf has them. The agent keeps a move of its proxy or DNS port off a taken
// one in its own conf (agent.proxyPort, agent.dnsPort), and that is read last,
// so where an older value names a port the agent moved to, the agent's own
// value says what listens there.
//
// In Docker mode the app joins that namespace, so a socket listening on one of
// these ports is keploy's, never the app's (the app cannot bind a port keploy
// listens on there), and a container port among them is published for
// keploy, not by the user for the app. Both questions are answered from this
// one list, so they never disagree on which ports are keploy's.
func KeployPorts(conf *Config) map[uint16]KeployListener {
	own := map[uint16]KeployListener{}
	if conf == nil {
		return own
	}
	for _, p := range []struct {
		port uint32
		is   KeployListener
	}{
		{conf.ProxyPort, KeployProxy},
		{conf.DNSPort, KeployDNS},
		{conf.Agent.AgentPort, KeployAPI},
		{conf.Agent.ProxyPort, KeployProxy},
		{conf.Agent.DnsPort, KeployDNS},
	} {
		if p.port != 0 && p.port <= math.MaxUint16 {
			own[uint16(p.port)] = p.is
		}
	}
	return own
}
