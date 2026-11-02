//go:build !linux

package relay

import "net"

// resetClientOnClose: off Linux the client socket is not asked what it holds,
// so its close stays a FIN, as before.
func resetClientOnClose(net.Conn) bool { return false }
