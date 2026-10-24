//go:build linux

package testnet

import (
	"context"
	"errors"
	"io/fs"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"

	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

// RefusedAddr is a loopback address, 127.0.0.1:P, that refuses every dial
// for the test's life: with network "tcp" a connect, with "udp" a datagram.
// Its port is held with a socket that never listens, so the kernel refuses a
// dial to it as it refuses one to a port nothing holds, and no other socket
// can take the port meanwhile at the addresses it is held at. A TCP port is
// held at every address, IPv4 and IPv6, by one dual-stack socket at [::] (at
// 0.0.0.0 where there is no IPv6): a test may dial it at this machine's own
// non-loopback address, and keploy's dials to a loopback address fall back
// to the other loopback family. A UDP port is held at 127.0.0.1.
func RefusedAddr(t *testing.T, network string) string {
	t.Helper()
	host := "127.0.0.1"
	if network == "tcp" {
		host = "::"
	}
	port, err := bindHold(t, network, host, 0, false)
	if network == "tcp" && addrAbsent(err) {
		port, err = bindHold(t, network, "0.0.0.0", 0, false)
	}
	require.NoError(t, err)
	return net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
}

// ListenIPv6Only opens a TCP listener at [::1]:P that no IPv4 address
// reaches. The listener is closed when the test ends. P is held at every
// IPv4 address, 0.0.0.0, for the test's life, by a socket that never
// listens. So a dial to 127.0.0.1:P is refused. No other socket can take P
// at an IPv4 address meanwhile. The hold is taken first, at port 0: the
// kernel then picks a port no IPv4 socket is at, one in TIME_WAIT included.
// The listener then opens at that port, IPv6-only. No IPv4 socket conflicts
// with it. Taken the other way round, an IPv6 bind picks the port. An IPv4
// TIME_WAIT can be at that port. The hold, a socket without SO_REUSEADDR,
// then cannot bind beside it. The test is skipped where there is no IPv6
// loopback. ListenIPv6Only is built on Linux only, where a hold can be made.
func ListenIPv6Only(t *testing.T) net.Listener {
	t.Helper()
	return listenIPv6Only(t, nil)
}

// listenIPv6Only is ListenIPv6Only that calls between, when it is not nil,
// with the port once it is held and before the listener opens there.
func listenIPv6Only(t *testing.T, between func(port int)) net.Listener {
	t.Helper()
	for range 10 {
		port, err := bindHold(t, "tcp", "0.0.0.0", 0, false)
		require.NoError(t, err)
		if between != nil {
			between(port)
		}
		ln, err := net.Listen("tcp6", net.JoinHostPort("::1", strconv.Itoa(port)))
		if err == nil {
			t.Cleanup(func() { _ = ln.Close() })
			return ln
		}
		if addrAbsent(err) {
			t.Skipf("no IPv6 loopback here: %v", err)
		}
		// Another IPv6-only socket has the port at [::1] or [::]. The
		// hold stays to the test's end. The next round holds another port.
		require.ErrorIs(t, err, syscall.EADDRINUSE)
	}
	t.Fatal("found no port free at [::1] among ten held at every IPv4 address")
	return nil
}

// SharedPort holds a free TCP port for the test's life at every address,
// IPv4 and IPv6, by one dual-stack socket at [::] that never listens (at
// 0.0.0.0 where there is no IPv6), and returns it. ListenShared and
// OpenShared open a listener at the port beside the hold: so a test can
// close its listener, find no listener at the port and a dial to it refused,
// and listen there again, while no other listener of this test binary or of
// a package tested beside it takes the port meanwhile. A listener at one
// loopback family leaves the port refused at the other. The hold and the
// test's listeners set SO_REUSEPORT; net.Listen does not, so a socket it
// makes cannot bind the port, and a bind to port 0 never lands on it. A
// socket of this user that sets SO_REUSEPORT and binds the port by number
// can, as keploy's DNS server does at the port it is given.
func SharedPort(t *testing.T) int {
	t.Helper()
	port, err := bindHold(t, "tcp", "::", 0, true)
	if addrAbsent(err) {
		port, err = bindHold(t, "tcp", "0.0.0.0", 0, true)
	}
	require.NoError(t, err)
	return port
}

// ListenShared opens a TCP listener at addr, a port SharedPort holds, beside
// the hold; it is closed when the test ends if the test has not closed it.
func ListenShared(t *testing.T, network, addr string) net.Listener {
	t.Helper()
	ln, err := OpenShared(network, addr)
	require.NoError(t, err)
	t.Cleanup(func() { _ = ln.Close() })
	return ln
}

// OpenShared is ListenShared that returns its error instead of failing the
// test, for a goroutine the test starts or a test that skips on it; the
// caller closes the listener.
func OpenShared(network, addr string) (net.Listener, error) {
	lc := net.ListenConfig{Control: func(_, _ string, c syscall.RawConn) error {
		var serr error
		if err := c.Control(func(fd uintptr) {
			serr = unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_REUSEPORT, 1)
		}); err != nil {
			return err
		}
		return serr
	}}
	return lc.Listen(context.Background(), network, addr)
}

// bindHold binds a socket that never listens at host:port for the test's
// life and returns its port; on an error nothing is held. The socket is made
// with syscall.Socket, so without the SO_REUSEADDR that net.Listen sets, and
// no other socket can bind the port at an address it overlaps. shared sets
// SO_REUSEPORT, so a socket of this user that sets it too, as OpenShared's
// listeners do, can bind beside it. An IPv6 socket is dual-stack: it holds
// the port at IPv4 addresses too. A UDP socket is connected to itself:
// bound alone it would take every datagram sent to its port, while
// connected it takes only its own, and the kernel answers any other
// sender's as it answers one to a port nothing holds, port unreachable,
// which a connected sender reads as refused.
func bindHold(t *testing.T, network, host string, port int, shared bool) (int, error) {
	t.Helper()
	sotype := syscall.SOCK_STREAM
	switch network {
	case "tcp":
	case "udp":
		sotype = syscall.SOCK_DGRAM
	default:
		t.Fatalf("bindHold: network %q is neither tcp nor udp", network)
	}
	ip := net.ParseIP(host)
	require.NotNil(t, ip, "bindHold: %q is not an IP address", host)
	family, sa := syscall.AF_INET, syscall.Sockaddr(nil)
	if ip4 := ip.To4(); ip4 != nil {
		sa = &syscall.SockaddrInet4{Port: port, Addr: [4]byte(ip4)}
	} else {
		family, sa = syscall.AF_INET6, &syscall.SockaddrInet6{Port: port, Addr: [16]byte(ip.To16())}
	}
	fd, err := syscall.Socket(family, sotype|syscall.SOCK_CLOEXEC, 0)
	if err != nil {
		return 0, err
	}
	fail := func(err error) (int, error) {
		_ = syscall.Close(fd)
		return 0, err
	}
	if family == syscall.AF_INET6 {
		if err := syscall.SetsockoptInt(fd, syscall.IPPROTO_IPV6, syscall.IPV6_V6ONLY, 0); err != nil {
			return fail(err)
		}
	}
	if shared {
		if err := unix.SetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_REUSEPORT, 1); err != nil {
			return fail(err)
		}
	}
	if err := syscall.Bind(fd, sa); err != nil {
		return fail(err)
	}
	name, err := syscall.Getsockname(fd)
	if err != nil {
		return fail(err)
	}
	if sotype == syscall.SOCK_DGRAM {
		if err := syscall.Connect(fd, name); err != nil {
			return fail(err)
		}
	}
	t.Cleanup(func() { _ = syscall.Close(fd) })
	switch n := name.(type) {
	case *syscall.SockaddrInet4:
		port = n.Port
	case *syscall.SockaddrInet6:
		port = n.Port
	}
	return port, nil
}

// addrAbsent reports a socket or bind refused because this machine has no
// such address or address family, so no socket can take a port there either.
func addrAbsent(err error) bool {
	return errors.Is(err, syscall.EADDRNOTAVAIL) || errors.Is(err, syscall.EAFNOSUPPORT)
}

// BoundHere counts the TCP sockets of this process at port, at any address:
// those /proc/net/tcp and tcp6 list at the port whose inode is one of this
// process's descriptors. The tables list listening sockets and those with a
// peer only, so it counts none for a socket that is bound but neither
// listens nor connects, as this package's TCP holds are. The tables give no
// inode, so it counts none for a socket in TIME_WAIT or one the kernel keeps
// with no owner (an orphan still closing, a connection not yet accepted). It
// reads the descriptors first and the tables then, so a socket closed
// between the two reads is not counted, and one opened between them is not
// either. known is true: on Linux it can tell.
func BoundHere(t *testing.T, port int) (n int, known bool) {
	t.Helper()
	mine := map[string]bool{}
	fds, err := os.ReadDir("/proc/self/fd")
	require.NoError(t, err)
	for _, fd := range fds {
		if l, err := os.Readlink("/proc/self/fd/" + fd.Name()); err == nil && strings.HasPrefix(l, "socket:[") {
			mine[l] = true
		}
	}
	for _, table := range []string{"/proc/net/tcp", "/proc/net/tcp6"} {
		b, err := os.ReadFile(table)
		if errors.Is(err, fs.ErrNotExist) {
			continue // no IPv6
		}
		require.NoError(t, err)
		for _, line := range strings.Split(string(b), "\n")[1:] {
			f := strings.Fields(line)
			if len(f) < 10 || !mine["socket:["+f[9]+"]"] {
				continue
			}
			_, hex, ok := strings.Cut(f[1], ":")
			if p, err := strconv.ParseUint(hex, 16, 16); ok && err == nil && int(p) == port {
				n++
				delete(mine, "socket:["+f[9]+"]")
			}
		}
	}
	return n, true
}

// GiveAway hands ln's socket to another process, which keeps it for the
// test's life, and closes ln: the port then has a socket at it that is not
// this process's, as when a socket of a package tested beside this one takes
// the port. The other process is cat, in a process group of its own,
// reading a pipe from this one: the test kills the group when it ends, and
// cat ends too when this process ends and the pipe closes with it.
func GiveAway(t *testing.T, ln *net.TCPListener) {
	t.Helper()
	f, err := ln.File()
	require.NoError(t, err)
	cmd := exec.Command("cat")
	cmd.ExtraFiles = []*os.File{f}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	in, err := cmd.StdinPipe()
	require.NoError(t, err)
	require.NoError(t, cmd.Start())
	t.Cleanup(func() {
		_ = in.Close()
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		_ = cmd.Wait()
	})
	require.NoError(t, f.Close())
	require.NoError(t, ln.Close())
}
