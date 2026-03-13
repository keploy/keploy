package replayer

import (
	"context"
	"net"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

func Replay(ctx context.Context, logger *zap.Logger, src net.Conn, dstCfg *models.ConditionalDstCfg, mockDb integrations.MockMemDb, opts models.OutgoingOptions) error {
	// TODO: Implement replaying logic
	// 1. Loop read from src
	// 2. Decode Request
	// 3. Match with mock
	// 4. Encode Response
	// 5. Write to src
	
	// If pass-through mode:
	// Handle connection to dstCfg
	return nil
}
