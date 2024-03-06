package v1

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"time"

	"github.com/jackc/pgproto3/v2"
	"go.keploy.io/server/v2/pkg/core/proxy/integrations"
	"go.keploy.io/server/v2/pkg/core/proxy/integrations/util"
	pUtil "go.keploy.io/server/v2/pkg/core/proxy/util"
	"go.keploy.io/server/v2/pkg/models"
	"go.keploy.io/server/v2/utils"
	"go.uber.org/zap"
)

func decodePostgres(ctx context.Context, logger *zap.Logger, reqBuf []byte, clientConn net.Conn, dstCfg *integrations.ConditionalDstCfg, mockDb integrations.MockMemDb, opts models.OutgoingOptions) error {
	pgRequests := [][]byte{reqBuf}

	for {
		select {
		case <-ctx.Done():
			return nil
		default:
			// Since protocol packets have to be parsed for checking stream end,
			// clientConnection have deadline for read to determine the end of stream.

			err := clientConn.SetReadDeadline(time.Now().Add(10 * time.Millisecond))
			if err != nil {
				utils.LogError(logger, err, "failed to set the read deadline for the pg client conn")
				return err
			}

			// To read the stream of request packets from the client
			for {
				buffer, err := pUtil.ReadBytes(ctx, clientConn)
				if err != nil {
					if netErr, ok := err.(net.Error); !(ok && netErr.Timeout()) && err != nil {
						if err == io.EOF {
							logger.Debug("EOF error received from client. Closing conn in postgres !!")
							return err
						}
						//TODO: why debug log sarthak?
						logger.Debug("failed to read the request message in proxy for postgres dependency")
						return err
					}
				}
				if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
					logger.Debug("the timeout for the client read in pg")
					break
				}
				pgRequests = append(pgRequests, buffer)
			}

			if len(pgRequests) == 0 {
				logger.Debug("the postgres request buffer is empty")
				continue
			}

			matched, pgResponses, err := matchingReadablePG(ctx, logger, pgRequests, mockDb)
			if err != nil {
				return fmt.Errorf("error while matching tcs mocks %v", err)
			}

			if !matched {

				// making destConn
				destConn, err := net.Dial("tcp", dstCfg.Addr)
				if err != nil {
					utils.LogError(logger, err, "failed to dial the destination server")
					return err
				}

				_, err = pUtil.PassThrough(ctx, logger, clientConn, destConn, pgRequests)
				if err != nil {
					utils.LogError(logger, err, "failed to pass the request", zap.Any("request packets", len(pgRequests)))
					return err
				}
				continue
			}
			for _, pgResponse := range pgResponses {
				encoded, err := util.DecodeBase64(pgResponse.Payload)
				if len(pgResponse.PacketTypes) > 0 && len(pgResponse.Payload) == 0 {
					encoded, err = postgresDecoderFrontend(pgResponse)
				}
				if err != nil {
					utils.LogError(logger, err, "failed to decode the response message in proxy for postgres dependency")
					return err
				}
				_, err = clientConn.Write(encoded)
				if err != nil {
					utils.LogError(logger, err, "failed to write the response message to the client application")
					return err
				}
			}
			// Clear the buffer for the next dependency call
			pgRequests = [][]byte{}
		}
	}
}

func decodePgRequest(logger *zap.Logger, buffer []byte) *models.Backend {
	pg := NewBackend()

	if !isStartupPacket(buffer) && len(buffer) > 5 {
		bufferCopy := buffer
		for i := 0; i < len(bufferCopy)-5; {
			logger.Debug("Inside the if condition")
			pg.BackendWrapper.MsgType = buffer[i]
			pg.BackendWrapper.BodyLen = int(binary.BigEndian.Uint32(buffer[i+1:])) - 4
			if len(buffer) < (i + pg.BackendWrapper.BodyLen + 5) {
				utils.LogError(logger, nil, "failed to translate the postgres request message due to shorter network packet buffer")
				break
			}
			msg, err := pg.translateToReadableBackend(buffer[i:(i + pg.BackendWrapper.BodyLen + 5)])
			if err != nil && buffer[i] != 112 {
				utils.LogError(logger, err, "failed to translate the request message to readable")
			}
			if pg.BackendWrapper.MsgType == 'p' {
				pg.BackendWrapper.PasswordMessage = *msg.(*pgproto3.PasswordMessage)
			}

			if pg.BackendWrapper.MsgType == 'P' {
				pg.BackendWrapper.Parse = *msg.(*pgproto3.Parse)
				pg.BackendWrapper.Parses = append(pg.BackendWrapper.Parses, pg.BackendWrapper.Parse)
			}

			if pg.BackendWrapper.MsgType == 'B' {
				pg.BackendWrapper.Bind = *msg.(*pgproto3.Bind)
				pg.BackendWrapper.Binds = append(pg.BackendWrapper.Binds, pg.BackendWrapper.Bind)
			}

			if pg.BackendWrapper.MsgType == 'E' {
				pg.BackendWrapper.Execute = *msg.(*pgproto3.Execute)
				pg.BackendWrapper.Executes = append(pg.BackendWrapper.Executes, pg.BackendWrapper.Execute)
			}

			pg.BackendWrapper.PacketTypes = append(pg.BackendWrapper.PacketTypes, string(pg.BackendWrapper.MsgType))

			i += 5 + pg.BackendWrapper.BodyLen
		}

		pg_mock := &models.Backend{
			PacketTypes: pg.BackendWrapper.PacketTypes,
			Identfier:   "ClientRequest",
			Length:      uint32(len(buffer)),
			// Payload:             bufStr,
			Bind:                pg.BackendWrapper.Bind,
			Binds:               pg.BackendWrapper.Binds,
			PasswordMessage:     pg.BackendWrapper.PasswordMessage,
			CancelRequest:       pg.BackendWrapper.CancelRequest,
			Close:               pg.BackendWrapper.Close,
			CopyData:            pg.BackendWrapper.CopyData,
			CopyDone:            pg.BackendWrapper.CopyDone,
			CopyFail:            pg.BackendWrapper.CopyFail,
			Describe:            pg.BackendWrapper.Describe,
			Execute:             pg.BackendWrapper.Execute,
			Executes:            pg.BackendWrapper.Executes,
			Flush:               pg.BackendWrapper.Flush,
			FunctionCall:        pg.BackendWrapper.FunctionCall,
			GssEncRequest:       pg.BackendWrapper.GssEncRequest,
			Parse:               pg.BackendWrapper.Parse,
			Parses:              pg.BackendWrapper.Parses,
			Query:               pg.BackendWrapper.Query,
			SSlRequest:          pg.BackendWrapper.SSlRequest,
			StartupMessage:      pg.BackendWrapper.StartupMessage,
			SASLInitialResponse: pg.BackendWrapper.SASLInitialResponse,
			SASLResponse:        pg.BackendWrapper.SASLResponse,
			Sync:                pg.BackendWrapper.Sync,
			Terminate:           pg.BackendWrapper.Terminate,
			MsgType:             pg.BackendWrapper.MsgType,
			AuthType:            pg.BackendWrapper.AuthType,
		}
		return pg_mock
	}
	return nil
}
