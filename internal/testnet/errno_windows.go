//go:build windows

package testnet

import "golang.org/x/sys/windows"

// errAddrInUse is what a bind to a port another socket holds ends in.
var errAddrInUse error = windows.WSAEADDRINUSE
