//go:build !linux

package util

import "net/netip"

// TCPListeners does not read the socket table here: ErrNoSocketTable.
func TCPListeners(uint16) ([]netip.Addr, error) { return nil, ErrNoSocketTable }
