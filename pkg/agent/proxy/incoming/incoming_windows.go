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
			logger.Warn("Failed to delete destination entry for gRPC",
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
