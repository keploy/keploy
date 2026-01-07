package recorder

import (
	"context"
	"net"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// Record records the Kafka traffic between the client and the broker.
func Record(ctx context.Context, logger *zap.Logger, clientConn, destConn net.Conn, mocks chan<- *models.Mock, clientClose chan bool, opts models.OutgoingOptions) error {
	// TODO: implement the record function
	return nil
}


