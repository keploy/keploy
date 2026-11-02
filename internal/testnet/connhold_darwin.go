//go:build darwin

package testnet

import (
	"net"

	"golang.org/x/sys/unix"
)

// connectHolder connects to peer from a port no bind to port 0 is given.
func connectHolder(peer string) (*net.TCPConn, error) {
	return connectUnpicked(peer, unix.SysctlUint32)
}
