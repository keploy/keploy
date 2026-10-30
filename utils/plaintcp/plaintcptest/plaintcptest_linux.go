//go:build linux

package plaintcptest

import (
	"context"
	"fmt"
	"net"
	"sync"
	"testing"

	"golang.org/x/sys/unix"
)

var (
	defaultsOnce  sync.Once
	defaultsMPTCP bool
	defaultsWhy   string
)

func requirePlainTCP(t testing.TB, ln net.Listener) {
	t.Helper()
	proto, err := protocol(tcpListener(t, ln))
	if err != nil {
		t.Fatal(err)
	}
	if proto == unix.IPPROTO_TCP {
		// Plain TCP. Say whether that told anything here.
		defaultsOnce.Do(probeDefaults)
		if !defaultsMPTCP {
			t.Skipf("the listener is plain TCP, but so is one opened with Go's defaults here (%s): this check cannot tell them apart", defaultsWhy)
		}
		return
	}
	if proto == unix.IPPROTO_MPTCP {
		t.Fatalf("the listener at %s is MPTCP (SO_PROTOCOL %d), want plain TCP (%d)", ln.Addr(), proto, unix.IPPROTO_TCP)
	}
	t.Fatalf("the listener at %s has SO_PROTOCOL %d, want plain TCP (%d)", ln.Addr(), proto, unix.IPPROTO_TCP)
}

// protocol is the SO_PROTOCOL of tl's socket: unix.IPPROTO_TCP for plain TCP,
// unix.IPPROTO_MPTCP for MPTCP.
func protocol(tl *net.TCPListener) (int, error) {
	rc, err := tl.SyscallConn()
	if err != nil {
		return 0, err
	}
	var proto int
	var gerr error
	if err := rc.Control(func(fd uintptr) {
		proto, gerr = unix.GetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_PROTOCOL)
	}); err != nil {
		return 0, err
	}
	if gerr != nil {
		return 0, fmt.Errorf("getsockopt SO_PROTOCOL: %w", gerr)
	}
	return proto, nil
}

// probeDefaults learns whether a listener opened with Go's defaults (a zero
// net.ListenConfig, as net.Listen uses) is MPTCP here.
func probeDefaults() {
	var lc net.ListenConfig
	ln, err := lc.Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		defaultsWhy = fmt.Sprintf("a probe listener failed: %v", err)
		return
	}
	defer ln.Close()
	tl, ok := ln.(*net.TCPListener)
	if !ok {
		defaultsWhy = fmt.Sprintf("a probe listener is a %T", ln)
		return
	}
	proto, err := protocol(tl)
	switch {
	case err != nil:
		defaultsWhy = fmt.Sprintf("a probe listener's protocol is unknown: %v", err)
	case proto == unix.IPPROTO_MPTCP:
		defaultsMPTCP = true
	default:
		defaultsWhy = "no MPTCP in this kernel, net.mptcp.enabled=0, or GODEBUG multipathtcp off"
	}
}
