//go:build linux

package proxy

import (
	"net/netip"

	"golang.org/x/sys/unix"
)

// probeIPv6Port binds [addr]:port with IPV6_V6ONLY, as Go's listeners do with
// SO_REUSEADDR, and closes the socket at once: it fails with EADDRINUSE when
// a listener holds the port on IPv6 where a bind of addr conflicts ([::1]:port
// or [::]:port, with IPV6_V6ONLY, for addr ::1; any IPv6 listener on the port
// for addr ::), which keploy's IPv4 forwarder's own bind does not find. It
// never listens, so no call can reach it.
func probeIPv6Port(addr netip.Addr, port uint16) error {
	fd, err := unix.Socket(unix.AF_INET6, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return err
	}
	defer func() { _ = unix.Close(fd) }()
	if err := unix.SetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_REUSEADDR, 1); err != nil {
		return err
	}
	if err := unix.SetsockoptInt(fd, unix.IPPROTO_IPV6, unix.IPV6_V6ONLY, 1); err != nil {
		return err
	}
	return unix.Bind(fd, &unix.SockaddrInet6{Port: int(port), Addr: addr.As16()})
}
