//go:build windows

package proxy

import (
	"context"
	"fmt"
	"net"
	"strconv"

	"go.keploy.io/server/v3/pkg/agent/proxy/util"
	"go.uber.org/zap"
)

// getActualDestination gets the real destination for Windows connections using hooks
func (pm *IngressProxyManager) getActualDestination(ctx context.Context, clientConn net.Conn, fallbackAddr string, logger *zap.Logger) string {
	// The source-port lookup below only makes sense when the forwarding target
	// is unknown — the WinDivert backend, which leaves the application on its
	// advertised port and has the kernel redirect inbound packets, so the real
	// destination has to be recovered per connection. StartIngress signals that
	// case with a zero port in fallbackAddr ("127.0.0.1:0").
	//
	// The unprivileged backend instead MOVES the application's listener and
	// hands us its port, so the target is already known and the lookup is not
	// merely redundant — it is unsafe. Its destination map holds the app's
	// OUTGOING connections keyed by source port, and an entry survives until the
	// proxy consumes it. An inbound client that happened to be assigned a port
	// matching a stale entry would be forwarded to that external host instead of
	// to the application, and would consume the mapping the real outgoing
	// connection still needed.
	if _, portStr, err := net.SplitHostPort(fallbackAddr); err == nil {
		if port, perr := strconv.ParseUint(portStr, 10, 16); perr == nil && port != 0 {
			return fallbackAddr
		}
	}

	// Extract source port from client connection
	clientAddr := clientConn.RemoteAddr().String()
	_, srcPortStr, err := net.SplitHostPort(clientAddr)
	if err != nil {
		logger.Debug("Failed to parse client address, using fallback",
			zap.String("clientAddr", clientAddr),
			zap.String("fallback", fallbackAddr))
		return fallbackAddr
	}

	srcPort64, err := strconv.ParseUint(srcPortStr, 10, 16)
	if err != nil {
		logger.Debug("Failed to parse source port, using fallback",
			zap.String("srcPort", srcPortStr),
			zap.String("fallback", fallbackAddr))
		return fallbackAddr
	}
	srcPort := uint16(srcPort64)

	// Get Windows destination info from hooks
	networkAddr, err := pm.hooks.Get(ctx, srcPort)
	if err == nil && networkAddr != nil {
		// Convert IP to string and build new address
		var destIP string
		if networkAddr.Version == 4 {
			destIP = util.ToIP4AddressStr(networkAddr.IPv4Addr)
		} else {
			destIP = util.ToIPv6AddressStr(networkAddr.IPv6Addr)
		}

		finalDestAddr := fmt.Sprintf("%s:%d", destIP, networkAddr.Port)

		logger.Debug("Found Windows destination for gRPC",
			zap.String("original", fallbackAddr),
			zap.String("actual", finalDestAddr),
			zap.Uint16("srcPort", srcPort))

		// Delete the entry from hooks to clean up
		if deleteErr := pm.hooks.Delete(ctx, srcPort); deleteErr != nil {
			logger.Debug("Failed to delete destination entry for gRPC",
				zap.Uint16("srcPort", srcPort),
				zap.Error(deleteErr))
		}

		return finalDestAddr
	}

	logger.Debug("No Windows destination found for gRPC, using fallback",
		zap.Uint16("srcPort", srcPort),
		zap.String("fallback", fallbackAddr),
		zap.Error(err))
	return fallbackAddr
}
