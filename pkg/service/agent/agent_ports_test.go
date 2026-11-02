package agent

import (
	"context"
	"errors"
	"testing"

	"go.keploy.io/server/v3/config"
	coreAgent "go.keploy.io/server/v3/pkg/agent"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
	"golang.org/x/sync/errgroup"
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

// loadedPortsHooks loads, and can then be told the proxy's ports.
type loadedPortsHooks struct {
	loadedHooks
	told [][2]uint32
}

func (h *loadedPortsHooks) SetProxyPorts(proxyPort, dnsPort uint32) error {
	h.told = append(h.told, [2]uint32{proxyPort, dnsPort})
	return nil
}

// movingProxy starts as a proxy whose ports another socket holds does: it
// binds others and tells the port mover it was given before its start, if
// any; with none, its start fails.
type movingProxy struct {
	startedProxy
	move func(proxyPort, dnsPort uint32) error
}

func (p *movingProxy) SetPortMover(move func(proxyPort, dnsPort uint32) error) { p.move = move }

func (p *movingProxy) StartProxy(ctx context.Context, opts coreAgent.ProxyOptions) error {
	_ = p.startedProxy.StartProxy(ctx, opts)
	if p.move == nil {
		return errors.New("the proxy's ports are taken, and it was given no port mover by its start")
	}
	return p.move(61001, 61002)
}

// The agent's start lets the proxy move before it starts it: a native agent
// whose ports another run holds starts, and its hooks and config get the
// ports the proxy moved to.
func TestHookLetsTheProxyMoveOffTakenPortsBeforeItStarts(t *testing.T) {
	hk, px := &loadedPortsHooks{}, &movingProxy{}
	a := &Agent{logger: zap.NewNop(), Hooks: hk, Proxy: px, config: &config.Config{}}
	grp, ctx := errgroup.WithContext(context.Background())
	ctx, cancel := context.WithCancel(context.WithValue(ctx, models.ErrGroupKey, grp))
	defer func() {
		cancel()
		_ = grp.Wait()
	}()

	if err := a.Hook(ctx, models.HookOptions{Mode: models.MODE_RECORD}); err != nil {
		t.Fatalf("the agent did not start: %v", err)
	}
	if px.opts == nil {
		t.Fatal("the agent never started its proxy")
	}
	c := a.config
	if len(hk.told) != 1 || hk.told[0] != [2]uint32{61001, 61002} || c.Agent.ProxyPort != 61001 || c.Agent.DnsPort != 61002 {
		t.Fatalf("the hooks were told %v, the config holds %d/%d; want [[61001 61002]] in both", hk.told, c.Agent.ProxyPort, c.Agent.DnsPort)
	}
}
