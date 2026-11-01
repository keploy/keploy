//go:build !linux

package util

import (
	"errors"
	"net/netip"
)

// ErrNoSocketTable is TCPListeners' error where it cannot read the kernel's
// socket table.
var ErrNoSocketTable = errors.New("the kernel's socket table is not read on this platform")

// TCPListeners cannot read the socket table here: ErrNoSocketTable.
func TCPListeners(uint16) ([]netip.Addr, error) { return nil, ErrNoSocketTable }
