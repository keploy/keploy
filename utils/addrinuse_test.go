package utils

import (
	"errors"
	"net"
	"testing"
)

// A bind of a port another socket holds is a taken port (IsAddrInUse), over
// TCP and over UDP, as the proxy and its DNS server bind theirs. On Windows
// that refusal is Winsock's WSAEADDRINUSE, which syscall.EADDRINUSE does not
// match: there a taken port was neither named nor retried. A bind that fails
// otherwise, of an address this host does not have, is not a taken port, nor
// is an error that is no bind's.
func TestABindOfAHeldPortIsAddrInUse(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	if again, err := net.Listen("tcp", ln.Addr().String()); err == nil {
		_ = again.Close()
		t.Fatalf("a second TCP listener bound the held %s", ln.Addr())
	} else if !IsAddrInUse(err) {
		t.Fatalf("a TCP bind of the held %s: %#v; want a taken port (IsAddrInUse)", ln.Addr(), err)
	}

	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen udp: %v", err)
	}
	t.Cleanup(func() { _ = pc.Close() })
	if again, err := net.ListenPacket("udp", pc.LocalAddr().String()); err == nil {
		_ = again.Close()
		t.Fatalf("a second UDP socket bound the held %s", pc.LocalAddr())
	} else if !IsAddrInUse(err) {
		t.Fatalf("a UDP bind of the held %s: %#v; want a taken port (IsAddrInUse)", pc.LocalAddr(), err)
	}

	others := []error{errors.New("connection refused"), nil}
	// 192.0.2.1 is TEST-NET-1, an address no host is given.
	if notHere, err := net.Listen("tcp", "192.0.2.1:0"); err == nil {
		_ = notHere.Close()
	} else {
		others = append(others, err)
	}
	for _, other := range others {
		if IsAddrInUse(other) {
			t.Fatalf("IsAddrInUse(%#v) = true; want false: no socket holds the port", other)
		}
	}
}
