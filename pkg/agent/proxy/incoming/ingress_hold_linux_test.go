//go:build linux

package proxy

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"strconv"
	"syscall"
	"testing"

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
	start := func(t *testing.T, orig uint16, held, ipv6 bool) (*fakePortHolder, *observer.ObservedLogs, error) {
		t.Helper()
		holder := &fakePortHolder{held: held}
		core, logs := observer.New(zapcore.WarnLevel)
		pm := &IngressProxyManager{logger: zap.New(core), tcChan: make(chan *models.TestCase, 4), samplingSem: make(chan struct{}, 1), portHolder: holder}
		if ipv6 {
			holder.ipv6 = map[uint16][]netip.Addr{orig: {netip.IPv6Unspecified()}}
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

			holder, logs, err := start(t, orig, true, true)
			if !errors.Is(err, errIngressPortYielded) || !errors.Is(err, syscall.EADDRINUSE) {
				t.Fatalf("an IPv6 app's forwarder, with %s on its port: %v, want the port yielded (EADDRINUSE)", other.Addr(), err)
			}
			holder.mu.Lock()
			yielded, registered := holder.yielded, len(holder.registered)
			holder.mu.Unlock()
			if len(yielded) != 1 || yielded[0] != orig || registered != 0 {
				t.Fatalf("yielded %v, registered %d; want [%d] and no forwarder", yielded, registered, orig)
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
		ingressIPv6Probe = func(netip.Addr, uint16) error { return syscall.EAFNOSUPPORT }
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

// The forwarder checks IPv6 exactly where the app's binds take IPv6 calls
// (the hold's notes), as the app's own bind would have conflicted without
// keploy, never by the socket's family alone. An app on ::ffff:127.0.0.1
// (Java's bind of 127.0.0.1 on a dual-stack host) takes IPv4 only, so a
// listener on [::1]:P keeps ::1 and the app's port is held, as on main. An
// app on :: (dual-stack or IPV6_V6ONLY) conflicts with that listener, and
// the port is yielded. An app on ::1 conflicts with it too, but not with a
// listener on another IPv6 address ([fd00::5]:P), which a check of [::]
// would find. Each address the binds take is checked, the conflicting one
// first or last. When the hooks cannot say what the binds take, the whole of
// IPv6 is checked: a hold must never take calls from a listener keploy does
// not own.
func TestTheIPv6CheckMirrorsWhatTheAppsBindsTake(t *testing.T) {
	any6, loop6 := netip.IPv6Unspecified(), netip.IPv6Loopback()
	for _, c := range []struct {
		name, x string
		binds   []netip.Addr
		err     error
		yield   bool
	}{
		{"an app on ::ffff:127.0.0.1, a listener on [::1]", "[::1]:0", nil, nil, false},
		{"an app on :: (dual-stack or IPV6_V6ONLY), a listener on [::1]", "[::1]:0", []netip.Addr{any6}, nil, true},
		{"an app on ::1, a listener on [::1]", "[::1]:0", []netip.Addr{loop6}, nil, true},
		{"an app on ::1, a listener on [fd00::5]", "[fd00::5]:0", []netip.Addr{loop6}, nil, false},
		{"an app on ::1 and ::, a listener on [fd00::5]", "[fd00::5]:0", []netip.Addr{loop6, any6}, nil, true},
		{"an app on ::1 and fd00::6, a listener on [::1]", "[::1]:0", []netip.Addr{loop6, netip.MustParseAddr("fd00::6")}, nil, true},
		{"binds the hooks cannot read, a listener on [fd00::5]", "[fd00::5]:0", nil, errors.New("iteration aborted"), true},
	} {
		t.Run(c.name, func(t *testing.T) {
			other := v6Listener(t, c.x)
			orig := portOf(t, other.Addr().String())
			holder := &fakePortHolder{held: true, ipv6: map[uint16][]netip.Addr{orig: c.binds}, ipv6Err: c.err}
			core, logs := observer.New(zapcore.WarnLevel)
			pm := &IngressProxyManager{logger: zap.New(core), tcChan: make(chan *models.TestCase, 4), samplingSem: make(chan struct{}, 1), portHolder: holder}
			g := newGoTCPIngressHook(pm)
			err := g.StartIngress(context.Background(), orig, portOf(t, freeAddr(t)))
			if err == nil {
				_ = g.StopIngress(orig)
			}
			holder.mu.Lock()
			yielded, registered := holder.yielded, len(holder.registered)
			holder.mu.Unlock()
			if c.yield {
				if !errors.Is(err, errIngressPortYielded) || !errors.Is(err, syscall.EADDRINUSE) || len(yielded) != 1 || registered != 0 || logs.Len() != 1 {
					t.Fatalf("%v; yielded %v, registered %d, logged %v; want the port yielded with one WARN", err, yielded, registered, logs.All())
				}
				return
			}
			if err != nil || len(yielded) != 0 || registered != 1 || logs.Len() != 0 {
				t.Fatalf("%v; yielded %v, registered %d, logged %v; want the forwarder started, the port held", err, yielded, registered, logs.All())
			}
		})
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
	if err := probeIPv6Port(netip.IPv6Unspecified(), port); !errors.Is(err, syscall.EADDRINUSE) {
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
	if err := probeIPv6Port(netip.IPv6Unspecified(), port); err != nil {
		t.Fatalf("the probe, with only an accepted connection on [::1]:%d: %v, want it to bind", port, err)
	}
}

// The hold is Linux's (eBPF), so a port is yielded only on Linux: these rows
// make a held port on a host where one cannot be (on Windows the taken
// port's bind fails with WSAEADDRINUSE, which is not syscall.EADDRINUSE).

// A forwarder that cannot listen on the app's port because another listener
// (not keploy's) holds it yields the hold's port to that listener, with a
// WARN that names the port, and is not started: the port's calls are that
// listener's, as without keploy. Without a hold there is nothing to yield;
// a forwarder that fails for another reason (the agent stopping) yields
// nothing.
func TestAForwarderThatFindsItsPortTakenYieldsTheHold(t *testing.T) {
	other, err := net.Listen("tcp4", "0.0.0.0:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = other.Close() })
	orig := portOf(t, other.Addr().String())

	for _, held := range []bool{true, false} {
		holder := &fakePortHolder{held: held}
		core, logs := observer.New(zapcore.WarnLevel)
		pm := &IngressProxyManager{logger: zap.New(core), tcChan: make(chan *models.TestCase, 4), samplingSem: make(chan struct{}, 1), portHolder: holder}
		err := newGoTCPIngressHook(pm).StartIngress(context.Background(), orig, portOf(t, freeAddr(t)))
		if !errors.Is(err, syscall.EADDRINUSE) {
			t.Fatalf("held %v: StartIngress on a taken port: %v, want EADDRINUSE", held, err)
		}
		holder.mu.Lock()
		yielded, registered := holder.yielded, len(holder.registered)
		holder.mu.Unlock()
		warns := logs.FilterMessageSnippet("another listener, not keploy's, holds the application's port").All()
		if !held {
			if len(yielded) != 0 || len(warns) != 0 {
				t.Fatalf("without a hold: yielded %v, %d WARNs, want nothing yielded", yielded, len(warns))
			}
			continue
		}
		if len(yielded) != 1 || yielded[0] != orig || registered != 0 {
			t.Fatalf("yielded %v (registered %d), want [%d] and no forwarder", yielded, registered, orig)
		}
		if len(warns) != 1 || warns[0].ContextMap()["port"] != uint16(orig) || warns[0].ContextMap()["family"] != "IPv4" {
			t.Fatalf("WARNs %v, want one naming port %d and IPv4", warns, orig)
		}
		if !errors.Is(err, errIngressPortYielded) {
			t.Fatalf("a yield's error %v does not say the port was yielded", err)
		}
	}

	holder := &fakePortHolder{held: true}
	pm := &IngressProxyManager{logger: zap.NewNop(), tcChan: make(chan *models.TestCase, 4), samplingSem: make(chan struct{}, 1), portHolder: holder}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := newGoTCPIngressHook(pm).StartIngress(ctx, portOf(t, freeAddr(t)), portOf(t, freeAddr(t))); err == nil {
		t.Fatal("a forwarder started on a canceled context")
	}
	if len(holder.yielded) != 0 {
		t.Fatalf("a forwarder that failed for a canceled context yielded %v", holder.yielded)
	}
}

// A port keploy yields by design is logged once, as the yield's WARN: the
// manager logs no "Ingress hook failed to start" ERROR for it, which it still
// does, as on main, for a forwarder that cannot listen on a port nothing
// holds for keploy.
func TestAYieldedPortLogsOnlyItsWarn(t *testing.T) {
	other, err := net.Listen("tcp4", "0.0.0.0:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = other.Close() })
	orig := portOf(t, other.Addr().String())
	for _, held := range []bool{true, false} {
		core, logs := observer.New(zapcore.DebugLevel)
		holder := &fakePortHolder{held: held}
		pm := &IngressProxyManager{logger: zap.New(core), tcChan: make(chan *models.TestCase, 4), samplingSem: make(chan struct{}, 1),
			active: make(map[uint16]proxyStop), portHolder: holder}
		pm.ingressHook = newGoTCPIngressHook(pm)
		pm.StartIngressProxy(context.Background(), orig, portOf(t, freeAddr(t)))
		errs := logs.FilterLevelExact(zapcore.ErrorLevel).All()
		warns := logs.FilterLevelExact(zapcore.WarnLevel).All()
		if held && (len(errs) != 0 || len(warns) != 1) {
			t.Fatalf("a yielded port logged %d ERRORs and %d WARNs, want only the yield's WARN: %v", len(errs), len(warns), logs.All())
		}
		if !held && (len(errs) != 1 || len(warns) != 0) {
			t.Fatalf("without a hold, a taken port logged %d ERRORs and %d WARNs, want main's one ERROR: %v", len(errs), len(warns), logs.All())
		}
	}
}
