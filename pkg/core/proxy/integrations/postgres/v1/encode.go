package v1

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"strconv"
	"time"

	"github.com/jackc/pgproto3/v2"
	"go.keploy.io/server/v2/pkg/core/proxy/integrations/util"
	pUtil "go.keploy.io/server/v2/pkg/core/proxy/util"
	"go.keploy.io/server/v2/pkg/models"
	"go.keploy.io/server/v2/utils"
	"go.uber.org/zap"
	"golang.org/x/sync/errgroup"
)

func encodePostgres(ctx context.Context, logger *zap.Logger, reqBuf []byte, clientConn, destConn net.Conn, mocks chan<- *models.Mock, _ models.OutgoingOptions) error {

	logger.Debug("Inside the encodePostgresOutgoing function")
	var pgRequests []models.Backend

	bufStr := util.EncodeBase64(reqBuf)
	logger.Debug("bufStr is ", zap.String("bufStr", bufStr))

	pg := NewBackend()
	_, err := pg.decodeStartupMessage(reqBuf)
	if err != nil {
		utils.LogError(logger, err, "failed to decode startup message server")
	}

	if bufStr != "" {
		pgRequests = append(pgRequests, models.Backend{
			PacketTypes:         pg.BackendWrapper.PacketTypes,
			Identfier:           "StartupRequest",
			Length:              uint32(len(reqBuf)),
			Payload:             bufStr,
			Bind:                pg.BackendWrapper.Bind,
			PasswordMessage:     pg.BackendWrapper.PasswordMessage,
			CancelRequest:       pg.BackendWrapper.CancelRequest,
			Close:               pg.BackendWrapper.Close,
			CopyData:            pg.BackendWrapper.CopyData,
			CopyDone:            pg.BackendWrapper.CopyDone,
			CopyFail:            pg.BackendWrapper.CopyFail,
			Describe:            pg.BackendWrapper.Describe,
			Execute:             pg.BackendWrapper.Execute,
			Flush:               pg.BackendWrapper.Flush,
			FunctionCall:        pg.BackendWrapper.FunctionCall,
			GssEncRequest:       pg.BackendWrapper.GssEncRequest,
			Parse:               pg.BackendWrapper.Parse,
			Query:               pg.BackendWrapper.Query,
			SSlRequest:          pg.BackendWrapper.SSlRequest,
			StartupMessage:      pg.BackendWrapper.StartupMessage,
			SASLInitialResponse: pg.BackendWrapper.SASLInitialResponse,
			SASLResponse:        pg.BackendWrapper.SASLResponse,
			Sync:                pg.BackendWrapper.Sync,
			Terminate:           pg.BackendWrapper.Terminate,
			MsgType:             pg.BackendWrapper.MsgType,
			AuthType:            pg.BackendWrapper.AuthType,
		})

		logger.Debug("Before for loop pg request starts", zap.Any("pgReqs", len(pgRequests)))
	}

	_, err = destConn.Write(reqBuf)
	if err != nil {
		utils.LogError(logger, err, "failed to write request message to the destination server")
		return err
	}
	var pgResponses []models.Frontend

	clientBuffChan := make(chan []byte)
	destBuffChan := make(chan []byte)
	errChan := make(chan error, 1)

	//get the error group from the context
	g := ctx.Value(models.ErrGroupKey).(*errgroup.Group)

	// read requests from client
	g.Go(func() error {
		defer pUtil.Recover(logger, clientConn, destConn)
		defer close(clientBuffChan)
		pUtil.ReadBuffConn(ctx, logger, clientConn, clientBuffChan, errChan)
		return nil
	})

	// read responses from destination
	g.Go(func() error {
		defer pUtil.Recover(logger, clientConn, destConn)
		defer close(destBuffChan)
		pUtil.ReadBuffConn(ctx, logger, destConn, destBuffChan, errChan)
		return nil
	})

	go func() {
		defer pUtil.Recover(logger, clientConn, destConn)
		err := g.Wait()
		if err != nil {
			logger.Info("error group is returning an error", zap.Error(err))
		}
		close(errChan)
	}()

	prevChunkWasReq := false
	logger.Debug("the iteration for the pg request starts", zap.Any("pgReqs", len(pgRequests)), zap.Any("pgResps", len(pgResponses)))

	reqTimestampMock := time.Now()
	var resTimestampMock time.Time

	for {
		select {
		case <-ctx.Done():
			if !prevChunkWasReq && len(pgRequests) > 0 && len(pgResponses) > 0 {
				metadata := make(map[string]string)
				metadata["type"] = "config"
				// Save the mock
				mocks <- &models.Mock{
					Version: models.GetVersion(),
					Name:    "mocks",
					Kind:    models.Postgres,
					Spec: models.MockSpec{
						PostgresRequests:  pgRequests,
						PostgresResponses: pgResponses,
						ReqTimestampMock:  reqTimestampMock,
						ResTimestampMock:  resTimestampMock,
						Metadata:          metadata,
					},
					ConnectionID: ctx.Value(models.ClientConnectionIDKey).(string),
				}
				return ctx.Err()
			}
		case buffer := <-clientBuffChan:

			// Write the request message to the destination
			_, err := destConn.Write(buffer)
			if err != nil {
				utils.LogError(logger, err, "failed to write request message to the destination server")
				return err
			}

			logger.Debug("the iteration for the pg request ends with no of pgReqs:" + strconv.Itoa(len(pgRequests)) + " and pgResps: " + strconv.Itoa(len(pgResponses)))
			if !prevChunkWasReq && len(pgRequests) > 0 && len(pgResponses) > 0 {
				metadata := make(map[string]string)
				metadata["type"] = "config"
				// Save the mock
				mocks <- &models.Mock{
					Version: models.GetVersion(),
					Name:    "mocks",
					Kind:    models.Postgres,
					Spec: models.MockSpec{
						PostgresRequests:  pgRequests,
						PostgresResponses: pgResponses,
						ReqTimestampMock:  reqTimestampMock,
						ResTimestampMock:  resTimestampMock,
						Metadata:          metadata,
					},
					ConnectionID: ctx.Value(models.ClientConnectionIDKey).(string),
				}
				pgRequests = []models.Backend{}
				pgResponses = []models.Frontend{}
			}

			bufStr := util.EncodeBase64(buffer)
			if bufStr != "" {
				pg := NewBackend()
				var msg pgproto3.FrontendMessage

				if !isStartupPacket(buffer) && len(buffer) > 5 {
					bufferCopy := buffer
					for i := 0; i < len(bufferCopy)-5; {
						logger.Debug("Inside the Pg request for loop")
						pg.BackendWrapper.BodyLen = int(binary.BigEndian.Uint32(buffer[i+1:])) - 4
						if len(buffer) < (i + pg.BackendWrapper.BodyLen + 5) {
							utils.LogError(logger, nil, "failed to translate the postgres request message due to shorter network packet buffer. Length of buffer is "+fmt.Sprint(len(buffer))+" buffer value :"+string(buffer)+" and pg.BackendWrapper.BodyLen is "+fmt.Sprint(pg.BackendWrapper.BodyLen))
							break
						}
						pg.BackendWrapper.MsgType = buffer[i]

						msg, err = pg.translateToReadableBackend(buffer[i:(i + pg.BackendWrapper.BodyLen + 5)])
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

					pgMock := &models.Backend{
						PacketTypes: pg.BackendWrapper.PacketTypes,
						Identfier:   "ClientRequest",
						Length:      uint32(len(reqBuf)),
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
					afterEncoded, err := postgresDecoderBackend(*pgMock)
					if err != nil {
						logger.Debug("failed to decode the response message in proxy for postgres dependency", zap.Error(err))
					}

					if len(afterEncoded) != len(buffer) && len(pgMock.PacketTypes) > 0 && pgMock.PacketTypes[0] != "p" {
						logger.Debug("the length of the encoded buffer is not equal to the length of the original buffer", zap.Any("after_encoded", len(afterEncoded)), zap.Any("buffer", len(buffer)))
						pgMock.Payload = bufStr
					}
					pgRequests = append(pgRequests, *pgMock)

				}

				if isStartupPacket(buffer) {
					pgMock := &models.Backend{
						Identfier: "StartupRequest",
						Payload:   bufStr,
					}
					pgRequests = append(pgRequests, *pgMock)
				}
			}
			prevChunkWasReq = true
		case buffer := <-destBuffChan:
			if prevChunkWasReq {
				// store the request timestamp
				reqTimestampMock = time.Now()
			}

			// Write the response message to the client
			_, err := clientConn.Write(buffer)
			if err != nil {
				utils.LogError(logger, err, "failed to write response message to the client")
				return err
			}

			bufStr := util.EncodeBase64(buffer)

			if bufStr != "" {
				pg := NewFrontend()
				if !isStartupPacket(buffer) && len(buffer) > 5 && bufStr != "Tg==" {
					bufferCopy := buffer

					//Saving list of packets in case of multiple packets in a single buffer steam
					ps := make([]pgproto3.ParameterStatus, 0)
					var dataRows []pgproto3.DataRow

					for i := 0; i < len(bufferCopy)-5; {
						pg.FrontendWrapper.MsgType = buffer[i]
						pg.FrontendWrapper.BodyLen = int(binary.BigEndian.Uint32(buffer[i+1:])) - 4
						msg, err := pg.translateToReadableResponse(logger, buffer[i:(i+pg.FrontendWrapper.BodyLen+5)])
						if err != nil {
							utils.LogError(logger, err, "failed to translate the response message to readable")
							break
						}

						pg.FrontendWrapper.PacketTypes = append(pg.FrontendWrapper.PacketTypes, string(pg.FrontendWrapper.MsgType))
						i += 5 + pg.FrontendWrapper.BodyLen

						if pg.FrontendWrapper.ParameterStatus.Name != "" {
							ps = append(ps, pg.FrontendWrapper.ParameterStatus)
						}
						if pg.FrontendWrapper.MsgType == 'C' {
							pg.FrontendWrapper.CommandComplete = *msg.(*pgproto3.CommandComplete)
							// empty the command tag
							pg.FrontendWrapper.CommandComplete.CommandTag = []byte{}
							pg.FrontendWrapper.CommandCompletes = append(pg.FrontendWrapper.CommandCompletes, pg.FrontendWrapper.CommandComplete)
						}
						if pg.FrontendWrapper.MsgType == 'D' && pg.FrontendWrapper.DataRow.RowValues != nil {
							// Create a new slice for each DataRow
							valuesCopy := make([]string, len(pg.FrontendWrapper.DataRow.RowValues))
							copy(valuesCopy, pg.FrontendWrapper.DataRow.RowValues)

							row := pgproto3.DataRow{
								RowValues: valuesCopy, // Use the copy of the values
								Values:    pg.FrontendWrapper.DataRow.Values,
							}
							dataRows = append(dataRows, row)
						}
					}

					if len(ps) > 0 {
						pg.FrontendWrapper.ParameterStatusCombined = ps
					}
					if len(dataRows) > 0 {
						pg.FrontendWrapper.DataRows = dataRows
					}

					// from here take the msg and append its readable form to the pgResponses
					pgMock := &models.Frontend{
						PacketTypes: pg.FrontendWrapper.PacketTypes,
						Identfier:   "ServerResponse",
						Length:      uint32(len(reqBuf)),
						// Payload:                         bufStr,
						AuthenticationOk:                pg.FrontendWrapper.AuthenticationOk,
						AuthenticationCleartextPassword: pg.FrontendWrapper.AuthenticationCleartextPassword,
						AuthenticationMD5Password:       pg.FrontendWrapper.AuthenticationMD5Password,
						AuthenticationGSS:               pg.FrontendWrapper.AuthenticationGSS,
						AuthenticationGSSContinue:       pg.FrontendWrapper.AuthenticationGSSContinue,
						AuthenticationSASL:              pg.FrontendWrapper.AuthenticationSASL,
						AuthenticationSASLContinue:      pg.FrontendWrapper.AuthenticationSASLContinue,
						AuthenticationSASLFinal:         pg.FrontendWrapper.AuthenticationSASLFinal,
						BackendKeyData:                  pg.FrontendWrapper.BackendKeyData,
						BindComplete:                    pg.FrontendWrapper.BindComplete,
						CloseComplete:                   pg.FrontendWrapper.CloseComplete,
						CommandComplete:                 pg.FrontendWrapper.CommandComplete,
						CommandCompletes:                pg.FrontendWrapper.CommandCompletes,
						CopyData:                        pg.FrontendWrapper.CopyData,
						CopyDone:                        pg.FrontendWrapper.CopyDone,
						CopyInResponse:                  pg.FrontendWrapper.CopyInResponse,
						CopyOutResponse:                 pg.FrontendWrapper.CopyOutResponse,
						DataRow:                         pg.FrontendWrapper.DataRow,
						DataRows:                        pg.FrontendWrapper.DataRows,
						EmptyQueryResponse:              pg.FrontendWrapper.EmptyQueryResponse,
						ErrorResponse:                   pg.FrontendWrapper.ErrorResponse,
						FunctionCallResponse:            pg.FrontendWrapper.FunctionCallResponse,
						NoData:                          pg.FrontendWrapper.NoData,
						NoticeResponse:                  pg.FrontendWrapper.NoticeResponse,
						NotificationResponse:            pg.FrontendWrapper.NotificationResponse,
						ParameterDescription:            pg.FrontendWrapper.ParameterDescription,
						ParameterStatusCombined:         pg.FrontendWrapper.ParameterStatusCombined,
						ParseComplete:                   pg.FrontendWrapper.ParseComplete,
						PortalSuspended:                 pg.FrontendWrapper.PortalSuspended,
						ReadyForQuery:                   pg.FrontendWrapper.ReadyForQuery,
						RowDescription:                  pg.FrontendWrapper.RowDescription,
						MsgType:                         pg.FrontendWrapper.MsgType,
						AuthType:                        pg.FrontendWrapper.AuthType,
					}

					afterEncoded, err := postgresDecoderFrontend(*pgMock)
					if err != nil {
						logger.Debug("failed to decode the response message in proxy for postgres dependency", zap.Error(err))
					}
					if len(afterEncoded) != len(buffer) && len(pgMock.PacketTypes) > 0 && pgMock.PacketTypes[0] != "R" {
						logger.Debug("the length of the encoded buffer is not equal to the length of the original buffer", zap.Any("after_encoded", len(afterEncoded)), zap.Any("buffer", len(buffer)))
						pgMock.Payload = bufStr
					}
					pgResponses = append(pgResponses, *pgMock)
				}

				if bufStr == "Tg==" || len(buffer) <= 5 {

					pgMock := &models.Frontend{
						Payload: bufStr,
					}
					pgResponses = append(pgResponses, *pgMock)
				}
			}

			resTimestampMock = time.Now()

			logger.Debug("the iteration for the postgres response ends with no of postgresReqs:" + strconv.Itoa(len(pgRequests)) + " and pgResps: " + strconv.Itoa(len(pgResponses)))
			prevChunkWasReq = false
		case err := <-errChan:
			if err == io.EOF {
				return nil
			}
			return err
		}
	}
}
