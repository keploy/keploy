// Package grpc provides functionality for integrating with gRPC outgoing calls.
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

func decodeGrpc(ctx context.Context, logger *zap.Logger, _ []byte, clientConn net.Conn, _ *integrations.ConditionalDstCfg, mockDb integrations.MockMemDb, _ models.OutgoingOptions) error {
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
