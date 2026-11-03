//go:build unix

package testnet

import "syscall"

// errAddrInUse is what a bind to a port another socket holds ends in.
var errAddrInUse error = syscall.EADDRINUSE
