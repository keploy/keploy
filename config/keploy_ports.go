package config

import "math"

// KeployPorts are the TCP ports keploy itself listens on in the keploy agent's
// network namespace: the agent's API (agent.port), the proxy (proxyPort) and
// the proxy's DNS server (dnsPort), as conf has them. The agent keeps a move
// of its proxy or DNS port off a taken one in its own conf.
//
// In Docker mode the app joins that namespace, so a socket listening on one of
// these ports is keploy's, never the app's (the app cannot bind a port keploy
// listens on there), and a container port among them is published for
// keploy, not by the user for the app. Both questions are answered from this
// one list, so they never disagree on which ports are keploy's.
func KeployPorts(conf *Config) map[uint16]bool {
	own := map[uint16]bool{}
	if conf == nil {
		return own
	}
	for _, p := range []uint32{conf.Agent.AgentPort, conf.Agent.ProxyPort, conf.ProxyPort, conf.Agent.DnsPort, conf.DNSPort} {
		if p != 0 && p <= math.MaxUint16 {
			own[uint16(p)] = true
		}
	}
	return own
}
