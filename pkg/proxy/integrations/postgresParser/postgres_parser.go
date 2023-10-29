package postgresparser

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/jackc/pgproto3/v2"
	"go.keploy.io/server/pkg"
	"go.keploy.io/server/pkg/proxy/util"

	"go.keploy.io/server/pkg/hooks"
	"go.keploy.io/server/pkg/models"

	"go.keploy.io/server/utils"
	"go.uber.org/zap"
)

var Emoji = "\U0001F430" + " Keploy:"

func IsOutgoingPSQL(buffer []byte) bool {
	const ProtocolVersion = 0x00030000 // Protocol version 3.0

	if len(buffer) < 8 {
		// Not enough data for a complete header
		return false
	}

	// The first four bytes are the message length, but we don't need to check those

	// The next four bytes are the protocol version
	version := binary.BigEndian.Uint32(buffer[4:8])

	if version == 80877103 {
		return true
	}
	return version == ProtocolVersion
}

func ProcessOutgoingPSQL(requestBuffer []byte, clientConn, destConn net.Conn, h *hooks.Hook, logger *zap.Logger, ctx context.Context) {
	switch models.GetMode() {
	case models.MODE_RECORD:
		encodePostgresOutgoing(requestBuffer, clientConn, destConn, h, logger, ctx)
	case models.MODE_TEST:
		decodePostgresOutgoing(requestBuffer, clientConn, destConn, h, logger, ctx)
	default:
		logger.Info("Invalid mode detected while intercepting outgoing http call", zap.Any("mode", models.GetMode()))
	}

}

// This is the encoding function for the streaming postgres wiremessage

func encodePostgresOutgoing(requestBuffer []byte, clientConn, destConn net.Conn, h *hooks.Hook, logger *zap.Logger, ctx context.Context) error {
	logger.Debug("Inside the encodePostgresOutgoing function")
	pgRequests := []models.Backend{}

	bufStr := base64.StdEncoding.EncodeToString(requestBuffer)
	logger.Debug("bufStr is ", zap.String("bufStr", bufStr))
	pg := NewBackend()
	_, err := pg.DecodeStartupMessage(requestBuffer)
	if err != nil {
		logger.Error("failed to decode startup message server", zap.Error(err))
	}

	if bufStr != "" {
		pgRequests = append(pgRequests, models.Backend{
			PacketTypes:         pg.BackendWrapper.PacketTypes,
			Identfier:           "StartupRequest",
			Length:              uint32(len(requestBuffer)),
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

	_, err = destConn.Write(requestBuffer)
	if err != nil {
		logger.Error("failed to write request message to the destination server", zap.Error(err))
		return err
	}
	pgResponses := []models.Frontend{}

	clientBufferChannel := make(chan []byte)
	destBufferChannel := make(chan []byte)
	errChannel := make(chan error)
	// read requests from client
	go func() {
		// Recover from panic and gracefully shutdown
		defer h.Recover(pkg.GenerateRandomID())

		defer utils.HandlePanic()

		ReadBuffConn(clientConn, clientBufferChannel, errChannel, logger)
	}()
	// read response from destination
	go func() {
		// Recover from panic and gracefully shutdown
		defer h.Recover(pkg.GenerateRandomID())

		defer utils.HandlePanic()

		ReadBuffConn(destConn, destBufferChannel, errChannel, logger)
	}()

	isPreviousChunkRequest := false
	logger.Debug("the iteration for the pg request starts", zap.Any("pgReqs", len(pgRequests)), zap.Any("pgResps", len(pgResponses)))

	reqTimestampMock := time.Now()
	var resTimestampMock time.Time

	for {

		sigChan := make(chan os.Signal, 1)
		signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)
		select {
		case <-sigChan:
			if !isPreviousChunkRequest && len(pgRequests) > 0 && len(pgResponses) > 0 {
				h.AppendMocks(&models.Mock{
					Version: models.V1Beta2,
					Name:    "mocks",
					Kind:    models.Postgres,
					Spec: models.MockSpec{
						PostgresRequests:  pgRequests,
						PostgresResponses: pgResponses,
						ReqTimestampMock:  reqTimestampMock,
						ResTimestampMock:  resTimestampMock,
					},
				}, ctx)
				pgRequests = []models.Backend{}
				pgResponses = []models.Frontend{}

				clientConn.Close()
				destConn.Close()
				return nil
			}
		case buffer := <-clientBufferChannel:

			// Write the request message to the destination
			_, err := destConn.Write(buffer)
			if err != nil {
				logger.Error("failed to write request message to the destination server", zap.Error(err))
				return err
			}

			logger.Debug("the iteration for the pg request ends with no of pgReqs:" + strconv.Itoa(len(pgRequests)) + " and pgResps: " + strconv.Itoa(len(pgResponses)))
			if !isPreviousChunkRequest && len(pgRequests) > 0 && len(pgResponses) > 0 {
				h.AppendMocks(&models.Mock{
					Version: models.V1Beta2,
					Name:    "mocks",
					Kind:    models.Postgres,
					Spec: models.MockSpec{
						PostgresRequests:  pgRequests,
						PostgresResponses: pgResponses,
						ReqTimestampMock:  reqTimestampMock,
						ResTimestampMock:  resTimestampMock,
					},
				}, ctx)
				pgRequests = []models.Backend{}
				pgResponses = []models.Frontend{}
			}

			bufStr := base64.StdEncoding.EncodeToString(buffer)
			if bufStr != "" {

				pg := NewBackend()
				var msg pgproto3.FrontendMessage

				if !isStartupPacket(buffer) && len(buffer) > 5 {
					bufferCopy := buffer
					for i := 0; i < len(bufferCopy); {
						logger.Debug("Inside the if condition")
						pg.BackendWrapper.MsgType = buffer[i]
						pg.BackendWrapper.BodyLen = int(binary.BigEndian.Uint32(buffer[i+1:])) - 4
						msg, err = pg.TranslateToReadableBackend(buffer[i:(i + pg.BackendWrapper.BodyLen + 5)])
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

						i += (5 + pg.BackendWrapper.BodyLen)
					}

					pg_mock := &models.Backend{
						PacketTypes:         pg.BackendWrapper.PacketTypes,
						Identfier:           "ClientRequest",
						Length:              uint32(len(requestBuffer)),
						Payload:             bufStr,
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

					after_encoded, _ := PostgresDecoderBackend(*pg_mock)
					if len(after_encoded) != len(buffer) {
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
			isPreviousChunkRequest = true
		case buffer := <-destBufferChannel:
			if isPreviousChunkRequest {
				// store the request timestamp
				reqTimestampMock = time.Now()
			}

			// Write the response message to the client
			_, err := clientConn.Write(buffer)
			if err != nil {
				logger.Error("failed to write response to the client", zap.Error(err))
				return err
			}

			bufStr := base64.StdEncoding.EncodeToString(buffer)

			if bufStr != "" {
				pg := NewFrontend()
				if !isStartupPacket(buffer) && len(buffer) > 5 && bufStr != "Tg==" {
					bufferCopy := buffer
					// fmt.Println("bufferCopy is ", bufferCopy)
					ps := make([]pgproto3.ParameterStatus, 0)
					dataRows := []pgproto3.DataRow{}

					for i := 0; i < len(bufferCopy); {
						pg.FrontendWrapper.MsgType = buffer[i]
						pg.FrontendWrapper.BodyLen = int(binary.BigEndian.Uint32(buffer[i+1:])) - 4
						msg, err := pg.TranslateToReadableResponse(buffer[i:(i+pg.FrontendWrapper.BodyLen+5)], logger) // arre yeh index leta hai length nhi
						if err != nil {
							logger.Error("failed to translate the response message to readable", zap.Error(err))
						}

						pg.FrontendWrapper.PacketTypes = append(pg.FrontendWrapper.PacketTypes, string(pg.FrontendWrapper.MsgType))
						i += (5 + pg.FrontendWrapper.BodyLen)
						if pg.FrontendWrapper.ParameterStatus.Name != "" {
							ps = append(ps, pg.FrontendWrapper.ParameterStatus)
						}
						if pg.FrontendWrapper.MsgType == 'C' {
							pg.FrontendWrapper.CommandComplete = *msg.(*pgproto3.CommandComplete)
							pg.FrontendWrapper.CommandCompletes = append(pg.FrontendWrapper.CommandCompletes, pg.FrontendWrapper.CommandComplete)
						}
						if pg.FrontendWrapper.DataRow.Values != nil {
							// Create a new slice for each DataRow
							valuesCopy := make([][]byte, len(pg.FrontendWrapper.DataRow.Values))
							copy(valuesCopy, pg.FrontendWrapper.DataRow.Values)

							row := pgproto3.DataRow{
								Values: valuesCopy, // Use the copy of the values
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

					// from here take the msg and append its readabable form to the pgResponses
					pg_mock := &models.Frontend{
						PacketTypes:                     pg.FrontendWrapper.PacketTypes,
						Identfier:                       "ServerResponse",
						Length:                          uint32(len(requestBuffer)),
						Payload:                         bufStr,
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
			isPreviousChunkRequest = false
		case err := <-errChannel:
			return err
		}

	}
}

func ReadBuffConn(conn net.Conn, bufferChannel chan []byte, errChannel chan error, logger *zap.Logger) error {
	for {
		buffer, err := util.ReadBytes(conn)
		if err != nil {
			if err == io.EOF {
				logger.Debug("EOF error received from client. Closing connection in postgres !!")
				return err
			}
			if !strings.Contains(err.Error(), "use of closed network connection") {
				logger.Error("failed to read the packet message in proxy for pg dependency", zap.Error(err))
			}
			errChannel <- err
			return err
		}
		bufferChannel <- buffer
	}
}

// This is the decoding function for the postgres wiremessage
func decodePostgresOutgoing(requestBuffer []byte, clientConn, destConn net.Conn, h *hooks.Hook, logger *zap.Logger, ctx context.Context) error {
	pgRequests := [][]byte{requestBuffer}

	for {
		// Since protocol packets have to be parsed for checking stream end,
		// clientConnection have deadline for read to determine the end of stream.
		err := clientConn.SetReadDeadline(time.Now().Add(10 * time.Millisecond))
		if err != nil {
			logger.Error(hooks.Emoji+"failed to set the read deadline for the pg client connection", zap.Error(err))
			return err
		}

		for {
			buffer, err := util.ReadBytes(clientConn)
			if netErr, ok := err.(net.Error); !(ok && netErr.Timeout()) && err != nil {
				if err == io.EOF {
					logger.Debug("EOF error received from client. Closing connection in postgres !!")
					return err
				}
				logger.Error("failed to read the request message in proxy for postgres dependency", zap.Error(err))
				return err
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

		tcsMocks := h.GetTcsMocks()
		// change auth to md5 instead of scram
		// CheckValidEncode(tcsMocks, h, logger)
		ChangeAuthToMD5(tcsMocks, h, logger)

		matched, pgResponses := matchingReadablePG(tcsMocks, pgRequests, h)

		if !matched {
			logger.Error("failed to match the dependency call from user application", zap.Any("request packets", len(pgRequests)))
			return errors.New("failed to match the dependency call from user application")
		}

		for _, pgResponse := range pgResponses {
			encoded, err := PostgresDecoder(pgResponse.Payload)
			// if len(pgResponse.PacketTypes) > 0 {
			// 	encoded, err = PostgresDecoderFrontend(pgResponse)
			// }

			if err != nil {
				logger.Error("failed to decode the response message in proxy for postgres dependency", zap.Error(err))
				return err
			}

			_, err = clientConn.Write([]byte(encoded))
			if err != nil {
				logger.Error("failed to write request message to the client application", zap.Error(err))
				return err
			}
		}
		// update for the next dependency call
		pgRequests = [][]byte{}
	}

}
