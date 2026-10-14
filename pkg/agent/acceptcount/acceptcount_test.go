package acceptcount

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"testing"
)

func tcp(s string) *net.TCPAddr { return net.TCPAddrFromAddrPort(netip.MustParseAddrPort(s)) }

func TestCountsByTheAddressAConnectionArrivedAt(t *testing.T) {
	var c Counter
	v4, v6 := netip.MustParseAddrPort("127.0.0.1:16789"), netip.MustParseAddrPort("[::1]:16789")
	targets := []netip.AddrPort{v4, v6}
	if got := c.To(targets); got != 0 {
		t.Fatalf("a fresh counter: %d, want 0", got)
	}
	c.Add(tcp("127.0.0.1:16789"))
	// A dual-stack listener's IPv4 connection arrives as ::ffff:127.0.0.1.
	c.Add(&net.TCPAddr{IP: net.ParseIP("::ffff:127.0.0.1"), Port: 16789})
	c.Add(tcp("[::1]:16789"))
	// Not at a target: the kernel counts neither.
	c.Add(tcp("127.0.0.2:16789"))
	c.Add(tcp("127.0.0.1:16790"))
	c.Add(&net.UnixAddr{Name: "/tmp/x", Net: "unix"})
	if got := c.To(targets); got != 3 {
		t.Fatalf("To(targets) = %d, want 3: two IPv4 (one v4-mapped) and one IPv6", got)
	}
	// A target given as v4-mapped, or twice, is the same target.
	if got := c.To([]netip.AddrPort{netip.MustParseAddrPort("[::ffff:127.0.0.1]:16789"), v4}); got != 2 {
		t.Fatalf("To(v4-mapped, v4) = %d, want 2", got)
	}
	if got := c.To(nil); got != 0 {
		t.Fatalf("To(nil) = %d, want 0", got)
	}
}

func TestWaitToWakesOnTheAcceptThatReachesTheCount(t *testing.T) {
	var c Counter
	targets := []netip.AddrPort{netip.MustParseAddrPort("127.0.0.1:1")}
	if err := c.WaitTo(context.Background(), targets, 0); err != nil {
		t.Fatalf("waiting for 0: %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- c.WaitTo(context.Background(), targets, 2) }()
	// An accept elsewhere wakes the waiter, which looks again and waits on.
	c.Add(tcp("127.0.0.2:1"))
	c.Add(tcp("127.0.0.1:1"))
	select {
	case err := <-done:
		t.Fatalf("WaitTo returned %v at 1 of 2", err)
	default:
	}
	c.Add(tcp("127.0.0.1:1"))
	if err := <-done; err != nil {
		t.Fatalf("WaitTo: %v", err)
	}
}

func TestWaitToEndsWithItsContext(t *testing.T) {
	var c Counter
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- c.WaitTo(ctx, []netip.AddrPort{netip.MustParseAddrPort("127.0.0.1:1")}, 1) }()
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("WaitTo after cancel: %v, want context.Canceled", err)
	}
}
