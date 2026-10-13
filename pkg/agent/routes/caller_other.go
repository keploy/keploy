//go:build !linux

package routes

import (
	"errors"
	"net/http"
)

// callerPID is only read from the kernel on Linux; the single-worker wire's
// instance claim is Linux-only in the first release.
func callerPID(*http.Request) (int, error) {
	return 0, errors.New("reading the calling process from the connection is supported on Linux only")
}
