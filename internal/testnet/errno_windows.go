//go:build windows

package testnet

import "golang.org/x/sys/windows"

// errAddrInUse is what a bind that sets no option, as Go's do, ends in when
// another socket that did not bind exclusively is at the same address and
// port (two wildcard binds, or two binds of one address). A bind that names
// 127.0.0.1 beside such a socket at the wildcard address succeeds; beside an
// exclusive one there it ends in WSAEACCES instead.
var errAddrInUse error = windows.WSAEADDRINUSE
