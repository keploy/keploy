//go:build !unix

package http

import (
	"net"
	"testing"
)

// heldLoopbackPort needs a socket bound and not listening, then handed over
// as a listener (net.FileListener), which Go offers only on unix systems.
func heldLoopbackPort(t *testing.T) (int, func() (net.Listener, error)) {
	t.Skip("a bound socket cannot be handed over as a listener here")
	return 0, nil
}
