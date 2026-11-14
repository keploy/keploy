package http

import (
	"errors"
	"fmt"
	"net"
	"runtime"
	"slices"
	"strings"
	"testing"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/utils"
)

// drawPort is where the rows draw their ports: utils.GetAvailablePort, which
// closes its probe listener before it returns, so two draws can return one
// port.
var drawPort = utils.GetAvailablePort

// holdPort draws a free port and holds it with a TCP listener until the test
// ends. A draw that returns a port held already, by this test or another
// process, cannot bind it and draws again, so no two ports a row holds meet.
func holdPort(t *testing.T) (uint32, net.Listener) {
	t.Helper()
	for i := 0; i < 16; i++ {
		p, err := drawPort()
		if err != nil {
			t.Fatalf("GetAvailablePort: %v", err)
		}
		ln, err := net.Listen("tcp", fmt.Sprintf(":%d", p))
		if err != nil {
			continue // held since it was drawn
		}
		t.Cleanup(func() { _ = ln.Close() })
		return p, ln
	}
	t.Fatal("could not hold a free port")
	return 0, nil
}

// heldTCPPort is a free port this test then holds with a TCP listener, as
// another record's check or agent would, until the test ends (holdPort).
func heldTCPPort(t *testing.T) uint32 {
	t.Helper()
	p, _ := holdPort(t)
	return p
}

// freeAtProbe gives the test a draw of ports that are free when proxyPorts
// checks them (availablePort), and not before: each is held from its draw
// (holdPort), and the check itself closes its listener just before it binds
// the port. A port drawn later is never one drawn before it, and no other
// process can take one between its draw and the check. A port proxyPorts
// does not check, as the agent's, stays held until the test ends.
//
// The rows drew free ports with utils.GetAvailablePort alone and let them go
// at once: twice, two of a row's draws returned one port, and proxyPorts
// rightly moved the second server off it.
func freeAtProbe(t *testing.T) func() uint32 {
	t.Helper()
	held := map[uint32]net.Listener{}
	was := availablePort
	t.Cleanup(func() { availablePort = was })
	availablePort = func(port uint32, exclude ...uint32) (uint32, error) {
		if ln, ok := held[port]; ok {
			delete(held, port)
			if err := ln.Close(); err != nil {
				return 0, err
			}
		}
		return was(port, exclude...)
	}
	return func() uint32 {
		t.Helper()
		p, ln := holdPort(t)
		held[p] = ln
		return p
	}
}

// onLinux sets agentRunsOnLinux for the test.
func onLinux(t *testing.T, linux bool) {
	t.Helper()
	was := agentRunsOnLinux
	agentRunsOnLinux = linux
	t.Cleanup(func() { agentRunsOnLinux = was })
}

func portsClient(proxy, dns uint32) (*AgentClient, *observer.ObservedLogs) {
	core, logs := observer.New(zapcore.WarnLevel)
	return &AgentClient{logger: zap.New(core), conf: &config.Config{ProxyPort: proxy, DNSPort: dns}}, logs
}

// A Linux agent, native or in Docker, is handed the configured proxy and DNS
// ports even while another socket holds them: its own bind moves off a taken
// one and names the holder. The CLI used to check them with a TCP listener
// first and draw others, said only at Debug, so two records started together
// each found the other's check holding a port for a moment and handed their
// agents moved ports with no line.
func TestALinuxAgentIsHandedTheConfiguredPortsEvenWhenTaken(t *testing.T) {
	for _, c := range []struct {
		name          string
		docker, linux bool
	}{
		{"native on Linux", false, true},
		{"in Docker", true, true},
		{"in Docker from macOS or Windows", true, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			onLinux(t, c.linux)
			proxy, dns := heldTCPPort(t), heldTCPPort(t)
			a, logs := portsClient(proxy, dns)
			agentPort, gotProxy, gotDNS, err := a.agentPorts(c.docker)
			if err != nil || gotProxy != proxy || gotDNS != dns || agentPort == proxy || agentPort == dns {
				t.Fatalf("agentPorts = %d, %d, %d, %v; want the configured %d, %d, as they are, and the agent elsewhere", agentPort, gotProxy, gotDNS, err, proxy, dns)
			}
			if n := logs.Len(); n != 0 {
				t.Fatalf("the CLI said %d lines (%v); the agent's bind says a move", n, logs.All())
			}
		})
	}
}

// On Linux, unset: the real agentRunsOnLinux hands a native agent its
// configured ports.
func TestAgentRunsOnLinuxOnLinux(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("agentRunsOnLinux is runtime.GOOS's")
	}
	proxy := heldTCPPort(t)
	a, _ := portsClient(proxy, heldTCPPort(t))
	if got, _, err := a.proxyPorts(false, heldTCPPort(t)); err != nil || got != proxy {
		t.Fatalf("proxyPorts = %d, %v; want the configured %d", got, err, proxy)
	}
}

// A native agent off Linux cannot move a port once its hooks are loaded: the
// CLI draws another for a port it finds taken, and says the move.
func TestANativeAgentOffLinuxIsMovedOffATakenPortAndTheMoveSaid(t *testing.T) {
	onLinux(t, false)
	freeKept := func(t *testing.T) {
		t.Helper()
		free := freeAtProbe(t)
		proxy, dns := free(), free()
		a, logs := portsClient(proxy, dns)
		gotProxy, gotDNS, err := a.proxyPorts(false, heldTCPPort(t))
		if err != nil || gotProxy != proxy || gotDNS != dns {
			t.Fatalf("proxyPorts = %d, %d, %v; want %d, %d", gotProxy, gotDNS, err, proxy, dns)
		}
		if n := logs.Len(); n != 0 {
			t.Fatalf("said %d lines for ports that did not move: %v", n, logs.All())
		}
	}
	t.Run("free ports kept, nothing said", freeKept)
	// Two draws return one port now and then (drawPort). Forced here: the
	// second draw returns the first's port, held by then, and is drawn again.
	t.Run("free ports kept when two draws return one port", func(t *testing.T) {
		was := drawPort
		t.Cleanup(func() { drawPort = was })
		var first uint32
		draws := 0
		drawPort = func() (uint32, error) {
			draws++
			if draws == 2 {
				return first, nil
			}
			p, err := was()
			if draws == 1 {
				first = p
			}
			return p, err
		}
		freeKept(t)
		if draws < 4 {
			t.Fatalf("%d draws; want the second one's port, the first's, drawn again", draws)
		}
	})
	for _, server := range []string{"proxy", "DNS server"} {
		t.Run(server+" taken", func(t *testing.T) {
			free := freeAtProbe(t)
			taken := heldTCPPort(t)
			proxy, dns := taken, free()
			if server == "DNS server" {
				proxy, dns = free(), taken
			}
			agentPort := heldTCPPort(t)
			a, logs := portsClient(proxy, dns)
			gotProxy, gotDNS, err := a.proxyPorts(false, agentPort)
			if err != nil {
				t.Fatalf("proxyPorts: %v", err)
			}
			got, kept, keptWant := gotProxy, gotDNS, dns
			if server == "DNS server" {
				got, kept, keptWant = gotDNS, gotProxy, proxy
			}
			if got == taken || got == agentPort || got == kept || kept != keptWant {
				t.Fatalf("proxyPorts = %d, %d with the %s's %d taken and the agent on %d; want the %s moved to a port of its own, the other kept", gotProxy, gotDNS, server, taken, agentPort, server)
			}
			// As the agent says its own move, naming the holder where the
			// platform can (utils.PortTaken: this test's process).
			want := fmt.Sprintf("the %s's %s; the %s listens on port %d instead", server, utils.PortTaken(taken), server, got)
			if logs.Len() != 1 || logs.All()[0].Message != want {
				t.Fatalf("said %v; want one warning %q", logs.All(), want)
			}
		})
	}
	t.Run("the DNS server's port is the proxy's", func(t *testing.T) {
		proxy := freeAtProbe(t)()
		a, _ := portsClient(proxy, proxy)
		gotProxy, gotDNS, err := a.proxyPorts(false, heldTCPPort(t))
		if err != nil || gotProxy != proxy || gotDNS == proxy {
			t.Fatalf("proxyPorts = %d, %d, %v; want the proxy on %d and the DNS server elsewhere", gotProxy, gotDNS, err, proxy)
		}
	})
	// A moved proxy is never handed the configured DNS server's port: a free
	// one is the DNS server's. Here the search's only free ports are the DNS
	// server's and one other, as utils.EnsureAvailablePorts finds them. The
	// proxy used to take the DNS server's, which then moved with a line saying
	// that a port nothing held was taken.
	t.Run("a moved proxy is not handed the DNS server's port", func(t *testing.T) {
		const taken, dns, spare, agentPort uint32 = 61000, 64000, 65000, 40000
		was := availablePort
		t.Cleanup(func() { availablePort = was })
		availablePort = func(port uint32, exclude ...uint32) (uint32, error) {
			if port != taken && !slices.Contains(exclude, port) {
				return port, nil
			}
			for _, p := range []uint32{dns, spare} {
				if !slices.Contains(exclude, p) {
					return p, nil
				}
			}
			return 0, errors.New("no port")
		}
		a, logs := portsClient(taken, dns)
		gotProxy, gotDNS, err := a.proxyPorts(false, agentPort)
		if err != nil || gotProxy != spare || gotDNS != dns {
			t.Fatalf("proxyPorts = %d, %d, %v; want the proxy moved to %d and the DNS server kept on its %d", gotProxy, gotDNS, err, spare, dns)
		}
		if logs.Len() != 1 || !strings.HasPrefix(logs.All()[0].Message, "the proxy's ") {
			t.Fatalf("said %v; want the proxy's move alone", logs.All())
		}
	})
	// The agent's port is free when proxyPorts runs, as it is before the agent
	// starts: free, it must still not be the proxy's too.
	t.Run("the proxy's port is the agent's", func(t *testing.T) {
		free := freeAtProbe(t)
		agentPort := free()
		a, _ := portsClient(agentPort, free())
		gotProxy, _, err := a.proxyPorts(false, agentPort)
		if err != nil || gotProxy == agentPort {
			t.Fatalf("proxyPorts = %d, %v; want the proxy off the agent's %d", gotProxy, err, agentPort)
		}
	})
	for _, server := range []string{"proxy", "DNS server"} {
		t.Run("no port for the "+server, func(t *testing.T) {
			was := availablePort
			t.Cleanup(func() { availablePort = was })
			exhausted := errors.New("no port")
			availablePort = func(port uint32, exclude ...uint32) (uint32, error) {
				if (server == "proxy") == (port == 16789) {
					return 0, exhausted
				}
				return port, nil
			}
			a, _ := portsClient(16789, 26789)
			_, _, err := a.proxyPorts(false, 40000)
			if !errors.Is(err, exhausted) || !strings.Contains(err.Error(), "the "+server) {
				t.Fatalf("proxyPorts: %v; want the %s's failure", err, server)
			}
		})
	}
}

// agentPorts draws the agent's port off the configured proxy's and DNS
// server's, and fails with what failed.
func TestAgentPortsKeepTheConfiguredPortsForTheProxyAndTheDNSServer(t *testing.T) {
	onLinux(t, true)
	defer func(h func() (uint32, error)) { hostPort = h }(hostPort)
	draws := []uint32{16789, 26789, 40000}
	hostPort = func() (uint32, error) {
		p := draws[0]
		draws = draws[1:]
		return p, nil
	}
	a, _ := portsClient(16789, 26789)
	if agentPort, proxy, dns, err := a.agentPorts(false); err != nil || agentPort != 40000 || proxy != 16789 || dns != 26789 {
		t.Fatalf("agentPorts = %d, %d, %d, %v; want the agent on 40000, the first draw neither configured port", agentPort, proxy, dns, err)
	}

	stop := errors.New("no port")
	hostPort = func() (uint32, error) { return 0, stop }
	if _, _, _, err := a.agentPorts(false); !errors.Is(err, stop) {
		t.Fatalf("agentPorts: %v; want the agent's picker's error", err)
	}

	onLinux(t, false)
	hostPort = func() (uint32, error) { return 40000, nil }
	was := availablePort
	t.Cleanup(func() { availablePort = was })
	availablePort = func(uint32, ...uint32) (uint32, error) { return 0, stop }
	if _, _, _, err := a.agentPorts(false); !errors.Is(err, stop) {
		t.Fatalf("agentPorts off Linux: %v; want the proxy's port check's error", err)
	}
}

// The agent's own port is never one its proxy or DNS server is handed: those
// are not bound when it is drawn.
func TestAgentPortForSkipsTheProxysAndDNSServersPorts(t *testing.T) {
	defer func(h, c func() (uint32, error)) { hostPort, containerPort = h, c }(hostPort, containerPort)
	for _, docker := range []bool{false, true} {
		draws := []uint32{16789, 26789, 40000}
		pick := func() (uint32, error) {
			p := draws[0]
			draws = draws[1:]
			return p, nil
		}
		hostPort, containerPort = pick, pick
		if got, err := agentPortFor(docker, 16789, 26789); err != nil || got != 40000 {
			t.Fatalf("docker %v: agentPortFor = %d, %v; want 40000, the first draw that is neither kept port", docker, got, err)
		}
	}
	hostPort = func() (uint32, error) { return 16789, nil }
	if got, err := agentPortFor(false, 16789, 26789); err == nil {
		t.Fatalf("agentPortFor = %d with every draw a kept port; want an error, not a hang", got)
	}
	stop := errors.New("no port")
	hostPort = func() (uint32, error) { return 0, stop }
	if _, err := agentPortFor(false, 16789, 26789); !errors.Is(err, stop) {
		t.Fatalf("agentPortFor: %v; want the picker's error", err)
	}
}
