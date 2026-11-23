package util

import (
	"errors"
	"net/netip"
)

// ErrNoSocketTable is TCPListeners' error on a platform where it does not
// read the kernel's socket table (all but Linux).
var ErrNoSocketTable = errors.New("the kernel's socket table is not read on this platform")

// ListenSocket is a TCP socket that listens: its local address, as
// TCPListeners gives it, and its inode, the number a process's fd that holds
// it links to ("socket:[inode]") and /proc/net/tcp's inode column shows.
type ListenSocket struct {
	Addr  netip.Addr
	Inode uint32
	// Family is the socket's own address family: AF_INET or AF_INET6 (an
	// IPv6 socket on a v4-mapped address is AF_INET6, its Addr IPv4).
	Family uint8
	// Cookie is the socket's cookie (SO_COOKIE), which no other socket of
	// this boot has.
	Cookie uint64
	// V6Only is an IPv6 socket's IPV6_V6ONLY, when V6OnlyKnown: the table
	// says it for every IPv6 socket.
	V6Only, V6OnlyKnown bool
}
