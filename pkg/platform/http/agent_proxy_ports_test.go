package http

import (
	"errors"
	"fmt"
	"net"
	"runtime"
	"strings"
	"testing"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/utils"
)

// heldTCPPort is a free port this test then holds with a TCP listener, as
// another record's check or agent would, until the test ends.
func heldTCPPort(t *testing.T) uint32 {
	t.Helper()
	for i := 0; i < 16; i++ {
		p, err := utils.GetAvailablePort()
		if err != nil {
			t.Fatalf("GetAvailablePort: %v", err)
		}
		ln, err := net.Listen("tcp", fmt.Sprintf(":%d", p))
		if err != nil {
			continue // taken since it was drawn
		}
		t.Cleanup(func() { _ = ln.Close() })
		return p
	}
	t.Fatal("could not hold a free port")
	return 0
}

// freePort is a port free when it is drawn.
func freePort(t *testing.T) uint32 {
	t.Helper()
	p, err := utils.GetAvailablePort()
	if err != nil {
		t.Fatalf("GetAvailablePort: %v", err)
	}
	return p
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
	a, _ := portsClient(proxy, freePort(t))
	if got, _, err := a.proxyPorts(false, freePort(t)); err != nil || got != proxy {
		t.Fatalf("proxyPorts = %d, %v; want the configured %d", got, err, proxy)
	}
}

// A native agent off Linux cannot move a port once its hooks are loaded: the
// CLI draws another for a port it finds taken, and says the move.
func TestANativeAgentOffLinuxIsMovedOffATakenPortAndTheMoveSaid(t *testing.T) {
	onLinux(t, false)
	t.Run("free ports kept, nothing said", func(t *testing.T) {
		proxy, dns := freePort(t), freePort(t)
		a, logs := portsClient(proxy, dns)
		gotProxy, gotDNS, err := a.proxyPorts(false, freePort(t))
		if err != nil || gotProxy != proxy || gotDNS != dns {
			t.Fatalf("proxyPorts = %d, %d, %v; want %d, %d", gotProxy, gotDNS, err, proxy, dns)
		}
		if n := logs.Len(); n != 0 {
			t.Fatalf("said %d lines for ports that did not move: %v", n, logs.All())
		}
	})
	for _, server := range []string{"proxy", "DNS server"} {
		t.Run(server+" taken", func(t *testing.T) {
			proxy, dns := freePort(t), freePort(t)
			taken := &proxy
			if server == "DNS server" {
				taken = &dns
			}
			*taken = heldTCPPort(t)
			agentPort := freePort(t)
			a, logs := portsClient(proxy, dns)
			gotProxy, gotDNS, err := a.proxyPorts(false, agentPort)
			if err != nil {
				t.Fatalf("proxyPorts: %v", err)
			}
			got, kept, keptWant := gotProxy, gotDNS, dns
			if server == "DNS server" {
				got, kept, keptWant = gotDNS, gotProxy, proxy
			}
			if got == *taken || got == agentPort || got == kept || kept != keptWant {
				t.Fatalf("proxyPorts = %d, %d with the %s's %d taken and the agent on %d; want the %s moved to a port of its own, the other kept", gotProxy, gotDNS, server, *taken, agentPort, server)
			}
			want := fmt.Sprintf("the %s's port %d is taken (", server, *taken)
			wantEnd := fmt.Sprintf("; the %s listens on port %d instead", server, got)
			if logs.Len() != 1 || !strings.HasPrefix(logs.All()[0].Message, want) || !strings.HasSuffix(logs.All()[0].Message, wantEnd) {
				t.Fatalf("said %v; want one warning %q...%q", logs.All(), want, wantEnd)
			}
		})
	}
	t.Run("the DNS server's port is the proxy's", func(t *testing.T) {
		proxy := freePort(t)
		a, _ := portsClient(proxy, proxy)
		gotProxy, gotDNS, err := a.proxyPorts(false, freePort(t))
		if err != nil || gotProxy != proxy || gotDNS == proxy {
			t.Fatalf("proxyPorts = %d, %d, %v; want the proxy on %d and the DNS server elsewhere", gotProxy, gotDNS, err, proxy)
		}
	})
	t.Run("the proxy's port is the agent's", func(t *testing.T) {
		agentPort := freePort(t)
		a, _ := portsClient(agentPort, freePort(t))
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
				if (server == "proxy") == (len(exclude) == 1) {
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
