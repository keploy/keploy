package v1

import (
	"context"
	"encoding/binary"
	"github.com/jackc/pgproto3/v2"
	"go.keploy.io/server/v2/pkg/core/proxy/integrations/util"
	pUtil "go.keploy.io/server/v2/pkg/core/proxy/util"
	"go.keploy.io/server/v2/pkg/models"
	"go.keploy.io/server/v2/utils"
	"go.uber.org/zap"
	"net"
	"strconv"
	"time"
)

func encodePostgres(ctx context.Context, logger *zap.Logger, reqBuf []byte, clientConn, destConn net.Conn, mocks chan<- *models.Mock, opts models.OutgoingOptions) error {
	//closing the destination conn
	defer func(destConn net.Conn) {
		err := destConn.Close()
		if err != nil {
			logger.Error("failed to close the destination connection", zap.Error(err))
		}
	}(destConn)

	logger.Debug("Inside the encodePostgresOutgoing function")
	var pgRequests []models.Backend

	bufStr := util.EncodeBase64(reqBuf)
	logger.Debug("bufStr is ", zap.String("bufStr", bufStr))

	pg := NewBackend()
	_, err := pg.decodeStartupMessage(reqBuf)
	if err != nil {
		logger.Error("failed to decode startup message server", zap.Error(err))
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
		logger.Error("failed to write request message to the destination server", zap.Error(err))
		return err
	}
	var pgResponses []models.Frontend

	clientBuffChan := make(chan []byte)
	destBuffChan := make(chan []byte)
	errChan := make(chan error)

	// read requests from client
	go func() {
		defer utils.Recover(logger)
		pUtil.ReadBuffConn(ctx, logger, clientConn, clientBuffChan, errChan)
	}()
	// read responses from destination
	go func() {
		defer utils.Recover(logger)
		pUtil.ReadBuffConn(ctx, logger, destConn, destBuffChan, errChan)
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
				}

				pgRequests = []models.Backend{}
				pgResponses = []models.Frontend{}
				return nil
			}
		case buffer := <-clientBuffChan:

			// Write the request message to the destination
			_, err := destConn.Write(buffer)
			if err != nil {
				logger.Error("failed to write request message to the destination server", zap.Error(err))
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
						logger.Debug("Inside the if condition")
						pg.BackendWrapper.MsgType = buffer[i]
						pg.BackendWrapper.BodyLen = int(binary.BigEndian.Uint32(buffer[i+1:])) - 4
						if len(buffer) < (i + pg.BackendWrapper.BodyLen + 5) {
							logger.Error("failed to translate the postgres request message due to shorter network packet buffer")
							break
						}
						msg, err = pg.translateToReadableBackend(buffer[i:(i + pg.BackendWrapper.BodyLen + 5)])
						if err != nil && buffer[i] != 112 {
							logger.Error("failed to translate the request message to readable", zap.Error(err))
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
					after_encoded, err := postgresDecoderBackend(*pg_mock)
					if err != nil {
						logger.Debug("failed to decode the response message in proxy for postgres dependency", zap.Error(err))
					}

					if len(after_encoded) != len(buffer) && pg_mock.PacketTypes[0] != "p" {
						logger.Debug("the length of the encoded buffer is not equal to the length of the original buffer", zap.Any("after_encoded", len(after_encoded)), zap.Any("buffer", len(buffer)))
						pg_mock.Payload = bufStr
					}
					pgRequests = append(pgRequests, *pg_mock)

				}

				if isStartupPacket(buffer) {
					pg_mock := &models.Backend{
						Identfier: "StartupRequest",
						Payload:   bufStr,
					}
					pgRequests = append(pgRequests, *pg_mock)
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
				logger.Error("failed to write response to the client", zap.Error(err))
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
							logger.Error("failed to translate the response message to readable", zap.Error(err))
							break
						}

						pg.FrontendWrapper.PacketTypes = append(pg.FrontendWrapper.PacketTypes, string(pg.FrontendWrapper.MsgType))
						i += 5 + pg.FrontendWrapper.BodyLen

						if pg.FrontendWrapper.ParameterStatus.Name != "" {
							ps = append(ps, pg.FrontendWrapper.ParameterStatus)
						}
						if pg.FrontendWrapper.MsgType == 'C' {
							pg.FrontendWrapper.CommandComplete = *msg.(*pgproto3.CommandComplete)
							pg.FrontendWrapper.CommandCompletes = append(pg.FrontendWrapper.CommandCompletes, pg.FrontendWrapper.CommandComplete)
						}
						if pg.FrontendWrapper.DataRow.RowValues != nil {
							// Create a new slice for each DataRow
							valuesCopy := make([]string, len(pg.FrontendWrapper.DataRow.RowValues))
							copy(valuesCopy, pg.FrontendWrapper.DataRow.RowValues)

							row := pgproto3.DataRow{
								RowValues: valuesCopy, // Use the copy of the values
							}
							// fmt.Println("row is ", row)
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
					pg_mock := &models.Frontend{
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

					after_encoded, err := postgresDecoderFrontend(*pg_mock)
					if err != nil {
						logger.Debug("failed to decode the response message in proxy for postgres dependency", zap.Error(err))
					}
					if (len(after_encoded) != len(buffer) && pg_mock.PacketTypes[0] != "R") || len(pg_mock.DataRows) > 0 {
						logger.Debug("the length of the encoded buffer is not equal to the length of the original buffer", zap.Any("after_encoded", len(after_encoded)), zap.Any("buffer", len(buffer)))
						pg_mock.Payload = bufStr
					}
					pgResponses = append(pgResponses, *pg_mock)
				}

				if bufStr == "Tg==" || len(buffer) <= 5 {

					pg_mock := &models.Frontend{
						Payload: bufStr,
					}
					pgResponses = append(pgResponses, *pg_mock)
				}
			}

			resTimestampMock = time.Now()

			logger.Debug("the iteration for the postgres response ends with no of postgresReqs:" + strconv.Itoa(len(pgRequests)) + " and pgResps: " + strconv.Itoa(len(pgResponses)))
			prevChunkWasReq = false
		case err := <-errChan:
			return err
		}
	}
}
