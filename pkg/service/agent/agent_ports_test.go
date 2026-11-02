package agent

import (
	"errors"
	"testing"

	"go.keploy.io/server/v3/config"
	coreAgent "go.keploy.io/server/v3/pkg/agent"
)

type portsHooks struct {
	coreAgent.Hooks
	told   [][2]uint32
	refuse error
}

func (h *portsHooks) SetProxyPorts(proxyPort, dnsPort uint32) error {
	h.told = append(h.told, [2]uint32{proxyPort, dnsPort})
	return h.refuse
}

type portsProxy struct {
	coreAgent.Proxy
	move func(proxyPort, dnsPort uint32) error
}

func (p *portsProxy) SetPortMover(move func(proxyPort, dnsPort uint32) error) { p.move = move }

// The proxy may move off a taken port only where the hooks can be told, and
// redirect to the proxy's own port, outside Docker; a move is told to them and kept in the
// agent's config, which the agent's later readers of the ports take.
func TestTheProxysPortsMoveOnlyWhereTheHooksCanBeTold(t *testing.T) {
	px := &portsProxy{}
	a := &Agent{Proxy: px, Hooks: struct{ coreAgent.Hooks }{}, config: &config.Config{}}
	a.letProxyPortsMove()
	if px.move != nil {
		t.Fatal("hooks that cannot be told of a move: the proxy may still move")
	}

	hk := &portsHooks{}
	a.Hooks = hk
	coreAgent.EbpfProxyPortOverride = 7000
	a.letProxyPortsMove()
	coreAgent.EbpfProxyPortOverride = 0
	if px.move != nil {
		t.Fatal("a port override set: the proxy may still move off the port the hooks redirect to")
	}

	a.Proxy = struct{ coreAgent.Proxy }{} // a proxy that cannot move: nothing to let
	a.letProxyPortsMove()
	a.Proxy = px

	a.config.Agent.IsDocker = true
	a.letProxyPortsMove()
	a.config.Agent.IsDocker = false
	if px.move != nil {
		t.Fatal("in Docker: the proxy may still move off the port the CLI publishes")
	}

	a.letProxyPortsMove()
	if px.move == nil {
		t.Fatal("hooks that can be told: the proxy may not move")
	}
	if err := px.move(61001, 61002); err != nil {
		t.Fatal(err)
	}
	c := a.config
	if len(hk.told) != 1 || hk.told[0] != [2]uint32{61001, 61002} ||
		c.Agent.ProxyPort != 61001 || c.Agent.DnsPort != 61002 || c.ProxyPort != 61001 || c.DNSPort != 61002 {
		t.Fatalf("the move: hooks told %v; config agent %d/%d, root %d/%d", hk.told, c.Agent.ProxyPort, c.Agent.DnsPort, c.ProxyPort, c.DNSPort)
	}

	// Hooks that refuse the new ports: the move fails the start, and the
	// agent's config keeps the ports the hooks hold.
	hk.refuse = errors.New("refused")
	if err := px.move(61003, 61004); !errors.Is(err, hk.refuse) || c.Agent.ProxyPort != 61001 || c.DNSPort != 61002 {
		t.Fatalf("a refused move: err %v; config agent %d/%d, root %d/%d", err, c.Agent.ProxyPort, c.Agent.DnsPort, c.ProxyPort, c.DNSPort)
	}
}
