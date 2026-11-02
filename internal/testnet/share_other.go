//go:build !linux && !windows

package testnet

import "syscall"

// shareControl sets nothing: Go's listeners set SO_REUSEADDR here, and on
// macOS that binds beside the connected socket that holds a SharedPort.
var shareControl func(network, address string, c syscall.RawConn) error
