//go:build linux && proxyconns_integration

// Root-only: it loads keploy's BPF object and attaches k_sockops.

package linux

import (
	"context"
	"net"
	"testing"

	"go.uber.org/zap"
	"golang.org/x/sys/unix"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/agent/hooks/structs"
	"go.keploy.io/server/v3/pkg/agent/proxy"
)

// The keploy instance the connect hook stamps on an application's
// connection (dest_info_t's instance) reaches the proxy with the connection
// it accepted, beside its destination and its caller's pid: one connection
// in an instance, one in none, each its own.
func TestTheProxyReadsEachConnectionsOwnInstance(t *testing.T) {
	dst := map[string]structs.DestInfo{
		"x": {IPVersion: 4, DestIP4: 0x7f000001, DestPort: 1111, KernelPid: 1001, Instance: 7},
		"y": {IPVersion: 4, DestIP4: 0x7f000001, DestPort: 2222, KernelPid: 2002},
	}

	h := NewHooks(zap.NewNop(), &config.Config{})
	loadProxyConnObject(t, h)
	ln, markErr, err := proxy.ListenMarked(context.Background(), "127.0.0.1:0", h.MarkProxyListener)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	if markErr != nil {
		t.Fatalf("mark: %v", markErr)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	info := structs.AgentInfo{DNSPort: -1, IsDocker: 1}
	info.Proxy = structs.ProxyInfo{IP4: 0x7f000001, IP6: [4]uint32{0, 0, 0, 1}, Port: uint32(port)}
	if err := h.SendAgentInfo(info); err != nil {
		t.Fatal(err)
	}
	for _, who := range []string{"x", "y"} {
		fd, _ := redirectedTo(t, h, false, 0, dst[who])
		connectTo(t, fd, port)
		if _, err := unix.Write(fd, []byte(who)); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 2; i++ {
		c, who := acceptOne(t, ln)
		pr, err := h.ProxyConnPair(mustRaw(t, c.(*net.TCPConn)))
		if err != nil {
			t.Fatal(err)
		}
		if pr.Dst == nil {
			t.Errorf("connection %s: the kernel paired it with no application connection", who)
			continue
		}
		if pr.Dst.Port != dst[who].DestPort || pr.Dst.KernelPid != dst[who].KernelPid || pr.Dst.Instance != dst[who].Instance {
			t.Errorf("connection %s paired to port %d for pid %d in instance %d, want its own port %d, pid %d, instance %d",
				who, pr.Dst.Port, pr.Dst.KernelPid, pr.Dst.Instance, dst[who].DestPort, dst[who].KernelPid, dst[who].Instance)
		}
	}
}
