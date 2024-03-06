package grpc

import (
	"context"
	"net"

	"go.keploy.io/server/v2/pkg/core/proxy/integrations"
	"go.keploy.io/server/v2/pkg/models"
	"go.keploy.io/server/v2/utils"
	"go.uber.org/zap"
	"golang.org/x/net/http2"
)

func decodeGrpc(ctx context.Context, logger *zap.Logger, reqBuf []byte, clientConn net.Conn, dstCfg *integrations.ConditionalDstCfg, mockDb integrations.MockMemDb, opts models.OutgoingOptions) error {
	framer := http2.NewFramer(clientConn, clientConn)
	srv := NewTranscoder(logger, framer, mockDb)
	// fake server in the test mode
	err := srv.ListenAndServe(ctx)
	if err != nil {
		utils.LogError(logger, nil, "could not serve grpc request")
		return err
	}
	return nil
}
