//go:build !windows

package proxy

import (
	"context"
	"net"

	"go.uber.org/zap"
)

// getActualDestination for non-Windows platforms simply returns the fallback address
func (pm *IngressProxyManager) getActualDestination(ctx context.Context, clientConn net.Conn, fallbackAddr string, logger *zap.Logger) string {
	// On non-Windows platforms, we don't need to look up destination info
	// Just return the provided fallback address
	return fallbackAddr
}
