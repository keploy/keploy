//go:build linux

package proxy

import (
	"context"
	"errors"
	"maps"
	"net"
	"net/netip"
	"strconv"
	"syscall"
	"testing"
	"time"

	"go.keploy.io/server/v3/internal/testnet"
	"go.keploy.io/server/v3/pkg/agent/proxy/util"
	"golang.org/x/sys/unix"
)

// freebind sets IP_FREEBIND on a socket before it binds, so it may be at an
// address no device of this namespace has: what a socket bound to a
// machine's own address looks like, unprivileged.
func freebind(fd int) error {
	return unix.SetsockoptInt(fd, unix.SOL_IP, unix.IP_FREEBIND, 1)
}

// listenFree is a listener at host ("127.0.0.1", "fe80::5%lo") opened with
// IP_FREEBIND (freebind) at a port testnet.SharedPort holds, beside the hold:
// so no other socket of this test binary or of a package tested beside it
// (one bound to port 0, or one without SO_REUSEPORT) listens on that port at
// another address while the rows ask. A bind to port 0 hands out a port free
// at the address bound alone, so one there that a row's destination reaches
// would be taken for it too, as listenInodes must. It is closed when the test
// ends; err is the listen's.
func listenFree(t *testing.T, network, host string) (net.Listener, error) {
	t.Helper()
	port := testnet.SharedPort(t)
	lc := testnet.SharedListenConfig()
	reusePort := lc.Control
	lc.Control = func(network, address string, c syscall.RawConn) error {
		if err := reusePort(network, address, c); err != nil {
			return err
		}
		var serr error
		if err := c.Control(func(fd uintptr) { serr = freebind(int(fd)) }); err != nil {
			return err
		}
		return serr
	}
	l, err := lc.Listen(context.Background(), network, net.JoinHostPort(host, strconv.Itoa(port)))
	if err != nil {
		return nil, err
	}
	t.Cleanup(func() { _ = l.Close() })
	return l, nil
}

// listenAsk is a connection's destination and whether listenInodes takes the
// one listener on its port for it.
type listenAsk struct {
	ip   string
	port uint32
	in   bool
}

// listenAsks opens the listeners the listenInodes rows ask about, each
// alone on its port (testnet.SharedPort), and the asks: a listener at any
// address is taken for every destination, one at an address for that
// address alone, and one at a loopback address for every loopback
// destination. An IPv6 one this machine cannot open is logged, and its asks
// left out. A connection to the loopback listener stays open, its accepted
// end on the listener's port and at its address but established, not
// listening (/proc/net/tcp's state 01, not 0A): the loopback listener's asks
// take the listener alone.
func listenAsks(t *testing.T) []listenAsk {
	t.Helper()
	var asks []listenAsk
	listen := func(network, host string, ips map[string]bool) net.Listener {
		l, err := listenFree(t, network, host)
		if err != nil {
			if network == "tcp6" {
				t.Logf("no IPv6 listener at %s here (%v): its asks are left out", host, err)
				return nil
			}
			t.Fatal(err)
		}
		for ip, in := range ips {
			asks = append(asks, listenAsk{ip, uint32(l.Addr().(*net.TCPAddr).Port), in})
		}
		return l
	}
	loop := listen("tcp4", "127.0.0.1", map[string]bool{"127.0.0.1": true, "127.0.0.2": true, "::1": true, "192.0.2.1": false})
	acceptOne(t, loop)
	listen("tcp4", "0.0.0.0", map[string]bool{"192.0.2.7": true, "127.0.0.1": true})
	listen("tcp4", "192.0.2.1", map[string]bool{"192.0.2.1": true, "127.0.0.1": false, "192.0.2.2": false})
	listen("tcp6", "::", map[string]bool{"127.0.0.1": true, "2001:db8::1": true})
	listen("tcp6", "2001:db8::5", map[string]bool{"2001:db8::5": true, "::1": false})
	// A link-local listener is bound to its device: sock_diag gives its
	// address with the device as its zone, /proc/net/tcp6 with none.
	listen("tcp6", "fe80::5%lo", map[string]bool{"fe80::5": true, "fe80::6": false})
	// An IPv6 socket at a v4-mapped address takes that IPv4 address's
	// connections: sock_diag and /proc/net/tcp6 give its address v4-mapped.
	if port, err := listenMapped(t, [4]byte{192, 0, 2, 9}); err != nil {
		t.Logf("no IPv6 listener at ::ffff:192.0.2.9 here (%v): its asks are left out", err)
	} else {
		asks = append(asks, listenAsk{"192.0.2.9", port, true}, listenAsk{"192.0.2.8", port, false})
	}
	return asks
}

// acceptOne connects to l and accepts the connection; both ends stay open
// until the test ends, when the dialing end closes first, so the TIME_WAIT
// is the dialing end's, on its own port (a later hold passes over a port a
// TIME_WAIT is at either way).
func acceptOne(t *testing.T, l net.Listener) {
	t.Helper()
	c, err := net.Dial(l.Addr().Network(), l.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	if err := l.(*net.TCPListener).SetDeadline(time.Now().Add(time.Minute)); err != nil {
		_ = c.Close()
		t.Fatal(err)
	}
	s, err := l.Accept()
	if err != nil {
		_ = c.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = c.Close()
		_ = s.Close()
	})
}

// listenMapped is the port of an IPv6 socket listening at v4's v4-mapped
// address (one net.Listen does not open: "tcp6" is IPv6 alone), at a port
// testnet.SharedPort holds, beside the hold, as listenFree's; it is closed
// when the test ends.
func listenMapped(t *testing.T, v4 [4]byte) (uint32, error) {
	t.Helper()
	port := testnet.SharedPort(t)
	fd, err := unix.Socket(unix.AF_INET6, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return 0, err
	}
	t.Cleanup(func() { _ = unix.Close(fd) })
	if err := unix.SetsockoptInt(fd, unix.IPPROTO_IPV6, unix.IPV6_V6ONLY, 0); err != nil {
		return 0, err
	}
	if err := unix.SetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_REUSEPORT, 1); err != nil {
		return 0, err
	}
	if err := freebind(fd); err != nil {
		return 0, err
	}
	if err := unix.Bind(fd, &unix.SockaddrInet6{Port: int(port), Addr: netip.AddrFrom4(v4).As16()}); err != nil {
		return 0, err
	}
	if err := unix.Listen(fd, 1); err != nil {
		return 0, err
	}
	return uint32(port), nil
}

// askIPs is a's destination as a connection can carry it: net.ParseIP's
// 16-byte form, and an IPv4 address's 4-byte one.
func askIPs(a listenAsk) []net.IP {
	ips := []net.IP{net.ParseIP(a.ip)}
	if v4 := ips[0].To4(); v4 != nil {
		ips = append(ips, v4)
	}
	return ips
}

// listenInodes reads the listeners from sock_diag, never /proc/net/tcp, and
// takes the sockets the /proc read takes for each destination. A port past
// 65535 has none, not the listener on the port it would wrap to.
func TestListenInodesReadsSockDiagAsProcWould(t *testing.T) {
	asks := listenAsks(t)
	before := procReads.Load()
	procCalls := int64(0)
	for _, a := range asks {
		for _, ip := range askIPs(a) {
			got := listenInodes(ip, a.port)
			if len(got) > 1 || (len(got) == 1) != a.in {
				t.Errorf("listenInodes(%s, %d) = %v, want the listener there: %v", ip, a.port, got, a.in)
			}
			procCalls++
			if proc := listenInodesProc(ip, a.port); !maps.Equal(got, proc) {
				t.Errorf("listenInodes(%s, %d) = %v from sock_diag, %v from /proc/net/tcp", ip, a.port, got, proc)
			}
		}
	}
	if reads := procReads.Load() - before; reads != procCalls {
		t.Errorf("%d reads of /proc/net/tcp, want %d: the rows' own listenInodesProc calls alone", reads, procCalls)
	}
	wrapped := asks[0].port + (1 << 16)
	if got := listenInodes(net.ParseIP(asks[0].ip), wrapped); len(got) != 0 {
		t.Errorf("listenInodes(%s, %d) = %v, want none", asks[0].ip, wrapped, got)
	}
}

// When sock_diag cannot be read, listenInodes reads /proc/net/tcp, once per
// call, and takes the same sockets.
func TestListenInodesReadsProcWhenSockDiagFails(t *testing.T) {
	asks := listenAsks(t)
	saved := listenSockets
	t.Cleanup(func() { listenSockets = saved })
	listenSockets = func(uint16) ([]util.ListenSocket, error) { return nil, errors.New("no inet_diag") }
	for _, a := range asks {
		for _, ip := range askIPs(a) {
			before := procReads.Load()
			got := listenInodes(ip, a.port)
			if reads := procReads.Load() - before; reads != 1 {
				t.Fatalf("listenInodes(%s, %d) with sock_diag failing read /proc/net/tcp %d times, want once", ip, a.port, reads)
			}
			if len(got) > 1 || (len(got) == 1) != a.in {
				t.Errorf("listenInodes(%s, %d) from /proc/net/tcp = %v, want the listener there: %v", ip, a.port, got, a.in)
			}
		}
	}
}

// No socket without SO_REUSEPORT can listen on a port the rows' listeners
// are on, at any address of either family, while the rows ask
// (testnet.SharedPort): one there that a destination reaches would be taken
// for it too. A bind to port 0 passes over every port an explicit bind is
// refused, so none lands there either.
func TestListenAsksListenersAreAloneOnTheirPorts(t *testing.T) {
	ports := map[uint32]bool{}
	for _, a := range listenAsks(t) {
		ports[a.port] = true
	}
	lc := net.ListenConfig{Control: func(_, _ string, c syscall.RawConn) error {
		var serr error
		if err := c.Control(func(fd uintptr) { serr = freebind(int(fd)) }); err != nil {
			return err
		}
		return serr
	}}
	for port := range ports {
		for _, at := range []struct{ network, host string }{
			{"tcp4", "127.0.0.1"}, {"tcp4", "127.0.0.2"}, {"tcp4", "0.0.0.0"}, {"tcp4", "192.0.2.77"},
			{"tcp6", "::1"}, {"tcp6", "::"}, {"tcp6", "2001:db8::77"},
			{"tcp", ""}, // both families at once, as net.Listen("tcp", ":P") is
		} {
			addr := net.JoinHostPort(at.host, strconv.FormatUint(uint64(port), 10))
			l, err := lc.Listen(context.Background(), at.network, addr)
			switch {
			case err == nil:
				_ = l.Close()
				t.Errorf("a listener opened at %s %s, a port of the rows' listeners: they are not alone on their ports", at.network, addr)
			case errors.Is(err, syscall.EADDRINUSE):
			case at.network != "tcp4" && (errors.Is(err, syscall.EAFNOSUPPORT) || errors.Is(err, syscall.EADDRNOTAVAIL)):
				t.Logf("no IPv6 listener at %s here (%v)", addr, err)
			default:
				t.Errorf("a listener at %s %s: %v, want %v", at.network, addr, err, syscall.EADDRINUSE)
			}
		}
	}
}
