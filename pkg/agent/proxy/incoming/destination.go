package proxy

import (
	"context"
	"net"

	"go.uber.org/zap"
)

// getActualDestination returns the address to forward an accepted ingress
// connection to.
//
// Every backend now tells Keploy where the application moved to — Linux and
// macOS through their bind hooks, Windows through the shim's — so the target is
// always known by the time a connection arrives and the fallback IS the answer.
//
// Windows used to be the exception: its kernel packet filter left the
// application on its advertised port and rewrote inbound packets, so the real
// destination had to be recovered per connection from the source port. That
// backend is gone, and with it the only reason this was ever platform-specific.
func (pm *IngressProxyManager) getActualDestination(_ context.Context, _ net.Conn, fallbackAddr string, _ *zap.Logger) string {
	return fallbackAddr
}
