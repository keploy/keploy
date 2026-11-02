//go:build windows

package testnet

import "syscall"

// shareControl sets SO_REUSEADDR on a listener's socket before it binds.
// Go's listeners set no option on Windows, and a bind without it is refused
// a port a connected socket holds. With it, the bind takes the port beside
// the connection that holds it (SharedPort): the connection answers only its
// peer, so a dial from anywhere else reaches the listener.
func shareControl(_, _ string, c syscall.RawConn) error {
	var serr error
	if err := c.Control(func(fd uintptr) {
		serr = syscall.SetsockoptInt(syscall.Handle(fd), syscall.SOL_SOCKET, syscall.SO_REUSEADDR, 1)
	}); err != nil {
		return err
	}
	return serr
}
