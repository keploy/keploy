//go:build linux

package proxy

import (
	"context"
	"net"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
	"golang.org/x/sys/unix"
)

func protocolOf(t *testing.T, ln net.Listener) int {
	t.Helper()
	rc, err := ln.(*net.TCPListener).SyscallConn()
	if err != nil {
		t.Fatal(err)
	}
	var p int
	var gerr error
	if err := rc.Control(func(fd uintptr) { p, gerr = unix.GetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_PROTOCOL) }); err != nil {
		t.Fatal(err)
	}
	if gerr != nil {
		t.Fatal(gerr)
	}
	return p
}

// The forwarder's listener is plain TCP, whatever Go's default: a Go
// listener is MPTCP wherever the kernel has MPTCP (Go 1.24 on), and the
// hold's sockhash, which the forwarder is registered in, refuses an MPTCP
// socket.
func TestTheForwardersListenerIsPlainTCP(t *testing.T) {
	def, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("Go's default listener here is protocol %d (%d is MPTCP)", protocolOf(t, def), unix.IPPROTO_MPTCP)
	_ = def.Close()

	holder := &fakePortHolder{held: true}
	pm := &IngressProxyManager{logger: zap.NewNop(), tcChan: make(chan *models.TestCase, 4), samplingSem: make(chan struct{}, 1), portHolder: holder}
	saved := ingressTargetWait
	t.Cleanup(func() { ingressTargetWait = saved })
	ingressTargetWait = func(context.Context, uint16, string, time.Duration) (bool, error) { return true, nil }
	orig := portOf(t, freeAddr(t))
	g := newGoTCPIngressHook(pm)
	if err := g.StartIngress(context.Background(), orig, portOf(t, freeAddr(t))); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = g.StopIngress(orig) })
	holder.mu.Lock()
	ln := holder.registered[orig]
	holder.mu.Unlock()
	if ln == nil {
		t.Fatal("no forwarder registered")
	}
	if p := protocolOf(t, ln); p != unix.IPPROTO_TCP {
		t.Fatalf("the forwarder's listener is protocol %d, want plain TCP (%d)", p, unix.IPPROTO_TCP)
	}
}
