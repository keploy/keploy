//go:build linux

package proxy

import (
	"net"
	"testing"

	"golang.org/x/sys/unix"
)

// The TCP DNS server's socket, bound by the proxy and not by miekg/dns, keeps
// the SO_REUSEPORT that miekg/dns's ReusePort set on it.
func TestTheTCPDNSServerKeepsSOReusePort(t *testing.T) {
	s, _, _ := serveDNSOnFreePort(t, "tcp", nil)
	if !s.bound {
		t.Fatalf("the TCP DNS server did not bind: %v", s.err)
	}
	rc, err := s.srv.Listener.(*net.TCPListener).SyscallConn()
	if err != nil {
		t.Fatal(err)
	}
	var on int
	var gerr error
	if err := rc.Control(func(fd uintptr) {
		on, gerr = unix.GetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_REUSEPORT)
	}); err != nil {
		t.Fatal(err)
	}
	if gerr != nil {
		t.Fatal(gerr)
	}
	if on != 1 {
		t.Fatalf("the TCP DNS server's SO_REUSEPORT is %d, want 1", on)
	}
}
