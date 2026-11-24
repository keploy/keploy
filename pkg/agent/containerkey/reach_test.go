package containerkey

import (
	"net/netip"
	"testing"

	"go.uber.org/zap"
)

// unlistedIndex is an index whose native watch (Start) has its first list out
// for the whole test, that tells what may be a container's by rc, on a host
// whose own addresses are addrs.
func unlistedIndex(t *testing.T, rc reach, addrs ...string) *dockerIndex {
	t.Helper()
	f := newFakeDocker()
	asked, release := heldList(t, f)
	r, _ := newIndex(f, addrs...)
	r.reach = rc
	t.Cleanup(func() { release(); stopIndex(r) })
	r.Start(zap.NewNop())
	within(t, asked, "the daemon was never asked for its list")
	return r
}

// While a native watch's first list is out, a destination may be a
// container's as this host tells without the daemon (MayBeContainer): any
// port on one of its own addresses (loopback, unspecified, its interfaces',
// one it sends to from itself), which a container may publish, and an
// address it routes apart from its default route (a Docker network's
// bridge). An address it has no route to, or sends to through its default
// route, is none. A host whose routes cannot be read, or that has no default
// route, may have one anywhere.
func TestMayBeContainerWhileTheFirstListIsOut(t *testing.T) {
	noRoute := netip.MustParseAddr("10.9.9.9")
	for _, c := range []struct {
		name string
		dst  string
		edit func(*reach)
		want bool
	}{
		{"a loopback port", "127.0.0.1:6379", nil, true},
		{"the IPv6 loopback's", "[::1]:5432", nil, true},
		{"an unspecified address's port", "0.0.0.0:6379", nil, true},
		{"one of this host's own addresses", "192.168.1.20:5432", nil, true},
		{"the address this host's default route leaves from, sent to from itself", "192.168.1.30:6379", func(rc *reach) {
			own := netip.MustParseAddr("192.168.1.30")
			rc.source = func(a netip.Addr) (netip.Addr, bool) { return own, true }
		}, true},
		{"an address this host routes to the daemon's bridge", "172.17.0.2:80", nil, true},
		{"a private address this host sends to through its default route", "10.0.4.7:5432", nil, false},
		{"a public address", "93.184.216.34:443", nil, false},
		{"an address this host has no route to", "10.9.9.9:5432", func(rc *reach) {
			src := rc.source
			rc.source = func(a netip.Addr) (netip.Addr, bool) {
				if a == noRoute {
					return netip.Addr{}, false
				}
				return src(a)
			}
		}, false},
		{"a private address, on a host with no default route", "10.0.4.7:5432", func(rc *reach) {
			src := rc.source
			rc.source = func(a netip.Addr) (netip.Addr, bool) {
				if a == defaultProbe4 || a == defaultProbe6 {
					return netip.Addr{}, false
				}
				return src(a)
			}
		}, true},
		{"a private address, on a host whose routes cannot be read", "10.0.4.7:5432", func(rc *reach) { rc.source = nil }, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			rc := hostRoutes()
			if c.edit != nil {
				c.edit(&rc)
			}
			r := unlistedIndex(t, rc, "192.168.1.20")
			if got := r.MayBeContainer(netip.MustParseAddrPort(c.dst)); got != c.want {
				t.Errorf("MayBeContainer(%s) = %v, want %v", c.dst, got, c.want)
			}
		})
	}
}

// Once the first list is in, the index alone says what a destination is:
// MayBeContainer says any may be. So it does for a watch no native session
// started (the keploy CLI's pusher, for a docker-mode agent's daemon).
func TestMayBeContainerLeavesTheIndexToSayOnceTheListIsIn(t *testing.T) {
	rds := netip.MustParseAddrPort("10.0.4.7:5432")
	f := newFakeDocker()
	r, _ := newIndex(f)
	r.reach = hostRoutes()
	t.Cleanup(func() { stopIndex(r) })
	r.Start(zap.NewNop())
	within(t, current(r).synced, "the watch never listed")
	if !r.MayBeContainer(rds) {
		t.Error("once the list is in, MayBeContainer still says a container cannot be at the destination")
	}

	g := newFakeDocker()
	asked, release := heldList(t, g)
	cli, _ := newIndex(g)
	cli.reach = hostRoutes()
	t.Cleanup(func() { release(); stopIndex(cli) })
	go cli.Watch(t.Context(), nil)
	within(t, asked, "the daemon was never asked for its list")
	if !cli.MayBeContainer(rds) {
		t.Error("a watch no native session started says what a container cannot be at")
	}
}

// This host's route to its own loopback leaves from the loopback (sourceAddr):
// the probe sends nothing and works with no network at all.
func TestSourceAddrOfTheLoopbackIsTheLoopback(t *testing.T) {
	lo := netip.MustParseAddr("127.0.0.1")
	if src, ok := sourceAddr(lo); !ok || src != lo {
		t.Fatalf("sourceAddr(%s) = %s, %v; want the loopback itself", lo, src, ok)
	}
}
