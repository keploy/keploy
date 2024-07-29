// go:build linux

package mysql

import (
	"context"
	"io"
	"net"
	"time"

	"go.keploy.io/server/v2/pkg/core/proxy/integrations/mysql/operation"
	pUtil "go.keploy.io/server/v2/pkg/core/proxy/util"
	"go.keploy.io/server/v2/pkg/models"
	"go.keploy.io/server/v2/pkg/models/mysql"
	"go.keploy.io/server/v2/utils"
	"go.uber.org/zap"
)

func handleClientQueries(ctx context.Context, logger *zap.Logger, clientConn, destConn net.Conn, mocks chan<- *models.Mock, reqTimestamp time.Time, decodeCtx *operation.DecodeContext) error {
	var (
		requests  []mysql.Request
		responses []mysql.Response
	)

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
			// read the command from the client
			command, err := pUtil.ReadBytes(ctx, logger, clientConn)
			if err != nil {
				if err != io.EOF {
					utils.LogError(logger, err, "failed to read command from the mysql client")
				}
				return err
			}

			// write the command to the destination server
			_, err = destConn.Write(command)
			if err != nil {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				utils.LogError(logger, err, "failed to write command to mysql server")
				return err
			}

			if len(command) == 0 {
				break
			}

			commandPkt, err := operation.DecodePayload(ctx, logger, command, clientConn, decodeCtx)
			if err != nil {
				utils.LogError(logger, err, "failed to decode the MySQL packet from the client")
				return err
			}

			//TODO: why directly requests is not used as the first argument
			requests = append([]mysql.Request{}, mysql.Request{
				PacketBundle: *commandPkt,
			})

			// read the command response from the destination server
			commandResp, err := pUtil.ReadBytes(ctx, logger, destConn)
			if err != nil {
				if err != io.EOF {
					utils.LogError(logger, err, "failed to read command response from mysql server")
				}
				return err
			}
			_, err = clientConn.Write(commandResp)
			if err != nil {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				utils.LogError(logger, err, "failed to write command response to mysql client")
				return err
			}

			if len(commandResp) == 0 {
				break
			}

			commandRespPkt, err := operation.DecodePayload(ctx, logger, commandResp, clientConn, decodeCtx)
			if err != nil {
				utils.LogError(logger, err, "failed to decode the MySQL packet from the server")
				return err
			}

			responses = append([]mysql.Response{}, mysql.Response{
				PacketBundle: *commandRespPkt,
			})

			recordMock(ctx, requests, responses, "mocks", commandPkt.Header.Type, commandRespPkt.Header.Type, mocks, reqTimestamp)
		}
	}
}
