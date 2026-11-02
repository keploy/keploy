//go:build !linux

package testnet

import "syscall"

// shareControl sets nothing. On macOS Go's listeners set SO_REUSEADDR,
// which binds beside the connected socket that holds a SharedPort. On
// Windows no socket is at a SharedPort (it is claimed: sharedPort), and a
// listener that sets no option, as Go's there do, fails to bind a port
// another socket is at rather than share it.
var shareControl func(network, address string, c syscall.RawConn) error
