package mysql

import (
	"context"
	"fmt"
	"net"
	"time"

	"go.keploy.io/server/v2/pkg/core/proxy/integrations"
	"go.keploy.io/server/v2/pkg/core/proxy/util"
	"go.keploy.io/server/v2/pkg/models"

	// "go.keploy.io/server/v2/utils"
	"go.uber.org/zap"
)

func decodeOutgoingMySQL(ctx context.Context, logger *zap.Logger, clientConn net.Conn, dstCfg *integrations.ConditionalDstCfg, mockDb integrations.MockMemDb, opts models.OutgoingOptions) error {
	firstLoop := true
	doHandshakeAgain := true
	prevRequest := ""
	var requestBuffers [][]byte
	for {
		configMocks, _ := mockDb.GetUnFilteredMocks()
		tcsMocks, _ := mockDb.GetFilteredMocks()
		//logger.Debug("Config and TCS Mocks", zap.Any("configMocks", configMocks), zap.Any("tcsMocks", tcsMocks))
		if firstLoop || doHandshakeAgain {
			if len(configMocks) == 0 {
				logger.Debug("No more config mocks available")
				return nil
			}

			header := configMocks[0].Spec.MySQLResponses[0].Header
			packet := configMocks[0].Spec.MySQLResponses[0].Message
			opr := configMocks[0].Spec.MySQLResponses[0].Header.PacketType

			binaryPacket, err := encodeToBinary(&packet, header, opr, 0)
			if err != nil {
				logger.Error("Failed to encode to binary", zap.Error(err))
				return err
			}
			fmt.Println("FIRST PACKET", binaryPacket)
			_, err = clientConn.Write(binaryPacket)
			if err != nil {
				logger.Error("Failed to write binary packet", zap.Error(err))
				return err
			}

			matchedIndex := 0
			matchedReqIndex := 0
			configMocks[matchedIndex].Spec.MySQLResponses = append(configMocks[matchedIndex].Spec.MySQLResponses[:matchedReqIndex], configMocks[matchedIndex].Spec.MySQLResponses[matchedReqIndex+1:]...)
			if len(configMocks[matchedIndex].Spec.MySQLResponses) == 0 {
				configMocks = (append(configMocks[:matchedIndex], configMocks[matchedIndex+1:]...))
			}
			mockDb.SetUnFilteredMocks(configMocks)
			firstLoop = false
			doHandshakeAgain = false
			logger.Debug("BINARY PACKET SENT HANDSHAKE", zap.ByteString("binaryPacketKey", binaryPacket))
			prevRequest = "MYSQLHANDSHAKE"
		} else {
			// fmt.Println(time.Duration(delay) * time.Second)
			// fmt.Println("Timeout duration test ->", opts.SQLDelay)
			timeoutDuration := 2 * time.Duration(10) * time.Second // 2-second timeout
			err := clientConn.SetReadDeadline(time.Now().Add(timeoutDuration))
			if err != nil {
				logger.Error("Failed to set read deadline", zap.Error(err))
				return err
			}

			// Attempt to read from the client
			requestBuffer, err := util.ReadBytes(ctx, logger, clientConn)
			requestBuffers = append(requestBuffers, requestBuffer)
			fmt.Println("REQUEST BUFFER", requestBuffer)
			// Reset the read deadline
			clientConn.SetReadDeadline(time.Time{})

			if err != nil {
				if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
					// Timeout occurred, no data received from client
					// Re-initiate handshake without logging an error
					doHandshakeAgain = true
					continue
				} else {
					// Handle other errors
					// logger.Error("Failed to read bytes from clientConn", zap.Error(err))
					return nil
				}
			}
			if len(requestBuffer) == 0 {
				return nil
			}
			if prevRequest == "MYSQLHANDSHAKE" {
				fmt.Println("Expecting handshake response test")
				expectingHandshakeResponseTest = true
			}

			oprRequest, requestHeader, decodedRequest, err := DecodeMySQLPacket(logger, bytesToMySQLPacket(requestBuffer))
			if err != nil {
				logger.Error("Failed to decode MySQL packet", zap.Error(err))
				return err
			}
			if oprRequest == "COM_QUIT" {
				return nil
			}
			if expectingHandshakeResponseTest {
				// configMocks = configMocks[1:]
				// h.SetConfigMocks(configMocks)
				expectingHandshakeResponseTest = false
			}

			prevRequest = ""
			logger.Debug("Logging request buffer and operation request",
				zap.ByteString("requestBuffer", requestBuffer),
				zap.String("oprRequest", oprRequest))

			mysqlRequest := models.MySQLRequest{
				Header: &models.MySQLPacketHeader{
					PacketLength: requestHeader.PayloadLength,
					PacketNumber: requestHeader.SequenceID,
					PacketType:   oprRequest,
				},
				Message: decodedRequest,
			}
			if oprRequest == "COM_STMT_CLOSE" {
				return nil
			}
			matchedResponse, matchedIndex, _, err := matchRequestWithMock(ctx, mysqlRequest, configMocks, tcsMocks, mockDb)
			if err != nil {
				logger.Error("Failed to match request with mock", zap.Error(err))
				return nil
			}
			if matchedIndex != -1 {
				responseBinary, err := encodeToBinary(&matchedResponse.Message, matchedResponse.Header, matchedResponse.Header.PacketType, 1)
				logger.Debug("Response binary",
					zap.ByteString("responseBinary", responseBinary),
					zap.String("packetType", matchedResponse.Header.PacketType))

				if err != nil {
					logger.Error("Failed to encode response to binary", zap.Error(err))
					return err
				}
				fmt.Println("Normal RESPONSE BINARY", responseBinary)
				_, err = clientConn.Write(responseBinary)
				if err != nil {
					logger.Error("Failed to write response to clientConn", zap.Error(err))
					return err
				}

			} else {
				responseBuffer, err := util.PassThrough(ctx, logger, clientConn, dstCfg, requestBuffers)
				if err != nil {
					return err
				}
				_, err = clientConn.Write(responseBuffer)
				if err != nil {
					logger.Error("Failed to write response to clientConn", zap.Error(err))
					return err
				}
			}
		}
	}
}

// func decodeMySQL(ctx context.Context, logger *zap.Logger, clientConn net.Conn, dstCfg *integrations.ConditionalDstCfg, mockDb integrations.MockMemDb, opts models.OutgoingOptions) error {
// 	fmt.Println("decodeMySQL...")
// 	firstLoop := true
// 	doHandshakeAgain := true
// 	prevRequest := ""
// 	var requestBuffers [][]byte

// 	configMocks, err := mockDb.GetUnFilteredMocks()
// 	if err != nil {
// 		utils.LogError(logger, err, "Failed to get unfiltered mocks")
// 		return err
// 	}

// 	tcsMocks, err := mockDb.GetFilteredMocks()
// 	if err != nil {
// 		utils.LogError(logger, err, "Failed to get filtered mocks")
// 		return err
// 	}

// 	errCh := make(chan error, 1)

// 	go func(errCh chan error, configMocks []*models.Mock, tcsMocks []*models.Mock, prevRequest string, requestBuffers [][]byte) {
// 		defer utils.Recover(logger)
// 		defer close(errCh)
// 		for {
// 			//log.Debug("Config and TCS Mocks", zap.Any("configMocks", configMocks), zap.Any("tcsMocks", tcsMocks))
// 			if firstLoop || doHandshakeAgain {
// 				if len(configMocks) == 0 {
// 					logger.Debug("No more config mocks available")
// 					errCh <- err
// 					return
// 				}
// 				sqlMock, found := getFirstSQLMock(configMocks)
// 				if !found {
// 					logger.Debug("No SQL mock found")
// 					errCh <- err
// 					return
// 				}
// 				header := sqlMock.Spec.MySQLResponses[0].Header
// 				packet := sqlMock.Spec.MySQLResponses[0].Message
// 				opr := sqlMock.Spec.MySQLResponses[0].Header.PacketType
// 				fmt.Println("Before encodeToBinary")
// 				binaryPacket, err := encodeToBinary(&packet, header, opr, 0)
// 				if err != nil {
// 					utils.LogError(logger, err, "Failed to encode to binary")
// 					errCh <- err
// 					return
// 				}

// 				_, err = clientConn.Write(binaryPacket)
// 				if err != nil {
// 					if ctx.Err() != nil {
// 						return
// 					}
// 					utils.LogError(logger, err, "Failed to write binary packet")
// 					errCh <- err
// 					return
// 				}
// 				matchedIndex := 0
// 				matchedReqIndex := 0
// 				configMocks[matchedIndex].Spec.MySQLResponses = append(configMocks[matchedIndex].Spec.MySQLResponses[:matchedReqIndex], configMocks[matchedIndex].Spec.MySQLResponses[matchedReqIndex+1:]...)
// 				if len(configMocks[matchedIndex].Spec.MySQLResponses) == 0 {
// 					configMocks = append(configMocks[:matchedIndex], configMocks[matchedIndex+1:]...)
// 				}
// 				//h.SetConfigMocks(configMocks)
// 				firstLoop = false
// 				doHandshakeAgain = false
// 				logger.Info("BINARY PACKET SENT HANDSHAKE", zap.ByteString("binaryPacketKey", binaryPacket))
// 				prevRequest = "MYSQLHANDSHAKE"
// 			} else {

// 				// fmt.Println(time.Duration(delay) * time.Second)
// 				timeoutDuration := 2 * time.Duration(opts.SQLDelay) * time.Second // 2-second timeout
// 				err := clientConn.SetReadDeadline(time.Now().Add(timeoutDuration))
// 				if err != nil {
// 					utils.LogError(logger, err, "Failed to set read deadline")
// 					errCh <- err
// 					return
// 				}

// 				// Attempt to read from the client
// 				requestBuffer, err := util.ReadBytes(ctx, logger, clientConn)
// 				if err != nil {
// 					if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
// 						// Timeout occurred, no data received from client
// 						// Re-initiate handshake without logging an error
// 						doHandshakeAgain = true
// 						continue
// 					}
// 					// Handle other errors
// 					// log.Error("Failed to read bytes from clientConn", zap.Error(err))
// 					errCh <- err
// 					return
// 				}

// 				// Reset the read deadline
// 				err = clientConn.SetReadDeadline(time.Time{})
// 				if err != nil {
// 					utils.LogError(logger, err, "Failed to reset read deadline")
// 					errCh <- err
// 					return
// 				}

// 				requestBuffers = append(requestBuffers, requestBuffer)

// 				if len(requestBuffer) == 0 {
// 					logger.Debug("Request buffer is empty")
// 					errCh <- err
// 					return
// 				}
// 				if prevRequest == "MYSQLHANDSHAKE" {
// 					expectingHandshakeResponseTest = true
// 				}

// 				oprRequest, requestHeader, decodedRequest, err := DecodeMySQLPacket(logger, bytesToMySQLPacket(requestBuffer))
// 				if err != nil {
// 					utils.LogError(logger, err, "Failed to decode MySQL packet")
// 					errCh <- err
// 					return
// 				}
// 				if oprRequest == "COM_QUIT" {
// 					logger.Debug("COM_QUIT received")
// 					errCh <- err
// 					return
// 				}
// 				if expectingHandshakeResponseTest {
// 					// configMocks = configMocks[1:]
// 					// h.SetConfigMocks(configMocks)
// 					expectingHandshakeResponseTest = false
// 				}

// 				prevRequest = ""
// 				logger.Info("Logging request buffer and operation request",
// 					zap.ByteString("requestBuffer", requestBuffer),
// 					zap.String("oprRequest", oprRequest))

// 				mysqlRequest := models.MySQLRequest{
// 					Header: &models.MySQLPacketHeader{
// 						PacketLength: requestHeader.PayloadLength,
// 						PacketNumber: requestHeader.SequenceID,
// 						PacketType:   oprRequest,
// 					},
// 					Message: decodedRequest,
// 				}
// 				if oprRequest == "COM_STMT_CLOSE" {
// 					logger.Debug("COM_STMT_CLOSE received")
// 					errCh <- err
// 					return
// 				}
// 				//TODO: both in case of no match or some other error, we are receiving the error.
// 				// Due to this, there will be no passthrough in case of no match.
// 				matchedResponse, matchedIndex, _, err := matchRequestWithMock(ctx, mysqlRequest, configMocks, tcsMocks)
// 				if err != nil {
// 					utils.LogError(logger, err, "Failed to match request with mock")
// 					errCh <- err
// 					return
// 				}

// 				if matchedIndex == -1 {
// 					logger.Debug("No matching mock found")

// 					responseBuffer, err := util.PassThrough(ctx, logger, clientConn, dstCfg, requestBuffers)
// 					if err != nil {
// 						utils.LogError(logger, err, "Failed to passthrough the mysql request to the actual database server")
// 						errCh <- err
// 						return
// 					}
// 					_, err = clientConn.Write(responseBuffer)
// 					if err != nil {
// 						if ctx.Err() != nil {
// 							return
// 						}
// 						utils.LogError(logger, err, "Failed to write response to clientConn")
// 						errCh <- err
// 						return
// 					}
// 					continue
// 				}

// 				responseBinary, err := encodeToBinary(&matchedResponse.Message, matchedResponse.Header, matchedResponse.Header.PacketType, 1)
// 				logger.Debug("Response binary",
// 					zap.ByteString("responseBinary", responseBinary),
// 					zap.String("packetType", matchedResponse.Header.PacketType))

// 				if err != nil {
// 					utils.LogError(logger, err, "Failed to encode response to binary")
// 					errCh <- err
// 					return
// 				}

// 				_, err = clientConn.Write(responseBinary)
// 				if err != nil {
// 					if ctx.Err() != nil {
// 						return
// 					}
// 					utils.LogError(logger, err, "Failed to write response to clientConn")
// 					errCh <- err
// 					return
// 				}
// 			}
// 		}
// 	}(errCh, configMocks, tcsMocks, prevRequest, requestBuffers)

// 	select {
// 	case <-ctx.Done():
// 		return ctx.Err()
// 	case err := <-errCh:
// 		return err
// 	}
// }

// func getFirstSQLMock(configMocks []*models.Mock) (*models.Mock, bool) {
// 	for _, mock := range configMocks {
// 		if len(mock.Spec.MySQLResponses) > 0 && mock.Kind == "SQL" && mock.Spec.MySQLResponses[0].Header.PacketType == "MySQLHandshakeV10" {
// 			return mock, true
// 		}
// 	}
// 	return nil, false
// }
