package utils

import (
	"errors"
	"net"
	"strings"
	"testing"
)

// iface is an up interface that is not the loopback, with the addresses cidrs
// name, in order.
func iface(t *testing.T, cidrs ...string) containerIface {
	t.Helper()
	return ifaceFlags(t, net.FlagUp, cidrs...)
}

func ifaceFlags(t *testing.T, flags net.Flags, cidrs ...string) containerIface {
	t.Helper()
	c := containerIface{flags: flags}
	for _, s := range cidrs {
		ip, n, err := net.ParseCIDR(s)
		if err != nil {
			t.Fatalf("bad row address %q: %v", s, err)
		}
		c.addrs = append(c.addrs, &net.IPNet{IP: ip, Mask: n.Mask})
	}
	return c
}

// loopback is a Pod's lo: up, flagged loopback, both families' addresses.
func loopback(t *testing.T) containerIface {
	return ifaceFlags(t, net.FlagUp|net.FlagLoopback, "127.0.0.1/8", "::1/128")
}

// The address the agent of a container (a Pod) has the kernel send the
// applications' connections to: its IPv4 address wherever it has one, as
// before, whatever order its interfaces list the families in; on a
// single-stack IPv6 network, its IPv6 address that is not link-local.
func TestContainerIPPicksTheAddressTheApplicationsReach(t *testing.T) {
	for _, tc := range []struct {
		name   string
		ifaces func(t *testing.T) []containerIface
		want   string
	}{
		{"IPv4 only", func(t *testing.T) []containerIface {
			return []containerIface{loopback(t), iface(t, "10.244.0.5/24")}
		}, "10.244.0.5"},
		{"dual-stack, IPv4 listed first", func(t *testing.T) []containerIface {
			return []containerIface{loopback(t), iface(t, "10.244.0.5/24", "fd00:10:244::5/64", "fe80::a/64")}
		}, "10.244.0.5"},
		{"dual-stack, IPv6 listed first (an IPv6-primary Pod)", func(t *testing.T) []containerIface {
			return []containerIface{loopback(t), iface(t, "fd00:10:244::5/64", "fe80::a/64", "10.244.0.5/24")}
		}, "10.244.0.5"},
		{"dual-stack, the IPv4 address on a later interface", func(t *testing.T) []containerIface {
			return []containerIface{loopback(t), iface(t, "2001:db8::5/64"), iface(t, "10.244.0.5/24")}
		}, "10.244.0.5"},
		{"IPv6 only, unique local", func(t *testing.T) []containerIface {
			return []containerIface{loopback(t), iface(t, "fe80::a/64", "fd00:10:244::5/64")}
		}, "fd00:10:244::5"},
		{"IPv6 only, global", func(t *testing.T) []containerIface {
			return []containerIface{loopback(t), iface(t, "fe80::a/64", "2001:db8::5/64")}
		}, "2001:db8::5"},
		{"IPv6 only, the first interface's address", func(t *testing.T) []containerIface {
			return []containerIface{loopback(t), iface(t, "fe80::a/64"), iface(t, "fd00:10:244::5/64"), iface(t, "2001:db8::7/64")}
		}, "fd00:10:244::5"},
		{"IPv6 only, a down interface's IPv4 address is not one", func(t *testing.T) []containerIface {
			return []containerIface{loopback(t), ifaceFlags(t, 0, "10.244.0.5/24"), iface(t, "fd00:10:244::5/64")}
		}, "fd00:10:244::5"},
		{"IPv6 only, a down interface's IPv6 address is not one", func(t *testing.T) []containerIface {
			return []containerIface{loopback(t), ifaceFlags(t, 0, "2001:db8::9/64"), iface(t, "fd00:10:244::5/64")}
		}, "fd00:10:244::5"},
		{"a loopback address on another interface is not one", func(t *testing.T) []containerIface {
			return []containerIface{loopback(t), iface(t, "127.0.0.2/8", "10.244.0.5/24")}
		}, "10.244.0.5"},
		{"IPv6 only, the loopback's other addresses are not one", func(t *testing.T) []containerIface {
			return []containerIface{ifaceFlags(t, net.FlagUp|net.FlagLoopback, "127.0.0.1/8", "::1/128", "10.96.0.10/32", "fd00:10:96::a/128"), iface(t, "fd00:10:244::5/64")}
		}, "fd00:10:244::5"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := containerIP(tc.ifaces(t))
			if err != nil {
				t.Fatalf("no address picked: %v", err)
			}
			if got.String() != tc.want {
				t.Fatalf("picked %s, want %s", got, tc.want)
			}
		})
	}
}

// A container with no address but loopback's and link-local ones has none
// its applications' connections can be sent to: the agent cannot start, and
// says why, tagged so its exit status says the environment lacks it.
func TestContainerIPRefusesLoopbackAndLinkLocalOnly(t *testing.T) {
	for _, tc := range []struct {
		name   string
		ifaces func(t *testing.T) []containerIface
	}{
		{"loopback and link-local", func(t *testing.T) []containerIface {
			return []containerIface{loopback(t), iface(t, "fe80::a/64")}
		}},
		{"loopback only", func(t *testing.T) []containerIface {
			return []containerIface{loopback(t)}
		}},
		{"link-local only, on two interfaces", func(t *testing.T) []containerIface {
			return []containerIface{iface(t, "fe80::a/64"), iface(t, "fe80::b/64")}
		}},
		{"a routable address only on a down interface", func(t *testing.T) []containerIface {
			return []containerIface{loopback(t), ifaceFlags(t, 0, "10.244.0.5/24", "fd00:10:244::5/64"), iface(t, "fe80::a/64")}
		}},
		{"no interfaces", func(*testing.T) []containerIface { return nil }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := containerIP(tc.ifaces(t))
			if err == nil {
				t.Fatalf("picked %s, want an error", got)
			}
			if !errors.Is(err, ErrEnvironmentUnsupported) {
				t.Fatalf("the error is not tagged as the environment's: %v", err)
			}
			for _, says := range []string{"could not find a non-loopback IP for the container", "IPv4", "link-local"} {
				if !strings.Contains(err.Error(), says) {
					t.Fatalf("the error does not say %q: %v", says, err)
				}
			}
		})
	}
}
