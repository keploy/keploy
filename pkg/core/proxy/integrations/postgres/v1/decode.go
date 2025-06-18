//go:build linux

// Package v1 provides functionality for decoding Postgres requests and responses.
package v1

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"time"

	"go.keploy.io/server/v2/pkg/core/proxy/integrations"
	"go.keploy.io/server/v2/pkg/core/proxy/integrations/util"
	pUtil "go.keploy.io/server/v2/pkg/core/proxy/util"
	"go.keploy.io/server/v2/pkg/models"
	"go.keploy.io/server/v2/utils"
	"go.uber.org/zap"
)

// min returns the minimum of two integers
func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// isSSLPacket checks if the buffer appears to be an SSL/TLS handshake packet
func isSSLPacket(buffer []byte) bool {
	if len(buffer) < 5 {
		return false
	}

	// SSL/TLS handshake typically starts with 0x16 (handshake record type)
	// followed by version bytes and length
	if buffer[0] == 0x16 {
		// Check for common TLS versions
		if buffer[1] == 0x03 && (buffer[2] == 0x01 || buffer[2] == 0x02 || buffer[2] == 0x03 || buffer[2] == 0x04) {
			return true
		}
	}

	// Note: We don't check for Postgres SSL request (80877103) here because that's a valid Postgres message
	// The SSL request is a special Postgres message that initiates SSL/TLS, but it's still part of the Postgres protocol

	return false
}

// isValidPostgresPacket performs basic validation of postgres packet structure
func isValidPostgresPacket(buffer []byte) (bool, error) {
	if len(buffer) < 8 {
		return false, fmt.Errorf("packet too short: %d bytes, minimum required: 8", len(buffer))
	}

	// Check if it might be an SSL packet first
	if isSSLPacket(buffer) {
		return false, fmt.Errorf("detected SSL/TLS encrypted packet")
	}

	// For startup messages, check protocol version
	if len(buffer) >= 8 {
		version := binary.BigEndian.Uint32(buffer[4:8])
		switch version {
		case 0x00030000: // Protocol version 3.0
			return true, nil
		case 80877103: // SSL request (this is a valid Postgres message)
			return true, nil
		case 80877102: // Cancel request
			return true, nil
		case 80877104: // GSS encryption request
			return true, nil
		}
	}

	// For regular messages (not startup), check if message type is valid
	if len(buffer) > 5 && buffer[0] != 0 {
		msgType := buffer[0]
		// Check for valid frontend message types
		validTypes := "BCDEFfcdHPpQSX"
		for _, t := range validTypes {
			if msgType == byte(t) {
				return true, nil
			}
		}
		return false, fmt.Errorf("invalid message type: %c (0x%02X)", msgType, msgType)
	}

	return true, nil
}

func decodePostgres(ctx context.Context, logger *zap.Logger, reqBuf []byte, clientConn net.Conn, dstCfg *models.ConditionalDstCfg, mockDb integrations.MockMemDb, _ models.OutgoingOptions) error {
	// Validate the initial request buffer before processing
	if valid, err := isValidPostgresPacket(reqBuf); !valid {
		logger.Warn("Invalid postgres packet detected, falling back to passthrough",
			zap.Error(err),
			zap.Int("buffer_length", len(reqBuf)),
			zap.String("buffer_hex", fmt.Sprintf("%x", reqBuf[:min(len(reqBuf), 32)])))

		// If it's SSL, log appropriately and pass through
		if strings.Contains(err.Error(), "SSL") || strings.Contains(err.Error(), "encrypted") {
			logger.Info("SSL/TLS encrypted connection detected, passing through without parsing")
		}

		// Pass through the request without parsing
		_, passErr := pUtil.PassThrough(ctx, logger, clientConn, dstCfg, [][]byte{reqBuf})
		if passErr != nil {
			utils.LogError(logger, passErr, "failed to pass through invalid postgres packet")
			return passErr
		}
		return nil
	}

	pgRequests := [][]byte{reqBuf}
	errCh := make(chan error, 1)

	go func(errCh chan error, pgRequests [][]byte) {
		defer pUtil.Recover(logger, clientConn, nil)
		// close should be called from the producer of the channel
		defer close(errCh)
		for {
		continueLoop:
			// Since protocol packets have to be parsed for checking stream end,
			// clientConnection have deadline for read to determine the end of stream.
			err := clientConn.SetReadDeadline(time.Now().Add(10 * time.Millisecond))
			if err != nil && err != io.EOF && strings.Contains(err.Error(), "use of closed network connection") {
				utils.LogError(logger, err, "failed to set the read deadline for the pg client conn")
				errCh <- err
			}

			// To read the stream of request packets from the client
			for {
				buffer, err := pUtil.ReadBytes(ctx, logger, clientConn)
				if err != nil {
					// Applied this nolint to ignore the staticcheck error here because of readability
					// nolint:staticcheck
					if netErr, ok := err.(net.Error); !(ok && netErr.Timeout()) {
						if err == io.EOF {
							logger.Debug("EOF error received from client. Closing conn in postgres !!")
							errCh <- err
						}
						logger.Debug("failed to read the request message in proxy for postgres dependency")
						errCh <- err
					}
				}
				if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
					break
				}
				pgRequests = append(pgRequests, buffer)
			}

			if len(pgRequests) == 0 {
				continue
			}

			// Validate each request buffer before processing
			for i, reqBuffer := range pgRequests {
				if valid, validationErr := isValidPostgresPacket(reqBuffer); !valid {
					logger.Warn("Invalid postgres packet in request stream",
						zap.Int("packet_index", i),
						zap.Error(validationErr),
						zap.Int("buffer_length", len(reqBuffer)))

					// If any packet is invalid, pass through all requests
					_, passErr := pUtil.PassThrough(ctx, logger, clientConn, dstCfg, pgRequests)
					if passErr != nil {
						utils.LogError(logger, passErr, "failed to pass through requests with invalid packets")
						errCh <- passErr
					}
					// Clear the buffer and continue
					pgRequests = [][]byte{}
					goto continueLoop
				}
			}

			var mutex sync.Mutex
			matched, pgResponses, err := matchingReadablePG(ctx, logger, &mutex, pgRequests, mockDb)
			if err != nil {
				logger.Error("error while matching postgres mocks",
					zap.Error(err),
					zap.Int("request_count", len(pgRequests)))
				errCh <- fmt.Errorf("error while matching tcs mocks %v", err)
				return
			}

			if !matched {
				logger.Debug("MISMATCHED REQ is" + string(pgRequests[0]))
				_, err = pUtil.PassThrough(ctx, logger, clientConn, dstCfg, pgRequests)
				if err != nil {
					utils.LogError(logger, err, "failed to pass the request", zap.Any("request packets", len(pgRequests)))
					errCh <- err
				}
				continue
			}
			for _, pgResponse := range pgResponses {
				var encoded []byte
				var err error

				// Safely decode the response
				if len(pgResponse.PacketTypes) > 0 && len(pgResponse.Payload) == 0 {
					encoded, err = postgresDecoderFrontend(pgResponse)
					if err != nil {
						logger.Error("failed to decode postgres response using frontend decoder",
							zap.Error(err),
							zap.Strings("packet_types", pgResponse.PacketTypes))
						errCh <- err
						return
					}
				} else {
					encoded, err = util.DecodeBase64(pgResponse.Payload)
					if err != nil {
						logger.Error("failed to decode base64 response payload",
							zap.Error(err),
							zap.Int("payload_length", len(pgResponse.Payload)))
						errCh <- err
						return
					}
				}

				_, err = clientConn.Write(encoded)
				if err != nil && err != io.EOF && !strings.Contains(err.Error(), "use of closed network connection") {
					logger.Error("failed to write response message to client",
						zap.Error(err),
						zap.Int("encoded_length", len(encoded)))
					errCh <- err
					return
				}
			}
			// Clear the buffer for the next dependency call
			pgRequests = [][]byte{}
		}
	}(errCh, pgRequests)

	select {
	case <-ctx.Done():
		return ctx.Err()
	case err := <-errCh:
		if err == io.EOF {
			return nil
		}
		return err
	}
}

type QueryData struct {
	PrepIdentifier string `json:"PrepIdentifier" yaml:"PrepIdentifier"`
	Query          string `json:"Query" yaml:"Query"`
}

type PrepMap map[string][]QueryData

type TestPrepMap map[string][]QueryData

func getRecordPrepStatement(allMocks []*models.Mock) PrepMap {
	preparedstatement := make(PrepMap)
	for _, v := range allMocks {
		if v.Kind != "Postgres" {
			continue
		}
		for _, req := range v.Spec.PostgresRequests {
			var querydata []QueryData
			psMap := make(map[string]string)
			if len(req.PacketTypes) > 0 && req.PacketTypes[0] != "p" && req.Identfier != "StartupRequest" {
				p := 0
				for _, header := range req.PacketTypes {
					if header == "P" {
						if strings.Contains(req.Parses[p].Name, "S_") || strings.Contains(req.Parses[p].Name, "s") {
							psMap[req.Parses[p].Query] = req.Parses[p].Name
							querydata = append(querydata, QueryData{PrepIdentifier: req.Parses[p].Name,
								Query: req.Parses[p].Query,
							})

						}
						p++
					}
				}
			}
			// also append the query data for the prepared statement
			if len(querydata) > 0 {
				preparedstatement[v.ConnectionID] = append(preparedstatement[v.ConnectionID], querydata...)
			}
		}

	}
	return preparedstatement
}
