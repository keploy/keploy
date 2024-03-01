package mysql

import (
	"context"
	"go.keploy.io/server/v2/pkg/core/proxy/integrations"
	"go.keploy.io/server/v2/pkg/core/proxy/util"
	"go.keploy.io/server/v2/pkg/models"
	"go.uber.org/zap"
	"net"
	"time"
)

func decodeMySql(ctx context.Context, logger *zap.Logger, clientConn net.Conn, dstCfg *integrations.ConditionalDstCfg, mockDb integrations.MockMemDb, opts models.OutgoingOptions) error {
	firstLoop := true
	doHandshakeAgain := true
	prevRequest := ""
	var requestBuffers [][]byte

	configMocks, err := mockDb.GetUnFilteredMocks()
	if err != nil {
		logger.Error("Failed to get unfiltered mocks", zap.Error(err))
		return err
	}

	tcsMocks, err := mockDb.GetFilteredMocks()
	if err != nil {
		logger.Error("Failed to get filtered mocks", zap.Error(err))
		return err
	}

	for {
		select {
		case <-ctx.Done():
			return nil
		default:

			//log.Debug("Config and TCS Mocks", zap.Any("configMocks", configMocks), zap.Any("tcsMocks", tcsMocks))
			if firstLoop || doHandshakeAgain {
				if len(configMocks) == 0 {
					logger.Debug("No more config mocks available")
					return nil
				}
				sqlMock, found := getfirstSQLMock(configMocks)
				if !found {
					logger.Debug("No SQL mock found")
					return nil
				}
				header := sqlMock.Spec.MySqlResponses[0].Header
				packet := sqlMock.Spec.MySqlResponses[0].Message
				opr := sqlMock.Spec.MySqlResponses[0].Header.PacketType

				binaryPacket, err := encodeToBinary(&packet, header, opr, 0)
				if err != nil {
					logger.Error("Failed to encode to binary", zap.Error(err))
					return err
				}

				_, err = clientConn.Write(binaryPacket)
				if err != nil {
					logger.Error("Failed to write binary packet", zap.Error(err))
					return err
				}
				matchedIndex := 0
				matchedReqIndex := 0
				configMocks[matchedIndex].Spec.MySqlResponses = append(configMocks[matchedIndex].Spec.MySqlResponses[:matchedReqIndex], configMocks[matchedIndex].Spec.MySqlResponses[matchedReqIndex+1:]...)
				if len(configMocks[matchedIndex].Spec.MySqlResponses) == 0 {
					configMocks = append(configMocks[:matchedIndex], configMocks[matchedIndex+1:]...)
				}
				//h.SetConfigMocks(configMocks)
				firstLoop = false
				doHandshakeAgain = false
				logger.Debug("BINARY PACKET SENT HANDSHAKE", zap.ByteString("binaryPacketKey", binaryPacket))
				prevRequest = "MYSQLHANDSHAKE"
			} else {

				// fmt.Println(time.Duration(delay) * time.Second)
				timeoutDuration := 2 * time.Duration(opts.SQLDelay) * time.Second // 2-second timeout
				err := clientConn.SetReadDeadline(time.Now().Add(timeoutDuration))
				if err != nil {
					logger.Error("Failed to set read deadline", zap.Error(err))
					return err
				}

				// Attempt to read from the client
				requestBuffer, err := util.ReadBytes(ctx, clientConn)
				if err != nil {
					if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
						// Timeout occurred, no data received from client
						// Re-initiate handshake without logging an error
						doHandshakeAgain = true
						continue
					} else {
						// Handle other errors
						// log.Error("Failed to read bytes from clientConn", zap.Error(err))
						return err
					}
				}

				// Reset the read deadline
				err = clientConn.SetReadDeadline(time.Time{})
				if err != nil {
					logger.Error("Failed to reset read deadline", zap.Error(err))
					return err
				}

				requestBuffers = append(requestBuffers, requestBuffer)

				if len(requestBuffer) == 0 {
					logger.Debug("Request buffer is empty")
					return nil
				}
				if prevRequest == "MYSQLHANDSHAKE" {
					expectingHandshakeResponseTest = true
				}

				oprRequest, requestHeader, decodedRequest, err := DecodeMySQLPacket(logger, bytesToMySQLPacket(requestBuffer))
				if err != nil {
					logger.Error("Failed to decode MySQL packet", zap.Error(err))
					return err
				}
				if oprRequest == "COM_QUIT" {
					logger.Debug("COM_QUIT received")
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
					logger.Debug("COM_STMT_CLOSE received")
					return nil
				}
				//TODO: both in case of no match or some other error, we are receiving the error.
				// Due to this, there will be no passthrough in case of no match.
				matchedResponse, matchedIndex, _, err := matchRequestWithMock(mysqlRequest, configMocks, tcsMocks)
				if err != nil {
					logger.Error("Failed to match request with mock", zap.Error(err))
					return err
				}

				if matchedIndex == -1 {
					logger.Debug("No matching mock found")

					// making destConn
					destConn, err := net.Dial("tcp", dstCfg.Addr)
					if err != nil {
						logger.Error("failed to dial the destination server", zap.Error(err))
						return err
					}

					responseBuffer, err := util.PassThrough(ctx, logger, clientConn, destConn, requestBuffers)
					if err != nil {
						logger.Error("failed to passthrough the mysql request to the actual database server", zap.Error(err))
						return err
					}
					_, err = clientConn.Write(responseBuffer)
					if err != nil {
						logger.Error("Failed to write response to clientConn", zap.Error(err))
						return err
					}
					continue
				}

				responseBinary, err := encodeToBinary(&matchedResponse.Message, matchedResponse.Header, matchedResponse.Header.PacketType, 1)
				logger.Debug("Response binary",
					zap.ByteString("responseBinary", responseBinary),
					zap.String("packetType", matchedResponse.Header.PacketType))

				if err != nil {
					logger.Error("Failed to encode response to binary", zap.Error(err))
					return err
				}

				_, err = clientConn.Write(responseBinary)
				if err != nil {
					logger.Error("Failed to write response to clientConn", zap.Error(err))
					return err
				}
			}
		}
	}
}

func getfirstSQLMock(configMocks []*models.Mock) (*models.Mock, bool) {
	for _, mock := range configMocks {
		if len(mock.Spec.MySqlResponses) > 0 && mock.Kind == "SQL" && mock.Spec.MySqlResponses[0].Header.PacketType == "MySQLHandshakeV10" {
			return mock, true
		}
	}
	return nil, false
}
