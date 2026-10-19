//go:build windows

package routes

import (
	"errors"
	"syscall"

	"golang.org/x/sys/windows"
)

// isAddrInUse reports whether err is a bind's "address already in use":
// Winsock's WSAEADDRINUSE, which syscall.EADDRINUSE (an errno of Go's own on
// Windows) is not.
func isAddrInUse(err error) bool {
	return errors.Is(err, windows.WSAEADDRINUSE) || errors.Is(err, syscall.EADDRINUSE)
}
