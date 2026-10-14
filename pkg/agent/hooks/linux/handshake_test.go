//go:build linux

package linux

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"strconv"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent"
	"golang.org/x/sys/unix"
)

func addrPort(t *testing.T, a net.Addr) netip.AddrPort {
	t.Helper()
	ap, err := netip.ParseAddrPort(a.String())
	if err != nil {
		t.Fatal(err)
	}
	return ap
}

// TestSocketCookieFindsTheApplicationsSocket: the held SYN names its socket
// only by its two ends, and the cookie found from them must be that socket's
// own (SO_COOKIE), for a plain IPv4 socket and for an IPv6 one connected over
// v4-mapped addresses, which is how the connect6 hook redirects IPv6
// destinations to the proxy.
func TestSocketCookieFindsTheApplicationsSocket(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			defer c.Close()
		}
	}()
	port := ln.Addr().(*net.TCPAddr).Port

	t.Run("ipv4", func(t *testing.T) {
		c, err := net.Dial("tcp4", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		want := cookieOf(t, c.(*net.TCPConn))
		got, err := socketCookie(addrPort(t, c.LocalAddr()), addrPort(t, c.RemoteAddr()))
		if err != nil {
			t.Fatalf("socketCookie(%s -> %s): %v", c.LocalAddr(), c.RemoteAddr(), err)
		}
		if got != want {
			t.Fatalf("cookie %#x, want the socket's own %#x", got, want)
		}
	})

	// An AF_INET6 socket connected to ::ffff:127.0.0.1 sends IPv4 packets, so
	// the held SYN names it by IPv4 ends.
	t.Run("v4-mapped ipv6", func(t *testing.T) {
		fd, err := unix.Socket(unix.AF_INET6, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
		if err != nil {
			t.Fatal(err)
		}
		defer unix.Close(fd)
		if err := unix.Connect(fd, &unix.SockaddrInet6{Port: port, Addr: netip.MustParseAddr("::ffff:127.0.0.1").As16()}); err != nil {
			t.Fatal(err)
		}
		sa, err := unix.Getsockname(fd)
		if err != nil {
			t.Fatal(err)
		}
		l := sa.(*unix.SockaddrInet6)
		want, err := unix.GetsockoptUint64(fd, unix.SOL_SOCKET, unix.SO_COOKIE)
		if err != nil {
			t.Fatal(err)
		}
		local := netip.AddrPortFrom(netip.AddrFrom16(l.Addr).Unmap(), uint16(l.Port))
		if !local.Addr().Is4() {
			t.Fatalf("local end %s is not v4-mapped", local)
		}
		got, err := socketCookie(local, netip.AddrPortFrom(netip.MustParseAddr("127.0.0.1"), uint16(port)))
		if err != nil {
			t.Fatalf("socketCookie for the v4-mapped socket: %v", err)
		}
		if got != want {
			t.Fatalf("cookie %#x, want the socket's own %#x", got, want)
		}
	})
}

// TestSocketCookieFindsAConnectingSocket: the lookup runs while the SYN is
// held, so the socket is still connecting (SYN_SENT). A listener whose accept
// queue is full drops further SYNs, which keeps a connect in SYN_SENT on any
// machine, without root or a route anywhere.
func TestSocketCookieFindsAConnectingSocket(t *testing.T) {
	lfd, err := unix.Socket(unix.AF_INET, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(lfd)
	if err := unix.Bind(lfd, &unix.SockaddrInet4{Addr: [4]byte{127, 0, 0, 1}}); err != nil {
		t.Fatal(err)
	}
	if err := unix.Listen(lfd, 0); err != nil { // never accepted from
		t.Fatal(err)
	}
	lsa, err := unix.Getsockname(lfd)
	if err != nil {
		t.Fatal(err)
	}
	lport := lsa.(*unix.SockaddrInet4).Port
	remote := netip.AddrPortFrom(netip.MustParseAddr("127.0.0.1"), uint16(lport))

	// Fill the accept queue; each of these completes its handshake.
	for i := 0; i < 4; i++ {
		c, err := net.DialTimeout("tcp", remote.String(), 200*time.Millisecond)
		if err != nil {
			break // the queue is full
		}
		defer c.Close()
	}

	fd, err := unix.Socket(unix.AF_INET, unix.SOCK_STREAM|unix.SOCK_NONBLOCK|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(fd)
	if err := unix.Connect(fd, &unix.SockaddrInet4{Port: lport, Addr: [4]byte{127, 0, 0, 1}}); err != unix.EINPROGRESS {
		t.Fatalf("connect to a full accept queue: %v, want it in progress", err)
	}
	sa, err := unix.Getsockname(fd)
	if err != nil {
		t.Fatal(err)
	}
	local := netip.AddrPortFrom(netip.AddrFrom4(sa.(*unix.SockaddrInet4).Addr), uint16(sa.(*unix.SockaddrInet4).Port))
	want, err := unix.GetsockoptUint64(fd, unix.SOL_SOCKET, unix.SO_COOKIE)
	if err != nil {
		t.Fatal(err)
	}
	if info, err := unix.GetsockoptTCPInfo(fd, unix.IPPROTO_TCP, unix.TCP_INFO); err != nil || info.State != tcpSynSent {
		t.Fatalf("socket state %v (%v), want SYN_SENT; nothing is tested", info, err)
	}
	got, err := socketCookie(local, remote)
	if err != nil {
		t.Fatalf("socketCookie(%s -> %s): %v", local, remote, err)
	}
	if got != want {
		t.Fatalf("cookie %#x, want %#x", got, want)
	}
}

// TestSocketCookieNamesNoOtherSocket: two ends that no socket has are not
// answered with some other socket's cookie.
func TestSocketCookieNamesNoOtherSocket(t *testing.T) {
	if _, err := socketCookie(netip.MustParseAddrPort("127.0.0.1:1"), netip.MustParseAddrPort("127.0.0.1:2")); err == nil {
		t.Fatal("found a socket for two ends nothing holds")
	}
}

// tcpSynSent is TCP_SYN_SENT (include/net/tcp_states.h), as TCP_INFO reports it.
const tcpSynSent = 2

func cookieOf(t *testing.T, c *net.TCPConn) uint64 {
	t.Helper()
	rc, err := c.SyscallConn()
	if err != nil {
		t.Fatal(err)
	}
	var cookie uint64
	var gerr error
	if err := rc.Control(func(fd uintptr) {
		cookie, gerr = unix.GetsockoptUint64(int(fd), unix.SOL_SOCKET, unix.SO_COOKIE)
	}); err != nil {
		t.Fatal(err)
	}
	if gerr != nil {
		t.Fatal(gerr)
	}
	return cookie
}

// TestCheckHandshakeLookupWorksHere: the self-check the proxy runs before
// holding handshakes passes wherever sock_diag answers.
func TestCheckHandshakeLookupWorksHere(t *testing.T) {
	if err := (&Hooks{}).CheckHandshakeLookup(context.Background()); err != nil {
		t.Fatalf("CheckHandshakeLookup: %v", err)
	}
}

// TestSocketCookieDoesNotNameAListener: the kernel's exact lookup falls back
// to a listener on the local port when no connection has the ends asked for.
// That is not the application's connecting socket: it is gone.
func TestSocketCookieDoesNotNameAListener(t *testing.T) {
	l, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	local := netip.MustParseAddrPort(l.Addr().String())
	_, err = socketCookie(local, netip.MustParseAddrPort("127.0.0.1:1"))
	if !errors.Is(err, agent.ErrConnectingSocketGone) {
		t.Fatalf("socketCookie for a listener's port: %v, want ErrConnectingSocketGone", err)
	}
}
