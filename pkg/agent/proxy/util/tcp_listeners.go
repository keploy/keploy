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
}
