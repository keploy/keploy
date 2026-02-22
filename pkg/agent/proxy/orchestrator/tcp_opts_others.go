//go:build !linux

package orchestrator

import "net"

// SetTCPQuickACK is a no-op on non-Linux platforms.
// TCP_QUICKACK is a Linux-only socket option.
func SetTCPQuickACK(_ net.Conn) {}

// extractTCPfd is a no-op on non-Linux platforms — returns -1.
func extractTCPfd(_ net.Conn) int { return -1 }

// quickACKByFD is a no-op on non-Linux platforms.
func quickACKByFD(_ int) {}
