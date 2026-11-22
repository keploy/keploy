//go:build !linux

package testnet

import "syscall"

// shareControl sets nothing. On macOS Go's listeners set SO_REUSEADDR,
// which binds beside the connected socket that holds a SharedPort. On
// Windows no socket is at a SharedPort (it is claimed: sharedPort), so the
// test's listeners bind it as they are: no listener binds beside an
// exclusive hold, and a hold at the wildcard address that is not exclusive
// refuses a listener there and keeps no bind that names 127.0.0.1 off the
// port.
var shareControl func(network, address string, c syscall.RawConn) error
