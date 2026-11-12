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
// redirect to the proxy's own port, in Docker as outside it; a move is told to
// them and kept in the agent's config, which the agent's later readers of the
// ports take.
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

	// Natively, and in Docker, where the agent's namespace holds ports of
	// Docker's own (its embedded DNS resolver's): the proxy may move off them,
	// and the move is told to the hooks and kept in the config.
	for _, docker := range []bool{false, true} {
		px.move, hk.told = nil, nil
		a.config = &config.Config{}
		a.config.Agent.IsDocker = docker
		a.letProxyPortsMove()
		if px.move == nil {
			t.Fatalf("docker %v, hooks that can be told: the proxy may not move", docker)
		}
		if err := px.move(61001, 61002); err != nil {
			t.Fatal(err)
		}
		c := a.config
		if len(hk.told) != 1 || hk.told[0] != [2]uint32{61001, 61002} ||
			c.Agent.ProxyPort != 61001 || c.Agent.DnsPort != 61002 || c.ProxyPort != 61001 || c.DNSPort != 61002 {
			t.Fatalf("docker %v, the move: hooks told %v; config agent %d/%d, root %d/%d", docker, hk.told, c.Agent.ProxyPort, c.Agent.DnsPort, c.ProxyPort, c.DNSPort)
		}
	}
	c := a.config

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

// The agent's start lets the proxy move before it starts it, in record and
// in replay: an agent whose ports another socket holds starts, and its hooks
// and config get the ports the proxy moved to. Natively the holder is another
// run; in Docker it is Docker's embedded DNS resolver, which listens in the
// agent container's namespace on ports of its ephemeral range before the
// agent runs, in either mode.
func TestHookLetsTheProxyMoveOffTakenPortsBeforeItStarts(t *testing.T) {
	for _, mode := range []models.Mode{models.MODE_RECORD, models.MODE_TEST} {
		for _, docker := range []bool{false, true} {
			hk, px := &loadedPortsHooks{}, &movingProxy{}
			a := &Agent{logger: zap.NewNop(), Hooks: hk, Proxy: px, config: &config.Config{}}
			a.config.Agent.Mode, a.config.Agent.IsDocker = mode, docker
			grp, ctx := errgroup.WithContext(context.Background())
			ctx, cancel := context.WithCancel(context.WithValue(ctx, models.ErrGroupKey, grp))

			err := a.Hook(ctx, models.HookOptions{Mode: mode, IsDocker: docker})
			cancel()
			_ = grp.Wait()
			if err != nil {
				t.Fatalf("%s, docker %v: the agent did not start: %v", mode, docker, err)
			}
			if px.opts == nil {
				t.Fatalf("%s, docker %v: the agent never started its proxy", mode, docker)
			}
			c := a.config
			if len(hk.told) != 1 || hk.told[0] != [2]uint32{61001, 61002} ||
				c.Agent.ProxyPort != 61001 || c.Agent.DnsPort != 61002 || c.ProxyPort != 61001 || c.DNSPort != 61002 {
				t.Fatalf("%s, docker %v: the hooks were told %v, the config holds agent %d/%d, root %d/%d; want [[61001 61002]] in all", mode, docker, hk.told, c.Agent.ProxyPort, c.Agent.DnsPort, c.ProxyPort, c.DNSPort)
			}
		}
	}
}
