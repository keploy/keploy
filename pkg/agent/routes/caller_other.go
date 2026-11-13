//go:build !linux && !darwin && !windows

package routes

import (
	"errors"
	"net/netip"
)

// tcpOwner is only read from the kernel on Linux, macOS and Windows; the
// single-worker wire's instance claim is unsupported on other platforms.
func tcpOwner(netip.AddrPort, netip.AddrPort) (int, error) {
	return 0, errors.New("reading the calling process from the connection is supported on Linux, macOS and Windows only")
}
