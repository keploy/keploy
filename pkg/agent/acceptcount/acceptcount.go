// Package acceptcount counts the connections a listener accepts, by the local
// address each arrived at, and lets a caller wait for a count to be reached.
//
// The proxy counts its accepts here. The agent compares the count at the
// addresses the connect hooks redirect to with the kernel's count of the
// connections completed to the proxy's listener there (the hooks'
// proxy_conns_established): the difference is what still waits in the accept
// queue, which a test's begin and end wait for (models.ProxyConnCounts).
package acceptcount

import (
	"context"
	"net"
	"net/netip"
	"sync"
)

// Counter counts accepted connections by local address. Its counts only ever
// grow. The zero value is ready to use.
type Counter struct {
	mu      sync.Mutex
	byLocal map[netip.AddrPort]uint64
	// changed is closed, and replaced, at every Add: a waiter wakes on it.
	changed chan struct{}
}

// key is the local address a connection arrived at, an IPv4 address as itself
// whether or not it reached a dual-stack listener as ::ffff:a.b.c.d.
func key(a netip.AddrPort) netip.AddrPort {
	return netip.AddrPortFrom(a.Addr().Unmap(), a.Port())
}

// Add counts one accepted connection that arrived at local, a connection's
// LocalAddr. An address that is not TCP or IP is counted nowhere.
func (c *Counter) Add(local net.Addr) {
	ta, ok := local.(*net.TCPAddr)
	if !ok {
		return
	}
	ap := ta.AddrPort()
	if !ap.IsValid() {
		return
	}
	c.mu.Lock()
	if c.byLocal == nil {
		c.byLocal = make(map[netip.AddrPort]uint64)
	}
	c.byLocal[key(ap)]++
	if c.changed != nil {
		close(c.changed)
		c.changed = nil
	}
	c.mu.Unlock()
}

// To is how many accepted connections arrived at any of targets. A target is
// matched as Add keys a connection: an IPv4 address as itself.
func (c *Counter) To(targets []netip.AddrPort) uint64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.toLocked(targets)
}

func (c *Counter) toLocked(targets []netip.AddrPort) uint64 {
	var n uint64
	seen := make(map[netip.AddrPort]bool, len(targets))
	for _, t := range targets {
		k := key(t)
		if seen[k] {
			continue
		}
		seen[k] = true
		n += c.byLocal[k]
	}
	return n
}

// WaitTo returns once To(targets) >= n, or with ctx's error when ctx ends
// first. It has no deadline of its own: every Add wakes it to look again.
func (c *Counter) WaitTo(ctx context.Context, targets []netip.AddrPort, n uint64) error {
	for {
		c.mu.Lock()
		if c.toLocked(targets) >= n {
			c.mu.Unlock()
			return nil
		}
		if c.changed == nil {
			c.changed = make(chan struct{})
		}
		ch := c.changed
		c.mu.Unlock()
		select {
		case <-ch:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}
