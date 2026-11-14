//go:build !windows

package utils

import (
	"errors"
	"syscall"
)

// IsAddrInUse says whether err is a bind's refusal of a port another socket
// holds (EADDRINUSE).
func IsAddrInUse(err error) bool { return errors.Is(err, syscall.EADDRINUSE) }
