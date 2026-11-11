//go:build windows

package proxy

import "golang.org/x/sys/windows"

// errAddrInUse is what a bind to an address and port another socket holds
// ends in; errAddrNotHere is what a bind to an address no interface has
// ends in. Winsock's own codes: Go's syscall.EADDRINUSE and EADDRNOTAVAIL
// are values of its own on Windows, which no Winsock error matches.
var errAddrInUse, errAddrNotHere error = windows.WSAEADDRINUSE, windows.WSAEADDRNOTAVAIL
