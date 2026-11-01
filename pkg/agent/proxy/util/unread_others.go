//go:build !linux

package util

import "net"

// UnreadBytes does not ask the socket here: 0, which can only fall short of
// what has arrived.
func UnreadBytes(net.Conn) int64 { return 0 }
