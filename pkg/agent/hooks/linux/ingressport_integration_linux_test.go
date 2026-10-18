//go:build linux && pidmap_integration

// Root-only, like the other pidmap_integration tests: it loads keploy's BPF
// objects (the main one and the ingress lookup) into the running kernel and
// attaches the lookup to this process's network namespace.

package linux

import (
	"context"
	"errors"
	"net"
	"os"
	"strconv"
	"syscall"
	"testing"
	"time"

	"github.com/cilium/ebpf"
	"github.com/cilium/ebpf/rlimit"
	"go.uber.org/zap"
	"golang.org/x/sys/unix"

	"go.keploy.io/server/v3/config"
)

// ingressHoldAgent loads keploy's main object as the agent does and returns
// the hooks, the hold not started.
func ingressHoldAgent(t *testing.T) *Hooks {
	t.Helper()
	if os.Geteuid() != 0 {
		t.Skip("needs root to load BPF programs")
	}
	if err := rlimit.RemoveMemlock(); err != nil {
		t.Fatal(err)
	}
	spec, err := loadBpf()
	if err != nil {
		t.Fatal(err)
	}
	if p := spec.Programs["k_sockops"]; p != nil {
		p.Type = ebpf.SockOps
		p.AttachType = ebpf.AttachCGroupSockOps
	}
	h := NewHooks(zap.NewNop(), &config.Config{})
	if err := spec.LoadAndAssign(&h.objects, nil); err != nil {
		t.Fatalf("load keploy's BPF object: %v", err)
	}
	t.Cleanup(func() { _ = h.objects.Close() })
	return h
}

// unusedPort is a loopback port nothing listens on now.
func unusedPort(t *testing.T) uint16 {
	t.Helper()
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()
	return uint16(p)
}

func dialPort(port uint16) (net.Conn, error) {
	return net.DialTimeout("tcp4", net.JoinHostPort("127.0.0.1", strconv.Itoa(int(port))), 10*time.Second)
}

// The agent's hold against keploy's own objects: a port the bind hooks moved
// (app_port_info, written here as they write it inside the app's bind) is
// refused while nothing holds it, as before; held, a call to it is accepted
// on the park with the dialed port as its local address, before any
// forwarder listens; once the forwarder is registered, the port's calls are
// the forwarder's. The park and the forwarder are plain TCP.
func TestAMovedPortIsHeldByTheParkUntilItsForwarderListens(t *testing.T) {
	h := ingressHoldAgent(t)
	orig, moved := unusedPort(t), unusedPort(t)
	if err := h.objects.AppPortInfo.Update(orig, appPortTranslate{ProxyPort: orig, ApplicationPort: moved}, ebpf.UpdateAny); err != nil {
		t.Fatal(err)
	}
	if c, err := dialPort(orig); err == nil {
		_ = c.Close()
		t.Fatal("the moved port answered before anything held it")
	} else if !errors.Is(err, syscall.ECONNREFUSED) {
		t.Fatalf("dial before the hold: %v, want refused", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	h.startIngressHold(ctx)
	if !h.IngressHeld() {
		t.Fatal("the hold did not start on this kernel")
	}
	park := h.IngressParkListener()
	if p := listenerProtocol(t, park); p != unix.IPPROTO_TCP {
		t.Fatalf("the park is protocol %d, want plain TCP", p)
	}
	if got, ok := h.MovedPort(orig); !ok || got != moved {
		t.Fatalf("MovedPort(%d) = %d, %v; want %d", orig, got, ok, moved)
	}

	accepted := make(chan net.Conn, 1)
	go func() {
		c, err := park.Accept()
		if err == nil {
			accepted <- c
		}
	}()
	c, err := dialPort(orig)
	if err != nil {
		t.Fatalf("dial the held port: %v, want the park to take it", err)
	}
	defer func() { _ = c.Close() }()
	select {
	case p := <-accepted:
		if got := p.LocalAddr().(*net.TCPAddr).Port; got != int(orig) {
			t.Fatalf("the parked connection's local port is %d, want the dialed %d", got, orig)
		}
		_ = p.Close()
	case <-time.After(10 * time.Second):
		t.Fatal("the park did not accept the call")
	}
	if n := h.IngressParked(); n != 1 {
		t.Fatalf("parked %d, want 1", n)
	}

	var lc net.ListenConfig
	lc.SetMultipathTCP(false)
	fwd, err := lc.Listen(context.Background(), "tcp4", net.JoinHostPort("0.0.0.0", strconv.Itoa(int(orig))))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = fwd.Close() }()
	h.RegisterIngressForwarder(orig, fwd)
	fwdAccepted := make(chan struct{}, 1)
	go func() {
		c, err := fwd.Accept()
		if err == nil {
			_ = c.Close()
			fwdAccepted <- struct{}{}
		}
	}()
	c2, err := dialPort(orig)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c2.Close() }()
	select {
	case <-fwdAccepted:
	case <-time.After(10 * time.Second):
		t.Fatal("the registered forwarder did not get the port's call")
	}
	if n := h.IngressParked(); n != 1 {
		t.Fatalf("parked %d after the forwarder registered, want still 1", n)
	}

	// Recording ends: the hold's link and park close with its context, so
	// the port is steered nowhere and, with nothing listening on it, refused,
	// as before keploy held it.
	_ = fwd.Close()
	cancel()
	for deadline := time.Now().Add(10 * time.Second); h.IngressHeld(); time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the hold did not close with its context")
		}
	}
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		c, err := dialPort(orig)
		if err != nil && errors.Is(err, syscall.ECONNREFUSED) {
			break
		}
		if c != nil {
			_ = c.Close()
		}
		if time.Now().After(deadline) {
			t.Fatalf("after the hold closed, a call to the port: %v, want it refused (the lookup and park still hold it)", err)
		}
	}
}

func listenerProtocol(t *testing.T, ln net.Listener) int {
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
