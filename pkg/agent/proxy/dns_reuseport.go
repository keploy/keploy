//go:build aix || darwin || dragonfly || freebsd || linux || netbsd || openbsd

package proxy

import (
	"syscall"

	"golang.org/x/sys/unix"
)

// dnsReusePort sets SO_REUSEPORT on the TCP DNS server's socket before it is
// bound, as miekg/dns's ReusePort does on the platforms it sets it on (these).
func dnsReusePort(_, _ string, c syscall.RawConn) error {
	var opErr error
	if err := c.Control(func(fd uintptr) {
		opErr = unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_REUSEPORT, 1)
	}); err != nil {
		return err
	}
	return opErr
}
