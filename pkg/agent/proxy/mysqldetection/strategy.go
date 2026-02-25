package mysqldetection

import (
	"context"
	"net"

	"go.keploy.io/server/v3/pkg/agent"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// MySQLDetectionStrategy defines the interface for MySQL detection strategies
type MySQLDetectionStrategy interface {
	// ShouldHandle checks if this strategy should handle the connection
	// Returns true if this strategy should process the connection
	ShouldHandle(ctx context.Context, destInfo *agent.NetworkAddress, initialBuf []byte) bool
	
	// HandleConnection processes the MySQL connection
	HandleConnection(
		ctx context.Context,
		parserCtx context.Context,
		srcConn net.Conn,
		dstAddr string,
		destInfo *agent.NetworkAddress,
		rule *agent.Session,
		outgoingOpts models.OutgoingOptions,
		mysqlIntegration integrations.Integrations,
		mockManager interface{},
		logger *zap.Logger,
		sendError func(error),
	) error
}
