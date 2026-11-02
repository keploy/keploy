//go:build linux

package util

import (
	"net"
	"net/netip"
	"slices"
	"strconv"
	"testing"

	"golang.org/x/sys/unix"
)

// TCPListeners gives the addresses of the sockets that listen on the port,
// IPv4 and IPv6, and nothing else: not a connection on the port (the
// accepted one has it as its local port), not a socket bound to it that
// never listened, and nothing once the listener has closed.
func TestTCPListenersGivesTheListenersOnThePortAlone(t *testing.T) {
	ln, err := net.Listen("tcp4", "127.0.0.2:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	port := uint16(ln.Addr().(*net.TCPAddr).Port)
	want := []netip.Addr{netip.MustParseAddr("127.0.0.2")}
	if v6, err := net.Listen("tcp6", net.JoinHostPort("::1", strconv.Itoa(int(port)))); err == nil {
		defer func() { _ = v6.Close() }()
		want = append(want, netip.IPv6Loopback())
	} else {
		t.Logf("no [::1] listener here (%v): IPv4 alone", err)
	}
	c, err := net.Dial("tcp4", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	s, err := ln.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()

	got, err := TCPListeners(port)
	if err != nil {
		t.Fatal(err)
	}
	slices.SortFunc(got, netip.Addr.Compare)
	if !slices.Equal(got, want) {
		t.Fatalf("TCPListeners(%d) = %v, want %v", port, got, want)
	}

	_ = ln.Close()
	got, err = TCPListeners(port)
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range got {
		if a.Is4() {
			t.Fatalf("TCPListeners(%d) = %v after the IPv4 listener closed", port, got)
		}
	}

	fd, err := unix.Socket(unix.AF_INET, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = unix.Close(fd) }()
	if err := unix.Bind(fd, &unix.SockaddrInet4{Addr: [4]byte{127, 0, 0, 1}}); err != nil {
		t.Fatal(err)
	}
	sa, err := unix.Getsockname(fd)
	if err != nil {
		t.Fatal(err)
	}
	boundPort := uint16(sa.(*unix.SockaddrInet4).Port)
	if got, err := TCPListeners(boundPort); err != nil || len(got) != 0 {
		t.Fatalf("TCPListeners of a port a socket is bound to but does not listen on = %v, %v", got, err)
	}
}
