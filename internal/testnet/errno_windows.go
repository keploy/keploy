//go:build windows

package testnet

import "golang.org/x/sys/windows"

// errAddrInUse is what a bind to a port another socket is at ends in when
// that socket did not bind it exclusively. Beside an exclusive socket at the
// wildcard address a bind that names 127.0.0.1 ends in WSAEACCES instead.
var errAddrInUse error = windows.WSAEADDRINUSE
