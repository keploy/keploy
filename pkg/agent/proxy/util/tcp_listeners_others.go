//go:build !linux

package util

import "net/netip"

// TCPListeners does not read the socket table here: ErrNoSocketTable.
func TCPListeners(uint16) ([]netip.Addr, error) { return nil, ErrNoSocketTable }

// TCPListenSockets does not read the socket table here: ErrNoSocketTable.
func TCPListenSockets(uint16) ([]ListenSocket, error) { return nil, ErrNoSocketTable }
