//go:build linux

package replayer

import (
	"context"
	"fmt"
	"io"
	"net"
	"time"

	"go.keploy.io/server/v2/pkg/core/proxy/integrations"
	mysqlUtils "go.keploy.io/server/v2/pkg/core/proxy/integrations/mysql/utils"
	"go.keploy.io/server/v2/pkg/core/proxy/integrations/mysql/wire"
	"go.keploy.io/server/v2/pkg/models"
	"go.keploy.io/server/v2/pkg/models/mysql"
	"go.keploy.io/server/v2/utils"
	"go.uber.org/zap"
)

func simulateCommandPhase(ctx context.Context, logger *zap.Logger, clientConn net.Conn, mockDb integrations.MockMemDb, decodeCtx *wire.DecodeContext, opts models.OutgoingOptions) error {

	// Log initial mock state at the start of command phase
	unfiltered, err := mockDb.GetUnFilteredMocks()
	if err != nil {
		utils.LogError(logger, err, "failed to get unfiltered mocks at command phase start")
	} else {
		var totalMySQLMocks, configMocks, dataMocks int
		for _, mock := range unfiltered {
			if mock.Kind == models.MySQL {
				totalMySQLMocks++
				if mock.Spec.Metadata["type"] == "config" {
					configMocks++
				} else {
					dataMocks++
				}
			}
		}
		logger.Info("Command phase starting",
			zap.Int("total_mysql_mocks", totalMySQLMocks),
			zap.Int("config_mocks", configMocks),
			zap.Int("data_mocks_available", dataMocks))
	}

	commandCount := 0
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
			commandCount++

			logger.Debug("Starting new command iteration",
				zap.Int("command_count", commandCount))

			// Log mock consumption periodically every 10 commands
			if commandCount%10 == 0 {
				logger.Debug("Checking mock consumption at interval",
					zap.Int("command_count", commandCount))
				unfiltered, err := mockDb.GetUnFilteredMocks()
				if err == nil {
					var totalMySQLMocks, configMocks, dataMocks int
					for _, mock := range unfiltered {
						if mock.Kind == models.MySQL {
							totalMySQLMocks++
							if mock.Spec.Metadata["type"] == "config" {
								configMocks++
							} else {
								dataMocks++
							}
						}
					}
					logger.Debug("Mock consumption progress",
						zap.Int("command_count", commandCount),
						zap.Int("remaining_data_mocks", dataMocks),
						zap.Int("remaining_config_mocks", configMocks),
						zap.Int("total_mysql_mocks_remaining", totalMySQLMocks))
				} else {
					logger.Debug("Failed to get mock stats at interval", zap.Error(err))
				}
			}

			// Set a read deadline on the client connection
			readTimeout := 2 * time.Second * time.Duration(opts.SQLDelay)
			err := clientConn.SetReadDeadline(time.Now().Add(readTimeout))
			if err != nil {
				utils.LogError(logger, err, "failed to set read deadline on client conn")
				return err
			}

			logger.Debug("About to read next command from client",
				zap.Int("command_count", commandCount),
				zap.Duration("read_timeout", readTimeout))

			// read the command from the client
			command, err := mysqlUtils.ReadPacketBuffer(ctx, logger, clientConn)
			if err != nil {
				if ne, ok := err.(net.Error); ok && ne.Timeout() {
					// Idle wait: keep the connection open and continue polling
					logger.Debug("read timeout waiting for next client command; keeping connection open")
					// Optional: back off a bit to avoid hot loop
					time.Sleep(50 * time.Millisecond)
					// Clear deadline or set another future deadline, then keep looping
					_ = clientConn.SetReadDeadline(time.Now().Add(readTimeout))
					continue
				}
				if err == io.EOF {
					logger.Debug("client closed the connection (EOF)")
				} else {
					utils.LogError(logger, err, "failed to read command packet from client")
				}
				return err
			}

			logger.Debug("Successfully read command from client",
				zap.Int("command_count", commandCount),
				zap.Int("command_size_bytes", len(command)))

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
					logger.Info("Connection closing due to EOF from matchCommand",
						zap.Int("commands_processed", commandCount),
						zap.String("request_type", req.Header.Type))
					return io.EOF
				}
				logger.Error("Connection closing due to match command error",
					zap.Error(err),
					zap.Int("commands_processed", commandCount),
					zap.String("request_type", req.Header.Type))
				utils.LogError(logger, err, "failed to match the command")
				return err
			}

			if !ok {
				logger.Error("Connection closing due to no matching mock found",
					zap.Int("commands_processed", commandCount),
					zap.String("request_type", req.Header.Type))
				utils.LogError(logger, nil, "No matching mock found for the command", zap.Any("command", command))
				return fmt.Errorf("error while simulating the command phase due to no matching mock found")
			}

			logger.Debug("Matched the command with the mock", zap.Any("mock", resp))

			// Handle prepared statement cleanup for COM_STMT_CLOSE
			if commandPkt.Header.Type == mysql.CommandStatusToString(mysql.COM_STMT_CLOSE) {
				if closePacket, ok := commandPkt.Message.(*mysql.StmtClosePacket); ok {
					delete(decodeCtx.PreparedStatements, closePacket.StatementID)
					logger.Debug("Cleaned up prepared statement", zap.Uint32("StatementID", closePacket.StatementID))
				}
			}

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
				return err
			}

			logger.Debug("About to write response to client",
				zap.String("request_type", req.Header.Type),
				zap.String("response_type", resp.Header.Type),
				zap.Int("response_size_bytes", len(buf)),
				zap.Int("commands_processed", commandCount))

			// Write the response to the client
			_, err = clientConn.Write(buf)
			if err != nil {
				if ctx.Err() != nil {
					logger.Debug("context done while writing the response to the client", zap.Error(ctx.Err()))
					return ctx.Err()
				}
				logger.Error("Failed to write response to client",
					zap.Error(err),
					zap.String("request_type", req.Header.Type),
					zap.Int("commands_processed", commandCount))
				utils.LogError(logger, err, "failed to write the response to the client")
				return err
			}

			logger.Debug("successfully wrote the response to the client",
				zap.Any("request", req.Header.Type),
				zap.String("response_type", resp.Header.Type),
				zap.Int("response_size_bytes", len(buf)),
				zap.Int("commands_processed", commandCount))

			// Check connection state after writing large response
			if len(buf) > 1000 {
				logger.Debug("Large response written, checking connection state",
					zap.Int("response_size_bytes", len(buf)),
					zap.String("request_type", req.Header.Type))
			}

			// Add a small delay and log to see if connection is still alive
			logger.Debug("Response write completed, continuing to next iteration",
				zap.Int("commands_processed", commandCount),
				zap.String("last_request", req.Header.Type))
		}
	}
}
