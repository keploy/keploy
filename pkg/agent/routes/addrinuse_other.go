//go:build !windows

package routes

import (
	"errors"
	"syscall"
)

// isAddrInUse reports whether err is a bind's "address already in use".
func isAddrInUse(err error) bool { return errors.Is(err, syscall.EADDRINUSE) }
