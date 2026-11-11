//go:build unix

package testnet

import "syscall"

// ErrAddrInUse is what a bind to an address and port another socket holds
// ends in.
var ErrAddrInUse error = syscall.EADDRINUSE

// ErrAddrNotAvail is what a bind to an address no interface has ends in.
var ErrAddrNotAvail error = syscall.EADDRNOTAVAIL
