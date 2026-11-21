//go:build linux

package proxy

import (
	"context"
	"errors"
	"maps"
	"net"
	"net/netip"
	"syscall"
	"testing"

	"go.keploy.io/server/v3/pkg/agent/proxy/util"
	"golang.org/x/sys/unix"
)

// listenFree is the port of a listener at addr ("host:0" or "[host%zone]:0")
// opened with IP_FREEBIND, so it may be at an address no device of this
// namespace has: what a socket bound to a machine's own address looks like,
// unprivileged. It is closed when the test ends; err is the listen's.
func listenFree(t *testing.T, network, addr string) (uint32, error) {
	t.Helper()
	lc := net.ListenConfig{Control: func(_, _ string, c syscall.RawConn) error {
		var serr error
		if err := c.Control(func(fd uintptr) {
			serr = unix.SetsockoptInt(int(fd), unix.SOL_IP, unix.IP_FREEBIND, 1)
		}); err != nil {
			return err
		}
		return serr
	}}
	l, err := lc.Listen(context.Background(), network, addr)
	if err != nil {
		return 0, err
	}
	t.Cleanup(func() { _ = l.Close() })
	return uint32(l.Addr().(*net.TCPAddr).Port), nil
}

// listenAsk is a connection's destination and whether listenInodes takes the
// one listener on its port for it.
type listenAsk struct {
	ip   string
	port uint32
	in   bool
}

// listenAsks opens the listeners the listenInodes rows ask about, and the
// asks: a listener at any address is taken for every destination, one at an
// address for that address alone, and one at a loopback address for every
// loopback destination. An IPv6 one this machine cannot open is logged, and
// its asks left out.
func listenAsks(t *testing.T) []listenAsk {
	t.Helper()
	var asks []listenAsk
	listen := func(network, addr string, ips map[string]bool) {
		port, err := listenFree(t, network, addr)
		if err != nil {
			if network == "tcp6" {
				t.Logf("no IPv6 listener at %s here (%v): its asks are left out", addr, err)
				return
			}
			t.Fatal(err)
		}
		for ip, in := range ips {
			asks = append(asks, listenAsk{ip, port, in})
		}
	}
	listen("tcp4", "127.0.0.1:0", map[string]bool{"127.0.0.1": true, "127.0.0.2": true, "::1": true, "192.0.2.1": false})
	listen("tcp4", "0.0.0.0:0", map[string]bool{"192.0.2.7": true, "127.0.0.1": true})
	listen("tcp4", "192.0.2.1:0", map[string]bool{"192.0.2.1": true, "127.0.0.1": false, "192.0.2.2": false})
	listen("tcp6", "[::]:0", map[string]bool{"127.0.0.1": true, "2001:db8::1": true})
	listen("tcp6", "[2001:db8::5]:0", map[string]bool{"2001:db8::5": true, "::1": false})
	// A link-local listener is bound to its device: sock_diag gives its
	// address with the device as its zone, /proc/net/tcp6 with none.
	listen("tcp6", "[fe80::5%lo]:0", map[string]bool{"fe80::5": true, "fe80::6": false})
	// An IPv6 socket at a v4-mapped address takes that IPv4 address's
	// connections: sock_diag and /proc/net/tcp6 give its address v4-mapped.
	if port, err := listenMapped(t, [4]byte{192, 0, 2, 9}); err != nil {
		t.Logf("no IPv6 listener at ::ffff:192.0.2.9 here (%v): its asks are left out", err)
	} else {
		asks = append(asks, listenAsk{"192.0.2.9", port, true}, listenAsk{"192.0.2.8", port, false})
	}
	return asks
}

// listenMapped is the port of an IPv6 socket listening at v4's v4-mapped
// address (one net.Listen does not open: "tcp6" is IPv6 alone), closed when
// the test ends.
func listenMapped(t *testing.T, v4 [4]byte) (uint32, error) {
	t.Helper()
	fd, err := unix.Socket(unix.AF_INET6, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return 0, err
	}
	t.Cleanup(func() { _ = unix.Close(fd) })
	if err := unix.SetsockoptInt(fd, unix.IPPROTO_IPV6, unix.IPV6_V6ONLY, 0); err != nil {
		return 0, err
	}
	if err := unix.SetsockoptInt(fd, unix.SOL_IP, unix.IP_FREEBIND, 1); err != nil {
		return 0, err
	}
	if err := unix.Bind(fd, &unix.SockaddrInet6{Addr: netip.AddrFrom4(v4).As16()}); err != nil {
		return 0, err
	}
	if err := unix.Listen(fd, 1); err != nil {
		return 0, err
	}
	sa, err := unix.Getsockname(fd)
	if err != nil {
		return 0, err
	}
	return uint32(sa.(*unix.SockaddrInet6).Port), nil
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
