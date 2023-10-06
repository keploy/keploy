package postgresparser

import (
	"context"
	"io"
	"net"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	// "time"

	// "time"
	// "sync"
	// "strings"

	"encoding/binary"
	// "encoding/json"
	"encoding/base64"
	// "fmt"
	// "github.com/jackc/pgproto3"
	sentry "github.com/getsentry/sentry-go"
	"go.keploy.io/server/pkg"
	"go.keploy.io/server/pkg/proxy/util"

	// "bytes"

	"errors"

	"go.keploy.io/server/pkg/hooks"
	"go.keploy.io/server/pkg/models"
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
		decodePostgresOutgoing(requestBuffer, clientConn, destConn, h, logger)
	default:
		logger.Info("Invalid mode detected while intercepting outgoing http call", zap.Any("mode", models.GetMode()))
	}

}

type PSQLMessage struct {
	// Define fields to store the relevant information from the buffer
	ID      uint32
	Payload []byte
	Field1  string
	Field2  int
	// Add more fields as needed
}

// This is the encoding function for the streaming postgres wiremessage
func encodePostgresOutgoing(requestBuffer []byte, clientConn, destConn net.Conn, h *hooks.Hook, logger *zap.Logger, ctx context.Context) error {

	pgRequests := []models.GenericPayload{}
	bufStr := base64.StdEncoding.EncodeToString(requestBuffer)

	if bufStr != "" {

		pgRequests = append(pgRequests, models.GenericPayload{
			Origin: models.FromClient,
			Message: []models.OutputBinary{
				{
					Type: "binary",
					Data: bufStr,
				},
			},
		})
	}
	_, err := destConn.Write(requestBuffer)
	if err != nil {
		logger.Error("failed to write request message to the destination server", zap.Error(err))
		return err
	}
	pgResponses := []models.GenericPayload{}

	clientBufferChannel := make(chan []byte)
	destBufferChannel := make(chan []byte)
	errChannel := make(chan error)
	// read requests from client
	go func() {
		// Recover from panic and gracefully shutdown
		defer h.Recover(pkg.GenerateRandomID())
		defer sentry.Recover()
		ReadBuffConn(clientConn, clientBufferChannel, errChannel, logger)
	}()
	// read response from destination
	go func() {
		// Recover from panic and gracefully shutdown
		defer h.Recover(pkg.GenerateRandomID())
		defer sentry.Recover()
		ReadBuffConn(destConn, destBufferChannel, errChannel, logger)
	}()

	isPreviousChunkRequest := false
	logger.Debug("the iteration for the pg request starts", zap.Any("pgReqs", len(pgRequests)), zap.Any("pgResps", len(pgResponses)))
	for {

		// start := time.NewTicker(1*time.Second)
		sigChan := make(chan os.Signal, 1)
		signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)
		select {
		// case <-start.C:
		case <-sigChan:
			if !isPreviousChunkRequest && len(pgRequests) > 0 && len(pgResponses) > 0 {
				h.AppendMocks(&models.Mock{
					Version: models.V1Beta2,
					Name:    "mocks",
					Kind:    models.Postgres,
					Spec: models.MockSpec{
						PostgresRequests:  pgRequests,
						PostgresResponses: pgResponses,
					},
				}, ctx)
				pgRequests = []models.GenericPayload{}
				pgResponses = []models.GenericPayload{}
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
					},
				}, ctx)
				pgRequests = []models.GenericPayload{}
				pgResponses = []models.GenericPayload{}
			}

			bufStr := base64.StdEncoding.EncodeToString(buffer)
			// }
			if bufStr != "" {

				pgRequests = append(pgRequests, models.GenericPayload{
					Origin: models.FromClient,
					Message: []models.OutputBinary{
						{
							Type: "binary",
							Data: bufStr,
						},
					},
				})
			}

			isPreviousChunkRequest = true
		case buffer := <-destBufferChannel:
			// Write the response message to the client
			_, err := clientConn.Write(buffer)
			if err != nil {
				logger.Error("failed to write response to the client", zap.Error(err))
				return err
			}

			bufStr := base64.StdEncoding.EncodeToString(buffer)
			// }
			if bufStr != "" {

				pgResponses = append(pgResponses, models.GenericPayload{
					Origin: models.FromServer,
					Message: []models.OutputBinary{
						{
							Type: "binary",
							Data: bufStr,
						},
					},
				})
			}

			logger.Debug("the iteration for the postgres response ends with no of postgresReqs:" + strconv.Itoa(len(pgRequests)) + " and pgResps: " + strconv.Itoa(len(pgResponses)))
			isPreviousChunkRequest = false
		case err := <-errChannel:
			return err
		}

	}
}

// This is the decoding function for the postgres wiremessage
func decodePostgresOutgoing(requestBuffer []byte, clientConn, destConn net.Conn, h *hooks.Hook, logger *zap.Logger) error {
	pgRequests := [][]byte{requestBuffer}
	tcsMocks := h.GetTcsMocks()
	// change auth to md5 instead of scram
	ChangeAuthToMD5(tcsMocks, h, logger)

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
				// errChannel <- err
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
		// bestMatchedIndx := 0
		// fuzzy match gives the index for the best matched pg mock

		matched, pgResponses := matchingPg(tcsMocks, pgRequests, h)

		if !matched {
			logger.Error("failed to match the dependency call from user application", zap.Any("request packets", len(pgRequests)))
			return errors.New("failed to match the dependency call from user application")
			// continue
		}
		for _, pgResponse := range pgResponses {
			encoded, _ := PostgresDecoder(pgResponse.Message[0].Data)

			_, err := clientConn.Write([]byte(encoded))
			if err != nil {
				logger.Error("failed to write request message to the client application", zap.Error(err))
				// errChannel <- err
				return err
			}
		}
		// }

		// update for the next dependency call

		pgRequests = [][]byte{}

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
