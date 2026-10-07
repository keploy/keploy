//go:build windows

package proxy

import (
	"errors"
	"syscall"

	"golang.org/x/sys/windows"
)

// isConnRefused reports whether a dial error is a refused connection — the
// signal that the ingress target is not listening yet, so the dial should be
// retried until the deadline. On Windows a refused TCP connect surfaces as
// WSAECONNREFUSED (10061), not the POSIX ECONNREFUSED, and the std syscall
// package does not export it — so both are matched (WSAECONNREFUSED via
// golang.org/x/sys/windows). Without it the retry never engaged and
// dialIngressTarget gave up immediately
// (TestDialIngressTarget{WaitsForAppToListen,GivesUpAfterTimeout}).
func isConnRefused(err error) bool {
	return errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, windows.WSAECONNREFUSED)
}
