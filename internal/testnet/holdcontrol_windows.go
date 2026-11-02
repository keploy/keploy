//go:build windows

package testnet

import (
	"errors"
	"syscall"
)

// soExclusiveAddrUse is Winsock's SO_EXCLUSIVEADDRUSE, ~SO_REUSEADDR.
const soExclusiveAddrUse = ^syscall.SO_REUSEADDR

// holdControl makes the socket that holds a port bind it exclusively
// (SO_EXCLUSIVEADDRUSE), before Go binds it at the dial. Go binds a TCP
// socket it dials from at the wildcard address, and Windows lets a socket
// that sets no option bind a specific address, such as 127.0.0.1, at a port
// another socket holds at the wildcard, unless that socket binds
// exclusively. With the option no other socket can bind the port while the
// hold is open.
func holdControl(_, _ string, c syscall.RawConn) error {
	var serr error
	err := c.Control(func(fd uintptr) {
		serr = syscall.SetsockoptInt(syscall.Handle(fd), syscall.SOL_SOCKET, soExclusiveAddrUse, 1)
	})
	return errors.Join(err, serr)
}
