//go:build linux && pidmap_integration

// Root-only, like pidmaps_integration_linux_test.go: it loads keploy's BPF
// object and attaches k_sockops, so it is behind the pidmap_integration tag.

package linux

import (
	"context"
	"fmt"
	"net"
	"testing"
	"time"

	"github.com/cilium/ebpf"
	"github.com/cilium/ebpf/link"
	"github.com/cilium/ebpf/rlimit"
	"go.uber.org/zap"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/agent"
	"go.keploy.io/server/v3/pkg/agent/acceptcount"
	"go.keploy.io/server/v3/pkg/agent/hooks/structs"
)

// loadProxyConnObject loads keploy's object into h, registers the agent with
// the proxy at port, and attaches k_sockops at the root of the cgroup
// hierarchy, as hooks.load does. It returns a func that detaches it.
func loadProxyConnObject(t *testing.T, h *Hooks, port int) func() {
	t.Helper()
	spec, err := loadBpf()
	if err != nil {
		t.Fatal(err)
	}
	if p := spec.Programs["k_sockops"]; p != nil {
		p.Type = ebpf.SockOps
		p.AttachType = ebpf.AttachCGroupSockOps
	}
	h.objectsMutex.Lock()
	err = spec.LoadAndAssign(&h.objects, nil)
	h.agentRegistrationMap = h.objects.KeployAgentRegistrationMap
	h.objectsMutex.Unlock()
	if err != nil {
		t.Fatalf("load keploy's BPF object: %v", err)
	}
	info := structs.AgentInfo{DNSPort: -1, IsDocker: 1}
	info.Proxy = structs.ProxyInfo{IP4: 0x7f000001, IP6: [4]uint32{0, 0, 0, 1}, Port: uint32(port)}
	if err := h.SendAgentInfo(info); err != nil {
		t.Fatal(err)
	}
	h.objectsMutex.Lock()
	h.proxyInfo, h.proxyInfoSet = info.Proxy, true
	h.objectsMutex.Unlock()
	cg, err := agent.DetectCgroupPath(h.logger)
	if err != nil {
		t.Fatalf("no cgroup v2 path: %v", err)
	}
	l, err := link.AttachCgroup(link.CgroupOptions{Path: cg, Attach: ebpf.AttachCGroupSockOps, Program: h.objects.K_sockops})
	if err != nil {
		t.Fatalf("attach k_sockops to %s: %v", cg, err)
	}
	return func() { _ = l.Close() }
}

// The kernel's count of the connections completed to the proxy's listener and
// the proxy's count of its accepts (acceptcount, which the proxy uses) agree
// at the hooks' targets: before anything is accepted the kernel already counts
// what waits in the accept queue; once all of it is accepted the two are
// equal; a connection to the listener at an address the hooks do not redirect
// to is in neither; and the kernel's count goes on growing across a reload of
// the object, never back to 0.
func TestProxyConnsMatchWhatTheProxyAccepts(t *testing.T) {
	if err := rlimit.RemoveMemlock(); err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", ":0") // the proxy's listener: dual-stack, every address
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	port := ln.Addr().(*net.TCPAddr).Port
	h := NewHooks(zap.NewNop(), &config.Config{})
	detach := loadProxyConnObject(t, h, port)
	t.Cleanup(func() { detach(); _ = h.objects.Close() })
	targets := h.ProxyConnTargets()
	if len(targets) != 2 {
		t.Fatalf("targets %v, want the IPv4 and IPv6 proxy addresses", targets)
	}
	ctx := context.Background()
	established := func() uint64 {
		t.Helper()
		n, err := h.ProxyConnsEstablished(ctx)
		if err != nil {
			t.Fatal(err)
		}
		return n
	}
	// waitEstablished waits for the kernel's count to reach want: a client's
	// connect returns when its side is established, the server's side a moment
	// later. No deadline but the test binary's -timeout.
	waitEstablished := func(want uint64, why string) {
		t.Helper()
		for {
			got := established()
			if got == want {
				return
			}
			if got > want {
				t.Fatalf("%s: established %d, want %d", why, got, want)
			}
			time.Sleep(time.Millisecond)
		}
	}
	dial := func(addr string) {
		t.Helper()
		c, err := net.Dial("tcp", addr)
		if err != nil {
			t.Fatalf("dial %s: %v", addr, err)
		}
		t.Cleanup(func() { _ = c.Close() })
	}

	for i := 1; i <= 3; i++ {
		dial(fmt.Sprintf("127.0.0.1:%d", port))
		waitEstablished(uint64(i), "connections nothing accepted yet")
	}
	dial(fmt.Sprintf("[::1]:%d", port))
	waitEstablished(4, "a connection to [::1]")
	dial(fmt.Sprintf("127.0.0.2:%d", port)) // not a target

	// The proxy's side, accepting all five as its accept loop does.
	var accepts acceptcount.Counter
	for i := 0; i < 5; i++ {
		c, err := ln.Accept()
		if err != nil {
			t.Fatal(err)
		}
		accepts.Add(c.LocalAddr())
		_ = c.Close()
	}
	if got, acc := established(), accepts.To(targets); got != 4 || acc != 4 {
		t.Fatalf("after accepting all five: established %d, accepted at the targets %d; want 4 and 4 (the one to 127.0.0.2 in neither)", got, acc)
	}

	// A reload of the object: the count goes on from where it was.
	detach()
	h.objectsMutex.Lock()
	h.keepProxyConnsAcrossUnload()
	_ = h.objects.Close()
	h.objectsMutex.Unlock()
	detach = loadProxyConnObject(t, h, port)
	if got := established(); got != 4 {
		t.Fatalf("after a reload: established %d, want the 4 counted before it", got)
	}
	dial(fmt.Sprintf("127.0.0.1:%d", port))
	waitEstablished(5, "a connection after the reload")
	c, err := ln.Accept()
	if err != nil {
		t.Fatal(err)
	}
	accepts.Add(c.LocalAddr())
	_ = c.Close()
	if got, acc := established(), accepts.To(targets); got != acc {
		t.Fatalf("after the reload: established %d, accepted %d; want them equal", got, acc)
	}
}
