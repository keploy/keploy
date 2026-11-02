//go:build !linux

package testnet

import "syscall"

// shareControl sets nothing. On macOS Go's listeners set SO_REUSEADDR,
// which binds beside the connected socket that holds a SharedPort. On
// Windows no socket is at a SharedPort (it is claimed: sharedPort), so the
// test's listeners bind it as they are: no listener binds beside an
// exclusive hold, and one that sets no option binds beside a hold that is
// not exclusive, which so keeps no other socket off the port either.
var shareControl func(network, address string, c syscall.RawConn) error
