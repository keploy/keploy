//go:build unix

package testnet

import "syscall"

// holdControl sets nothing on the socket that holds a port: on Linux and
// macOS a socket that sets no option is enough (holdByConnection).
var holdControl func(network, address string, c syscall.RawConn) error
