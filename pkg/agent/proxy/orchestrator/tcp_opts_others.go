//go:build !linux

package orchestrator

import "net"

// SetTCPQuickACK is a no-op on non-Linux platforms.
// TCP_QUICKACK is a Linux-only socket option.
func SetTCPQuickACK(_ net.Conn) {}
