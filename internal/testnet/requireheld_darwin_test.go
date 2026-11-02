//go:build darwin

package testnet

import (
	"context"
	"net"
	"syscall"
	"testing"

	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

// requireHeld checks that a socket holds addr's port, so a bind without
// SO_REUSEADDR fails because the address is in use, and that no bind to port
// 0 can be given it: it lies outside each range those take a port from. A
// listener that sets SO_REUSEADDR, as Go's do, can still bind it on macOS
// when it names the port.
func requireHeld(t *testing.T, addr string) {
	t.Helper()
	lc := net.ListenConfig{Control: func(_, _ string, rc syscall.RawConn) error {
		var serr error
		if err := rc.Control(func(fd uintptr) {
			serr = syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_REUSEADDR, 0)
		}); err != nil {
			return err
		}
		return serr
	}}
	ln, err := lc.Listen(context.Background(), "tcp", addr)
	if err == nil {
		_ = ln.Close()
		t.Fatalf("listened on %s without SO_REUSEADDR: no socket holds the port", addr)
	}
	require.ErrorIs(t, err, errAddrInUse, "listen on %s without SO_REUSEADDR: want the address in use", addr)
	tcp, err := net.ResolveTCPAddr("tcp", addr)
	require.NoError(t, err)
	for _, r := range [][2]string{{"lowlast", "lowfirst"}, {"first", "last"}, {"hifirst", "hilast"}} {
		a, b := portrange(t, r[0]), portrange(t, r[1])
		if tcp.Port >= min(a, b) && tcp.Port <= max(a, b) {
			t.Fatalf("port %d lies in net.inet.ip.portrange.%s-%s (%d-%d), which a bind to port 0 takes a port from, so another test's server can be given it", tcp.Port, r[0], r[1], a, b)
		}
	}
}

func portrange(t *testing.T, name string) int {
	t.Helper()
	n, err := unix.SysctlUint32("net.inet.ip.portrange." + name)
	require.NoError(t, err, "read net.inet.ip.portrange.%s: cannot tell whether a bind to port 0 can be given the held port", name)
	return int(n)
}
