//go:build linux

package proxy

import (
	"context"
	"errors"
	"net"
	"strconv"
	"syscall"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
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

// v6Listener listens on addr over IPv6 only, or skips the test where this
// host has no IPv6.
func v6Listener(t *testing.T, addr string) net.Listener {
	t.Helper()
	lc := net.ListenConfig{Control: func(_, _ string, rc syscall.RawConn) error {
		var serr error
		if err := rc.Control(func(fd uintptr) { serr = unix.SetsockoptInt(int(fd), unix.SOL_IPV6, unix.IPV6_FREEBIND, 1) }); err != nil {
			return err
		}
		return serr
	}}
	ln, err := lc.Listen(context.Background(), "tcp6", addr) // tcp6: IPV6_V6ONLY, also on [::]
	if err != nil {
		t.Skipf("no IPv6 listener on %s here: %v", addr, err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	return ln
}

// Under the hold, a forwarder whose app bound its port over IPv6 (Go's
// net.Listen("tcp", ":P") binds [::]:P, dual-stack) checks the port on IPv6
// too, since its own IPv4 bind does not find a listener there. Another
// listener on [::1]:P (Node's listen(P, "localhost") where localhost is ::1
// first), or on [::]:P with IPV6_V6ONLY, holds the port: the hold yields the
// whole port to the kernel, as without keploy (the app's own bind would have
// failed), with one WARN naming the port and IPv6, and no forwarder starts
// (its IPv4 bind is let go). With nothing there, or for an app bound over
// IPv4 (the hold takes none of its port's IPv6 calls, so an IPv6 listener
// keeps them anyway), or without a hold, the forwarder starts.
func TestAForwarderYieldsAPortAListenerHoldsOnIPv6(t *testing.T) {
	saved := ingressTargetWait
	t.Cleanup(func() { ingressTargetWait = saved })
	waited := 0
	ingressTargetWait = func(context.Context, uint16, string, time.Duration) (bool, error) {
		waited++
		return true, nil
	}
	start := func(t *testing.T, orig uint16, held, ipv6 bool) (*fakePortHolder, *observer.ObservedLogs, error) {
		t.Helper()
		holder := &fakePortHolder{held: held}
		core, logs := observer.New(zapcore.WarnLevel)
		pm := &IngressProxyManager{logger: zap.New(core), tcChan: make(chan *models.TestCase, 4), samplingSem: make(chan struct{}, 1), portHolder: holder}
		if ipv6 {
			pm.ipv6Binds = map[uint16]bool{orig: true}
		}
		g := newGoTCPIngressHook(pm)
		err := g.StartIngress(context.Background(), orig, portOf(t, freeAddr(t)))
		if err == nil {
			_ = g.StopIngress(orig)
		}
		return holder, logs, err
	}
	for _, x := range []string{"[::1]:0", "[::]:0", "[fd00::5]:0"} {
		t.Run(x, func(t *testing.T) {
			// [fd00::5] is an address the host need not have (bound with
			// IPV6_FREEBIND): another of the host's IPv6 addresses, which
			// only a bind of [::] finds.
			other := v6Listener(t, x)
			orig := portOf(t, other.Addr().String())

			waited = 0
			holder, logs, err := start(t, orig, true, true)
			if !errors.Is(err, errIngressPortYielded) || !errors.Is(err, syscall.EADDRINUSE) {
				t.Fatalf("an IPv6 app's forwarder, with %s on its port: %v, want the port yielded (EADDRINUSE)", other.Addr(), err)
			}
			holder.mu.Lock()
			yielded, registered := holder.yielded, len(holder.registered)
			holder.mu.Unlock()
			if len(yielded) != 1 || yielded[0] != orig || registered != 0 || waited != 0 {
				t.Fatalf("yielded %v, registered %d, waited %d; want [%d], no forwarder and no wait", yielded, registered, waited, orig)
			}
			warns := logs.FilterMessageSnippet("another listener, not keploy's, holds the application's port").All()
			if len(warns) != 1 || warns[0].ContextMap()["port"] != orig || warns[0].ContextMap()["family"] != "IPv6" {
				t.Fatalf("WARNs %v, want one naming port %d and IPv6", logs.All(), orig)
			}
			ln, err := net.Listen("tcp4", net.JoinHostPort("0.0.0.0", strconv.Itoa(int(orig))))
			if err != nil {
				t.Fatalf("the yielding forwarder kept its IPv4 bind of the port: %v", err)
			}
			_ = ln.Close()

			for _, c := range []struct {
				name       string
				held, ipv6 bool
			}{{"an IPv4 app", true, false}, {"no hold", false, true}} {
				holder, logs, err := start(t, orig, c.held, c.ipv6)
				if err != nil || len(holder.yielded) != 0 || logs.Len() != 0 {
					t.Fatalf("%s, with %s on its port: %v, yielded %v, logged %v; want the forwarder started", c.name, other.Addr(), err, holder.yielded, logs.All())
				}
			}
		})
	}
	t.Run("no IPv6 here", func(t *testing.T) {
		savedProbe := ingressIPv6Probe
		t.Cleanup(func() { ingressIPv6Probe = savedProbe })
		ingressIPv6Probe = func(uint16) error { return syscall.EAFNOSUPPORT }
		orig := portOf(t, freeAddr(t))
		holder, logs, err := start(t, orig, true, true)
		if err != nil || len(holder.yielded) != 0 || logs.Len() != 0 {
			t.Fatalf("an IPv6 app's forwarder on a host whose IPv6 check fails (EAFNOSUPPORT): %v, yielded %v, logged %v; want it started, the port held", err, holder.yielded, logs.All())
		}
	})
	t.Run("nothing on IPv6", func(t *testing.T) {
		v6Listener(t, "[::1]:0") // IPv6 is here
		orig := portOf(t, freeAddr(t))
		holder, logs, err := start(t, orig, true, true)
		if err != nil || len(holder.yielded) != 0 || logs.Len() != 0 {
			t.Fatalf("an IPv6 app's forwarder, nothing else on its port: %v, yielded %v, logged %v; want it started (its own IPv4 bind is no IPv6 listener)", err, holder.yielded, logs.All())
		}
	})
}

// The bind events say which of the app's ports it bound over IPv6, for the
// forwarder's IPv6 check; recording's end forgets them.
func TestTheBindEventsMarkTheIPv6Ports(t *testing.T) {
	events := make(chan models.IngressEvent, 2)
	events <- models.IngressEvent{Family: unix.AF_INET6, OrigAppPort: 8106, NewAppPort: 41006}
	events <- models.IngressEvent{Family: unix.AF_INET, OrigAppPort: 8104, NewAppPort: 41004}
	hook := gatedIngressHook{entered: make(chan uint16, 2), release: make(chan struct{})}
	close(hook.release)
	pm := &IngressProxyManager{logger: zap.NewNop(), active: make(map[uint16]proxyStop), ingressHook: hook,
		hooks: holdingHooks{fakePortHolder: &fakePortHolder{}, events: events}}
	done := make(chan struct{})
	go func() { pm.ListenForIngressEvents(context.Background()); close(done) }()
	<-hook.entered
	<-hook.entered
	if !pm.appBoundIPv6(8106) || pm.appBoundIPv6(8104) {
		t.Fatalf("IPv6 marks: 8106 %v, 8104 %v; want only the IPv6 bind's port", pm.appBoundIPv6(8106), pm.appBoundIPv6(8104))
	}
	close(events)
	<-done
	if pm.appBoundIPv6(8106) {
		t.Fatal("the IPv6 mark outlived recording's end")
	}
}

// The IPv6 probe finds a listener, not a connection: a socket still bound to
// [::1]:P that does not listen (a connection an old listener accepted, or
// one in TIME_WAIT after it) holds no calls, so the probe binds past it, as
// Go's own listeners do (SO_REUSEADDR). Once a listener is there, it does
// not.
func TestTheIPv6ProbeFindsAListenerNotAConnection(t *testing.T) {
	ln := v6Listener(t, "[::1]:0")
	port := portOf(t, ln.Addr().String())
	if err := probeIPv6Port(port); !errors.Is(err, syscall.EADDRINUSE) {
		t.Fatalf("the probe, with a listener on [::1]:%d: %v, want EADDRINUSE", port, err)
	}
	c, err := net.Dial("tcp6", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	s, err := ln.Accept()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	_ = ln.Close()
	if err := probeIPv6Port(port); err != nil {
		t.Fatalf("the probe, with only an accepted connection on [::1]:%d: %v, want it to bind", port, err)
	}
}
