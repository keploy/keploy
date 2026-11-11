//go:build !windows

package proxy

import "syscall"

// errAddrInUse is what a bind to an address and port another socket holds
// ends in; errAddrNotHere is what a bind to an address no interface has
// ends in.
var errAddrInUse, errAddrNotHere error = syscall.EADDRINUSE, syscall.EADDRNOTAVAIL
