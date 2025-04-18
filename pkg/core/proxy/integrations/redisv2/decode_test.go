//go:build linux
package redisv2

import (
	"context"
	"net"
	"testing"

	"github.com/stretchr/testify/require"
	integrationsMocks "go.keploy.io/server/v2/mocks/pkg/core/proxy/integrations"
	"go.keploy.io/server/v2/pkg/models"
	"go.uber.org/zap"
)

// Test generated using Keploy
func TestDecodeRedis_BasicScenario_123(t *testing.T) {
	// Arrange
	ctx := context.Background()
	logger := zap.NewNop()
	reqBuf := []byte("PING")
	clientConn := &net.TCPConn{}
	dstCfg := &models.ConditionalDstCfg{
		Addr: "127.0.0.1:6379",
		Port: 6379,
	}
	mockDb := new(integrationsMocks.MockMemDb)
	outgoingOptions := models.OutgoingOptions{}

	// Act
	err := decodeRedis(ctx, logger, reqBuf, clientConn, dstCfg, mockDb, outgoingOptions)

	// Assert
	require.NoError(t, err)
}

