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

// reusedPortConns makes two of the application's connections to the proxy's
// listener ln (at port) from one source port, while nothing accepts: X, a
// call to dstX, connects, writes "x" and resets (SO_LINGER 0); Y, a call to
// dstY from another process's stamp, then connects from X's port and writes
// "y". It returns that port.
func reusedPortConns(t *testing.T, h *Hooks, port int, dstX, dstY structs.DestInfo) int {
	t.Helper()
	x, closeX := redirectedTo(t, h, false, 0, dstX)
	p := connectTo(t, x, port)
	if _, err := unix.Write(x, []byte("x")); err != nil {
		t.Fatal(err)
	}
	if err := unix.SetsockoptLinger(x, unix.SOL_SOCKET, unix.SO_LINGER, &unix.Linger{Onoff: 1, Linger: 0}); err != nil {
		t.Fatal(err)
	}
	closeX()
	y, _ := redirectedTo(t, h, false, p, dstY)
	if got := connectTo(t, y, port); got != p {
		t.Fatalf("Y's source port %d, want X's %d", got, p)
	}
	if _, err := unix.Write(y, []byte("y")); err != nil {
		t.Fatal(err)
	}
	return p
}

// acceptOne accepts the next connection on ln and reads its first byte: which
// application connection it is.
func acceptOne(t *testing.T, ln net.Listener) (net.Conn, string) {
	t.Helper()
	c, err := ln.Accept()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	b := make([]byte, 1)
	if _, err := c.Read(b); err != nil {
		t.Fatalf("read the accepted connection's first byte: %v", err)
	}
	return c, string(b)
}

// A source port reused while the proxy has not yet taken the connection
// that held it: the kernel's pairing, which the proxy routes by under the
// proven switch, still gives each connection its own destination and its own
// caller's pid, never the other's. Who made a call is read from this
// pairing, so it must hold when the proxy is slow.
func TestAReusedSourcePortKeepsEachConnectionsOwnPairing(t *testing.T) {
	dstX := structs.DestInfo{IPVersion: 4, DestIP4: 0x7f000001, DestPort: 1111, KernelPid: 1001}
	dstY := structs.DestInfo{IPVersion: 4, DestIP4: 0x7f000001, DestPort: 2222, KernelPid: 2002}
	want := map[string]uint32{"x": 1111, "y": 2222}
	wantPid := map[string]uint32{"x": 1001, "y": 2002}

	// The pairing is kept in the accepted connection's own socket
	// (ProxyConnPair), not by the source port both connections share.
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
	reusedPortConns(t, h, port, dstX, dstY)
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
		if pr.Dst.Port != want[who] || pr.Dst.KernelPid != wantPid[who] {
			t.Errorf("connection %s paired to port %d for pid %d, want its own port %d and pid %d", who, pr.Dst.Port, pr.Dst.KernelPid, want[who], wantPid[who])
		}
	}
}
