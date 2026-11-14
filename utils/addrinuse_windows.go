//go:build windows

package utils

import (
	"errors"
	"syscall"

	"golang.org/x/sys/windows"
)

// IsAddrInUse says whether err is a bind's refusal of a port another socket
// holds: Winsock's WSAEADDRINUSE, which syscall.EADDRINUSE (an errno of Go's
// own on Windows) does not match, or that errno.
func IsAddrInUse(err error) bool {
	return errors.Is(err, windows.WSAEADDRINUSE) || errors.Is(err, syscall.EADDRINUSE)
}
