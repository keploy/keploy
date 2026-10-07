//go:build !windows

package proxy

import (
	"errors"
	"syscall"
)

// isConnRefused reports whether a dial error is a refused connection — the
// signal that the ingress target is not listening yet, so the dial should be
// retried until the deadline. On POSIX platforms that is ECONNREFUSED.
func isConnRefused(err error) bool {
	return errors.Is(err, syscall.ECONNREFUSED)
}
