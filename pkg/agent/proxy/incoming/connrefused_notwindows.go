//go:build !windows

package proxy

import (
	"errors"
	"syscall"
)

// isConnRefused reports whether a dial error is a refused connection: the
// app not listening there, yet or any more (dialApp). On POSIX platforms
// that is ECONNREFUSED.
func isConnRefused(err error) bool {
	return errors.Is(err, syscall.ECONNREFUSED)
}
