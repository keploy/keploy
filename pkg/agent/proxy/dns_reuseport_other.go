//go:build !aix && !darwin && !dragonfly && !freebsd && !linux && !netbsd && !openbsd

package proxy

import "syscall"

// dnsReusePort is nil here: miekg/dns's ReusePort sets nothing on these
// platforms either.
var dnsReusePort func(network, address string, c syscall.RawConn) error
