//go:build windows

package testnet

import "golang.org/x/sys/windows"

// ErrAddrInUse is what a bind that sets no option, as Go's do, ends in when
// another socket that did not bind exclusively is at the same address and
// port (two wildcard binds, or two binds of one address). A bind that names
// 127.0.0.1 beside such a socket at the wildcard address succeeds; beside an
// exclusive one there it ends in WSAEACCES instead. It is Winsock's code:
// Go's syscall.EADDRINUSE is a value of its own on Windows, which no Winsock
// error matches.
var ErrAddrInUse error = windows.WSAEADDRINUSE

// ErrAddrNotAvail is what a bind to an address no interface has ends in:
// Winsock's WSAEADDRNOTAVAIL, as syscall.EADDRNOTAVAIL matches no Winsock
// error either.
var ErrAddrNotAvail error = windows.WSAEADDRNOTAVAIL
