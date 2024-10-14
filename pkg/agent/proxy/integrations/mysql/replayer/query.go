//go:build linux

package replayer

import (
	"context"
	"fmt"
	"io"
	"net"
	"time"

	"go.keploy.io/server/v2/pkg/agent/proxy/integrations"
	mysqlUtils "go.keploy.io/server/v2/pkg/agent/proxy/integrations/mysql/utils"
	"go.keploy.io/server/v2/pkg/agent/proxy/integrations/mysql/wire"
	"go.keploy.io/server/v2/pkg/models"
	"go.keploy.io/server/v2/pkg/models/mysql"
	"go.keploy.io/server/v2/utils"
	"go.uber.org/zap"
)

func simulateCommandPhase(ctx context.Context, logger *zap.Logger, clientConn net.Conn, mockDb integrations.MockMemDb, decodeCtx *wire.DecodeContext, opts models.OutgoingOptions) error {

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:

			// Set a read deadline on the client connection
			readTimeout := 2 * time.Second * time.Duration(opts.SQLDelay)
			err := clientConn.SetReadDeadline(time.Now().Add(readTimeout))
			if err != nil {
				utils.LogError(logger, err, "failed to set read deadline on client conn")
				return err
			}

			// read the command from the client
			command, err := mysqlUtils.ReadPacketBuffer(ctx, logger, clientConn)
			if err != nil {
				// when the read deadline is reached, we should close the connection
				if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
					logger.Debug("closing the client connection since the read deadline is reached")
					return io.EOF
				}
				if err != io.EOF {
					utils.LogError(logger, err, "failed to read command packet from client")
				}
				return err
			}

			// reset the read deadline
			err = clientConn.SetReadDeadline(time.Time{})
			if err != nil {
				utils.LogError(logger, err, "failed to reset read deadline on client conn")
				return err
			}

			// Decode the command
			commandPkt, err := wire.DecodePayload(ctx, logger, command, clientConn, decodeCtx)
			if err != nil {
				utils.LogError(logger, err, "failed to decode the MySQL packet from the client")
			}

			req := mysql.Request{
				PacketBundle: *commandPkt,
			}

			// Match the request with the mock
			resp, ok, err := matchCommand(ctx, logger, req, mockDb, decodeCtx)
			if err != nil {
				if err == io.EOF {
					return io.EOF
				}
				utils.LogError(logger, err, "failed to match the command")
				return err
			}

			if !ok {
				utils.LogError(logger, nil, "No matching mock found for the command", zap.Any("command", command))
				return fmt.Errorf("error while simulating the command phase due to no matching mock found")
			}

			logger.Debug("Matched the command with the mock", zap.Any("mock", resp))

			// We could have just returned before matching the command for no response commands.
			// But we need to remove the corresponding mock from the mockDb for no response commands.
			if wire.IsNoResponseCommand(commandPkt.Header.Type) {
				// No response for COM_STMT_CLOSE and COM_STMT_SEND_LONG_DATA
				logger.Debug("No response for the command", zap.Any("command", command))
				continue
			}

			//Encode the matched resp
			buf, err := wire.EncodeToBinary(ctx, logger, &resp.PacketBundle, clientConn, decodeCtx)
			if err != nil {
				utils.LogError(logger, err, "failed to encode the response", zap.Any("response", resp))
			}

			// Write the response to the client
			_, err = clientConn.Write(buf)
			if err != nil {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				utils.LogError(logger, err, "failed to write the response to the client")
				return err
			}

			logger.Debug("successfully wrote the response to the client", zap.Any("request", req.Header.Type))
		}
	}
}
