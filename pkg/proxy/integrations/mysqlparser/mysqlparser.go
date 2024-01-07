package mysqlparser

import (
	"context"
	"encoding/binary"
	"fmt"
	"log"
	"net"
	"time"

	"go.keploy.io/server/pkg/hooks"
	"go.keploy.io/server/pkg/models"
	"go.keploy.io/server/pkg/proxy/util"
	"go.uber.org/zap"
)

type MySqlParser struct {
	logger *zap.Logger
	hooks  *hooks.Hook
	delay  uint64
}

func NewMySqlParser(logger *zap.Logger, hooks *hooks.Hook, delay uint64) *MySqlParser {
	return &MySqlParser{
		logger: logger,
		hooks:  hooks,
		delay:  delay,
	}
}

func (sql *MySqlParser) OutgoingType(buffer []byte) bool {
	//Returning false here because sql parser is using the ports to check if the packet is mysql or not.
	return false
}
func (sql *MySqlParser) ProcessOutgoing(requestBuffer []byte, clientConn, destConn net.Conn, ctx context.Context) {
	delay := sql.delay
	switch models.GetMode() {
	case models.MODE_RECORD:
		encodeOutgoingMySql(requestBuffer, clientConn, destConn, sql.hooks, sql.logger, ctx)
	case models.MODE_TEST:
		decodeOutgoingMySQL(requestBuffer, clientConn, destConn, sql.hooks, sql.logger, ctx, delay)
	default:
	}
}

var (
	isConfigRecorded = false
)
var (
	isPluginData = false
)
var (
	expectingAuthSwitchResponse = false
)
var (
	expectingHandshakeResponse = false
)

func encodeOutgoingMySql(requestBuffer []byte, clientConn, destConn net.Conn, h *hooks.Hook, logger *zap.Logger, ctx context.Context) {
	var (
		mysqlRequests  = []models.MySQLRequest{}
		mysqlResponses = []models.MySQLResponse{}
	)
	for {
		lastCommand = 0x00 //resetting last command for new loop
		data, source, err := ReadFirstBuffer(clientConn, destConn)
		if len(data) == 0 {
			break
		}
		if err != nil {
			logger.Error("failed to read initial data", zap.Error(err))
			return
		}
		if source == "destination" {
			handshakeResponseBuffer := data
			_, err = clientConn.Write(handshakeResponseBuffer)
			if err != nil {
				logger.Error("failed to write handshake request to client", zap.Error(err))
				return
			}
			handshakeResponseFromClient, err := util.ReadBytes(clientConn)
			if err != nil {
				logger.Error("failed to read handshake response from client", zap.Error(err))
				return
			}
			_, err = destConn.Write(handshakeResponseFromClient)
			if err != nil {
				logger.Error("failed to write handshake response to server", zap.Error(err))
				return
			}
			time.Sleep(100 * time.Millisecond)
			okPacket1, err := util.ReadBytes(destConn)
			if err != nil {
				logger.Error("failed to read packet from server after handshake", zap.Error(err))
				return
			}
			_, err = clientConn.Write(okPacket1)
			if err != nil {
				logger.Error("failed to write auth switch request to client", zap.Error(err))
				return
			}
			expectingHandshakeResponse = true
			oprRequest, requestHeader, mysqlRequest, err := DecodeMySQLPacket(bytesToMySQLPacket(handshakeResponseFromClient), logger, destConn)
			if err != nil {
				logger.Error("failed to decode MySQL packet from client", zap.Error(err))
				return
			}
			mysqlRequests = append(mysqlRequests, models.MySQLRequest{
				Header: &models.MySQLPacketHeader{
					PacketLength: requestHeader.PayloadLength,
					PacketNumber: requestHeader.SequenceID,
					PacketType:   oprRequest,
				},
				Message: mysqlRequest,
			})
			expectingHandshakeResponse = false
			oprResponse1, responseHeader1, mysqlResp1, err := DecodeMySQLPacket(bytesToMySQLPacket(handshakeResponseBuffer), logger, destConn)
			if err != nil {
				logger.Error("failed to decode MySQL packet from destination", zap.Error(err))
				return
			}
			mysqlResponses = append(mysqlResponses, models.MySQLResponse{
				Header: &models.MySQLPacketHeader{
					PacketLength: responseHeader1.PayloadLength,
					PacketNumber: responseHeader1.SequenceID,
					PacketType:   oprResponse1,
				},
				Message: mysqlResp1,
			})
			oprResponse2, responseHeader2, mysqlResp2, err := DecodeMySQLPacket(bytesToMySQLPacket(okPacket1), logger, destConn)
			if err != nil {
				logger.Error("failed to decode MySQL packet from OK packet", zap.Error(err))
				return
			}
			mysqlResponses = append(mysqlResponses, models.MySQLResponse{
				Header: &models.MySQLPacketHeader{
					PacketLength: responseHeader2.PayloadLength,
					PacketNumber: responseHeader2.SequenceID,
					PacketType:   oprResponse2,
				},
				Message: mysqlResp2,
			})
			if oprResponse2 == "AUTH_SWITCH_REQUEST" {

				authSwitchResponse, err := util.ReadBytes(clientConn)
				if err != nil {
					logger.Error("failed to read AuthSwitchResponse from client", zap.Error(err))
					return
				}
				_, err = destConn.Write(authSwitchResponse)
				if err != nil {
					logger.Error("failed to write AuthSwitchResponse to server", zap.Error(err))
					return
				}
				ServerResponse, err := util.ReadBytes(destConn)
				if err != nil {
					logger.Error("failed to read final response from server", zap.Error(err))
					return
				}
				_, err = clientConn.Write(ServerResponse)
				if err != nil {
					logger.Error("failed to write final response to client", zap.Error(err))
					return
				}
				expectingAuthSwitchResponse = true

				oprRequestFinal, requestHeaderFinal, mysqlRequestFinal, err := DecodeMySQLPacket(bytesToMySQLPacket(authSwitchResponse), logger, destConn)
				if err != nil {
					logger.Error("failed to decode MySQL packet from client after full authentication", zap.Error(err))
					return
				}
				mysqlRequests = append(mysqlRequests, models.MySQLRequest{
					Header: &models.MySQLPacketHeader{
						PacketLength: requestHeaderFinal.PayloadLength,
						PacketNumber: requestHeaderFinal.SequenceID,
						PacketType:   oprRequestFinal,
					},
					Message: mysqlRequestFinal,
				})
				expectingAuthSwitchResponse = false

				isPluginData = true
				oprResponse, responseHeader, mysqlResp, err := DecodeMySQLPacket(bytesToMySQLPacket(ServerResponse), logger, destConn)
				isPluginData = false
				if err != nil {
					logger.Error("failed to decode MySQL packet from destination after full authentication", zap.Error(err))
					return
				}
				mysqlResponses = append(mysqlResponses, models.MySQLResponse{
					Header: &models.MySQLPacketHeader{
						PacketLength: responseHeader.PayloadLength,
						PacketNumber: responseHeader.SequenceID,
						PacketType:   oprResponse,
					},
					Message: mysqlResp,
				})
				var pluginType string

				if handshakeResp, ok := mysqlResp.(*HandshakeResponseOk); ok {
					pluginType = handshakeResp.PluginDetails.Type
				}
				if pluginType == "cachingSha2PasswordPerformFullAuthentication" {

					clientResponse, err := util.ReadBytes(clientConn)
					if err != nil {
						logger.Error("failed to read response from client", zap.Error(err))
						return
					}
					_, err = destConn.Write(clientResponse)
					if err != nil {
						logger.Error("failed to write client's response to server", zap.Error(err))
						return
					}
					finalServerResponse, err := util.ReadBytes(destConn)
					if err != nil {
						logger.Error("failed to read final response from server", zap.Error(err))
						return
					}
					_, err = clientConn.Write(finalServerResponse)
					if err != nil {
						logger.Error("failed to write final response to client", zap.Error(err))
						return
					}
					oprRequestFinal, requestHeaderFinal, mysqlRequestFinal, err := DecodeMySQLPacket(bytesToMySQLPacket(clientResponse), logger, destConn)
					if err != nil {
						logger.Error("failed to decode MySQL packet from client after full authentication", zap.Error(err))
						return
					}
					mysqlRequests = append(mysqlRequests, models.MySQLRequest{
						Header: &models.MySQLPacketHeader{
							PacketLength: requestHeaderFinal.PayloadLength,
							PacketNumber: requestHeaderFinal.SequenceID,
							PacketType:   oprRequestFinal,
						},
						Message: mysqlRequestFinal,
					})
					isPluginData = true
					oprResponseFinal, responseHeaderFinal, mysqlRespFinal, err := DecodeMySQLPacket(bytesToMySQLPacket(finalServerResponse), logger, destConn)
					isPluginData = false
					if err != nil {
						logger.Error("failed to decode MySQL packet from destination after full authentication", zap.Error(err))
						return
					}
					mysqlResponses = append(mysqlResponses, models.MySQLResponse{
						Header: &models.MySQLPacketHeader{
							PacketLength: responseHeaderFinal.PayloadLength,
							PacketNumber: responseHeaderFinal.SequenceID,
							PacketType:   oprResponseFinal,
						},
						Message: mysqlRespFinal,
					})
					clientResponse1, err := util.ReadBytes(clientConn)
					if err != nil {
						logger.Error("failed to read response from client", zap.Error(err))
						return
					}
					_, err = destConn.Write(clientResponse1)
					if err != nil {
						logger.Error("failed to write client's response to server", zap.Error(err))
						return
					}
					finalServerResponse1, err := util.ReadBytes(destConn)
					if err != nil {
						logger.Error("failed to read final response from server", zap.Error(err))
						return
					}
					_, err = clientConn.Write(finalServerResponse1)
					if err != nil {
						logger.Error("failed to write final response to client", zap.Error(err))
						return
					}
					finalServerResponsetype1, finalServerResponseHeader1, mysqlRespfinalServerResponse, err := DecodeMySQLPacket(bytesToMySQLPacket(finalServerResponse1), logger, destConn)
					if err != nil {
						logger.Error("failed to decode MySQL packet from final server response", zap.Error(err))
						return
					}
					mysqlResponses = append(mysqlResponses, models.MySQLResponse{
						Header: &models.MySQLPacketHeader{
							PacketLength: finalServerResponseHeader1.PayloadLength,
							PacketNumber: finalServerResponseHeader1.SequenceID,
							PacketType:   finalServerResponsetype1,
						},
						Message: mysqlRespfinalServerResponse,
					})
					oprRequestFinal1, requestHeaderFinal1, err := decodeEncryptPassword(clientResponse1)
					if err != nil {
						logger.Error("failed to decode MySQL packet from client after full authentication", zap.Error(err))
						return
					}
					type DataMessage struct {
						Data []byte
					}
					mysqlRequests = append(mysqlRequests, models.MySQLRequest{
						Header: &models.MySQLPacketHeader{
							PacketLength: requestHeaderFinal1.PayloadLength,
							PacketNumber: requestHeaderFinal1.SequenceID,
							PacketType:   oprRequestFinal1,
						},
						Message: DataMessage{
							Data: requestHeaderFinal1.Payload,
						},
					})
				} else {
					// time.Sleep(10 * time.Millisecond)
					finalServerResponse, err := util.ReadBytes(destConn)
					if err != nil {
						logger.Error("failed to read final response from server", zap.Error(err))
						return
					}
					_, err = clientConn.Write(finalServerResponse)
					if err != nil {
						logger.Error("failed to write final response to client", zap.Error(err))
						return
					}
					oprResponseFinal, responseHeaderFinal, mysqlRespFinal, err := DecodeMySQLPacket(bytesToMySQLPacket(finalServerResponse), logger, destConn)
					isPluginData = false
					if err != nil {
						logger.Error("failed to decode MySQL packet from destination after full authentication", zap.Error(err))
						return
					}
					mysqlResponses = append(mysqlResponses, models.MySQLResponse{
						Header: &models.MySQLPacketHeader{
							PacketLength: responseHeaderFinal.PayloadLength,
							PacketNumber: responseHeaderFinal.SequenceID,
							PacketType:   oprResponseFinal,
						},
						Message: mysqlRespFinal,
					})
				}

			}

			var pluginType string

			if handshakeResp, ok := mysqlResp2.(*HandshakeResponseOk); ok {
				pluginType = handshakeResp.PluginDetails.Type
			}
			if pluginType == "cachingSha2PasswordPerformFullAuthentication" {

				clientResponse, err := util.ReadBytes(clientConn)
				if err != nil {
					logger.Error("failed to read response from client", zap.Error(err))
					return
				}
				_, err = destConn.Write(clientResponse)
				if err != nil {
					logger.Error("failed to write client's response to server", zap.Error(err))
					return
				}
				finalServerResponse, err := util.ReadBytes(destConn)
				if err != nil {
					logger.Error("failed to read final response from server", zap.Error(err))
					return
				}
				_, err = clientConn.Write(finalServerResponse)
				if err != nil {
					logger.Error("failed to write final response to client", zap.Error(err))
					return
				}
				oprRequestFinal, requestHeaderFinal, mysqlRequestFinal, err := DecodeMySQLPacket(bytesToMySQLPacket(clientResponse), logger, destConn)
				if err != nil {
					logger.Error("failed to decode MySQL packet from client after full authentication", zap.Error(err))
					return
				}
				mysqlRequests = append(mysqlRequests, models.MySQLRequest{
					Header: &models.MySQLPacketHeader{
						PacketLength: requestHeaderFinal.PayloadLength,
						PacketNumber: requestHeaderFinal.SequenceID,
						PacketType:   oprRequestFinal,
					},
					Message: mysqlRequestFinal,
				})
				isPluginData = true
				oprResponseFinal, responseHeaderFinal, mysqlRespFinal, err := DecodeMySQLPacket(bytesToMySQLPacket(finalServerResponse), logger, destConn)
				isPluginData = false
				if err != nil {
					logger.Error("failed to decode MySQL packet from destination after full authentication", zap.Error(err))
					return
				}
				mysqlResponses = append(mysqlResponses, models.MySQLResponse{
					Header: &models.MySQLPacketHeader{
						PacketLength: responseHeaderFinal.PayloadLength,
						PacketNumber: responseHeaderFinal.SequenceID,
						PacketType:   oprResponseFinal,
					},
					Message: mysqlRespFinal,
				})
				clientResponse1, err := util.ReadBytes(clientConn)
				if err != nil {
					logger.Error("failed to read response from client", zap.Error(err))
					return
				}
				_, err = destConn.Write(clientResponse1)
				if err != nil {
					logger.Error("failed to write client's response to server", zap.Error(err))
					return
				}
				finalServerResponse1, err := util.ReadBytes(destConn)
				if err != nil {
					logger.Error("failed to read final response from server", zap.Error(err))
					return
				}
				_, err = clientConn.Write(finalServerResponse1)
				if err != nil {
					logger.Error("failed to write final response to client", zap.Error(err))
					return
				}
				finalServerResponsetype1, finalServerResponseHeader1, mysqlRespfinalServerResponse, err := DecodeMySQLPacket(bytesToMySQLPacket(finalServerResponse1), logger, destConn)
				if err != nil {
					logger.Error("failed to decode MySQL packet from final server response", zap.Error(err))
					return
				}
				mysqlResponses = append(mysqlResponses, models.MySQLResponse{
					Header: &models.MySQLPacketHeader{
						PacketLength: finalServerResponseHeader1.PayloadLength,
						PacketNumber: finalServerResponseHeader1.SequenceID,
						PacketType:   finalServerResponsetype1,
					},
					Message: mysqlRespfinalServerResponse,
				})
				oprRequestFinal1, requestHeaderFinal1, err := decodeEncryptPassword(clientResponse1)
				if err != nil {
					logger.Error("failed to decode MySQL packet from client after full authentication", zap.Error(err))
					return
				}
				type DataMessage struct {
					Data []byte
				}
				mysqlRequests = append(mysqlRequests, models.MySQLRequest{
					Header: &models.MySQLPacketHeader{
						PacketLength: requestHeaderFinal1.PayloadLength,
						PacketNumber: requestHeaderFinal1.SequenceID,
						PacketType:   oprRequestFinal1,
					},
					Message: DataMessage{
						Data: requestHeaderFinal1.Payload,
					},
				})
			}
			recordMySQLMessage(h, mysqlRequests, mysqlResponses, oprRequest, oprResponse2, "config", ctx)
			mysqlRequests = []models.MySQLRequest{}
			mysqlResponses = []models.MySQLResponse{}
			handleClientQueries(h, nil, clientConn, destConn, logger, ctx)
		} else if source == "client" {
			handleClientQueries(h, nil, clientConn, destConn, logger, ctx)
		}
	}
	return
}

var (
	mockResponseRead = 0
)

var (
	expectingHandshakeResponseTest = false
)

func getfirstSQLMock(configMocks []*models.Mock) (*models.Mock, bool) {
	for _, mock := range configMocks {
		if len(mock.Spec.MySqlResponses) > 0 && mock.Kind == "SQL" && mock.Spec.MySqlResponses[0].Header.PacketType == "MySQLHandshakeV10" {
			return mock, true
		}
	}
	return nil, false
}

func decodeOutgoingMySQL(requestBuffer []byte, clientConn, destConn net.Conn, h *hooks.Hook, logger *zap.Logger, ctx context.Context, delay uint64) {
	firstLoop := true
	doHandshakeAgain := true
	prevRequest := ""
	var requestBuffers [][]byte
	configMocks, _ := h.GetConfigMocks()
	tcsMocks, _ := h.GetTcsMocks()
	for {

		//logger.Debug("Config and TCS Mocks", zap.Any("configMocks", configMocks), zap.Any("tcsMocks", tcsMocks))
		if firstLoop || doHandshakeAgain {
			if len(configMocks) == 0 {
				logger.Debug("No more config mocks available")
				return
			}
			sqlMock, found := getfirstSQLMock(configMocks)
			if !found {
				logger.Debug("No SQL mock found")
				return
			}
			header := sqlMock.Spec.MySqlResponses[0].Header
			packet := sqlMock.Spec.MySqlResponses[0].Message
			opr := sqlMock.Spec.MySqlResponses[0].Header.PacketType

			binaryPacket, err := encodeToBinary(&packet, header, opr, 0)
			if err != nil {
				logger.Error("Failed to encode to binary", zap.Error(err))
				return
			}

			_, err = clientConn.Write(binaryPacket)
			if err != nil {
				logger.Error("Failed to write binary packet", zap.Error(err))
				return
			}
			matchedIndex := 0
			matchedReqIndex := 0
			configMocks[matchedIndex].Spec.MySqlResponses = append(configMocks[matchedIndex].Spec.MySqlResponses[:matchedReqIndex], configMocks[matchedIndex].Spec.MySqlResponses[matchedReqIndex+1:]...)
			if len(configMocks[matchedIndex].Spec.MySqlResponses) == 0 {
				configMocks = (append(configMocks[:matchedIndex], configMocks[matchedIndex+1:]...))
			}
			//h.SetConfigMocks(configMocks)
			firstLoop = false
			doHandshakeAgain = false
			logger.Debug("BINARY PACKET SENT HANDSHAKE", zap.ByteString("binaryPacketKey", binaryPacket))
			prevRequest = "MYSQLHANDSHAKE"
		} else {
			// fmt.Println(time.Duration(delay) * time.Second)
			timeoutDuration := 2 * time.Duration(delay) * time.Second // 2-second timeout
			err := clientConn.SetReadDeadline(time.Now().Add(timeoutDuration))
			if err != nil {
				logger.Error("Failed to set read deadline", zap.Error(err))
				return
			}

			// Attempt to read from the client
			requestBuffer, err := util.ReadBytes(clientConn)
			requestBuffers = append(requestBuffers, requestBuffer)
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
					return
				}
			}
			if len(requestBuffer) == 0 {
				return
			}
			if prevRequest == "MYSQLHANDSHAKE" {
				expectingHandshakeResponseTest = true
			}

			oprRequest, requestHeader, decodedRequest, err := DecodeMySQLPacket(bytesToMySQLPacket(requestBuffer), logger, destConn)
			if err != nil {
				logger.Error("Failed to decode MySQL packet", zap.Error(err))
				return
			}
			if oprRequest == "COM_QUIT" {
				return
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
				return
			}
			matchedResponse, matchedIndex, _, err := matchRequestWithMock(mysqlRequest, configMocks, tcsMocks, h)
			if err != nil {
				logger.Error("Failed to match request with mock", zap.Error(err))
				return
			}
			if matchedIndex != -1 {
				responseBinary, err := encodeToBinary(&matchedResponse.Message, matchedResponse.Header, matchedResponse.Header.PacketType, 1)
				logger.Debug("Response binary",
					zap.ByteString("responseBinary", responseBinary),
					zap.String("packetType", matchedResponse.Header.PacketType))

				if err != nil {
					logger.Error("Failed to encode response to binary", zap.Error(err))
					return
				}

				_, err = clientConn.Write(responseBinary)
				if err != nil {
					logger.Error("Failed to write response to clientConn", zap.Error(err))
					return
				}

			} else {
				responseBuffer, err := util.Passthrough(clientConn, destConn, requestBuffers, h.Recover, logger)
				if err != nil {
					return
				}
				_, err = clientConn.Write(responseBuffer)
				if err != nil {
					logger.Error("Failed to write response to clientConn", zap.Error(err))
					return
				}
			}
		}
	}
}

func matchRequestWithMock(mysqlRequest models.MySQLRequest, configMocks, tcsMocks []*models.Mock, h *hooks.Hook) (*models.MySQLResponse, int, string, error) {
	allMocks := append([]*models.Mock(nil), configMocks...)
	allMocks = append(allMocks, tcsMocks...)
	var bestMatch *models.MySQLResponse
	var matchedIndex int
	var matchedReqIndex int
	var mockType string
	maxMatchCount := 0

	for i, mock := range allMocks {
		for j, mockReq := range mock.Spec.MySqlRequests {
			matchCount := compareMySQLRequests(mysqlRequest, mockReq)
			if matchCount > maxMatchCount {
				maxMatchCount = matchCount
				matchedIndex = i
				matchedReqIndex = j
				mockType = mock.Spec.Metadata["type"]
				if len(mock.Spec.MySqlResponses) > j {
					if mockType == "config" {
						responseCopy := mock.Spec.MySqlResponses[j]
						bestMatch = &responseCopy
					} else {
						bestMatch = &mock.Spec.MySqlResponses[j]
					}
				}
			}
		}
	}

	if bestMatch == nil {
		return nil, -1, "", fmt.Errorf("no matching mock found")
	}

	if mockType == "config" {
		if matchedIndex >= len(configMocks) {
			return nil, -1, "", fmt.Errorf("index out of range in configMocks")
		}
		configMocks[matchedIndex].Spec.MySqlRequests = append(configMocks[matchedIndex].Spec.MySqlRequests[:matchedReqIndex], configMocks[matchedIndex].Spec.MySqlRequests[matchedReqIndex+1:]...)
		configMocks[matchedIndex].Spec.MySqlResponses = append(configMocks[matchedIndex].Spec.MySqlResponses[:matchedReqIndex], configMocks[matchedIndex].Spec.MySqlResponses[matchedReqIndex+1:]...)

		if len(configMocks[matchedIndex].Spec.MySqlResponses) == 0 {
			configMocks = append(configMocks[:matchedIndex], configMocks[matchedIndex+1:]...)
		}
		//h.SetConfigMocks(configMocks)
	} else {
		realIndex := matchedIndex - len(configMocks)
		if realIndex < 0 || realIndex >= len(tcsMocks) {
			return nil, -1, "", fmt.Errorf("index out of range in tcsMocks")
		}
		tcsMocks[realIndex].Spec.MySqlRequests = append(tcsMocks[realIndex].Spec.MySqlRequests[:matchedReqIndex], tcsMocks[realIndex].Spec.MySqlRequests[matchedReqIndex+1:]...)
		tcsMocks[realIndex].Spec.MySqlResponses = append(tcsMocks[realIndex].Spec.MySqlResponses[:matchedReqIndex], tcsMocks[realIndex].Spec.MySqlResponses[matchedReqIndex+1:]...)

		if len(tcsMocks[realIndex].Spec.MySqlResponses) == 0 {
			tcsMocks = append(tcsMocks[:realIndex], tcsMocks[realIndex+1:]...)
		}
		//h.SetTcsMocks(tcsMocks)
	}

	return bestMatch, matchedIndex, mockType, nil
}

func compareMySQLRequests(req1, req2 models.MySQLRequest) int {
	matchCount := 0

	// Compare Header fields
	if req1.Header.PacketType == "MySQLQuery" && req2.Header.PacketType == "MySQLQuery" {
		packet1 := req1.Message
		packet, ok := packet1.(*QueryPacket)
		if !ok {
			return 0
		}
		packet2 := req2.Message

		packet3, ok := packet2.(*models.MySQLQueryPacket)
		if !ok {
			return 0
		}
		if packet.Query == packet3.Query {
			matchCount += 5
		}
	}
	if req1.Header.PacketLength == req2.Header.PacketLength {
		matchCount++
	}
	if req1.Header.PacketNumber == req2.Header.PacketNumber {
		matchCount++
	}
	if req1.Header.PacketType == req2.Header.PacketType {
		matchCount++
	}
	return matchCount
}
func ReadFirstBuffer(clientConn, destConn net.Conn) ([]byte, string, error) {
	// Attempt to read from destConn first
	n, err := util.ReadBytes(destConn)
	// If there is data from destConn, return it
	if err == nil {
		return n, "destination", nil
	}
	// If the error is a timeout, try to read from clientConn
	if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
		n, err = util.ReadBytes(clientConn)
		// If there is data from clientConn, return it
		if err == nil {
			return n, "client", nil
		}
		// Return any error from reading clientConn
		return nil, "", err
	}
	// Return any other error from reading destConn
	return nil, "", err
}
func handleClientQueries(h *hooks.Hook, initialBuffer []byte, clientConn, destConn net.Conn, logger *zap.Logger, ctx context.Context) ([]*models.Mock, error) {
	firstIteration := true
	var (
		mysqlRequests  []models.MySQLRequest
		mysqlResponses []models.MySQLResponse
	)
	for {
		var queryBuffer []byte
		var err error
		if firstIteration && initialBuffer != nil {
			queryBuffer = initialBuffer
			firstIteration = false
		} else {
			queryBuffer, err = util.ReadBytes(clientConn)
			if err != nil {
				if !h.IsUsrAppTerminateInitiated() {
					logger.Error("failed to read query from the mysql client", zap.Error(err))
					return nil, err
				}
			}
		}
		if len(queryBuffer) == 0 {
			break
		}
		operation, requestHeader, mysqlRequest, err := DecodeMySQLPacket(bytesToMySQLPacket(queryBuffer), logger, destConn)
		mysqlRequests = append([]models.MySQLRequest{}, models.MySQLRequest{
			Header: &models.MySQLPacketHeader{
				PacketLength: requestHeader.PayloadLength,
				PacketNumber: requestHeader.SequenceID,
				PacketType:   operation,
			},
			Message: mysqlRequest,
		})
		res, err := destConn.Write(queryBuffer)
		if err != nil {
			logger.Error("failed to write query to mysql server", zap.Error(err))
			return nil, err
		}
		if res == 9 {
			return nil, nil
		}
		queryResponse, err := util.ReadBytes(destConn)
		if err != nil {
			logger.Error("failed to read query response from mysql server", zap.Error(err))
			return nil, err
		}
		_, err = clientConn.Write(queryResponse)
		if err != nil {
			logger.Error("failed to write query response to mysql client", zap.Error(err))
			return nil, err
		}
		if len(queryResponse) == 0 {
			break
		}
		responseOperation, responseHeader, mysqlResp, err := DecodeMySQLPacket(bytesToMySQLPacket(queryResponse), logger, destConn)
		if err != nil {
			logger.Error("Failed to decode the MySQL packet from the destination server", zap.Error(err))
			continue
		}
		if len(queryResponse) == 0 || responseOperation == "COM_STMT_CLOSE" {
			break
		}
		mysqlResponses = append([]models.MySQLResponse{}, models.MySQLResponse{
			Header: &models.MySQLPacketHeader{
				PacketLength: responseHeader.PayloadLength,
				PacketNumber: responseHeader.SequenceID,
				PacketType:   responseOperation,
			},
			Message: mysqlResp,
		})
		recordMySQLMessage(h, mysqlRequests, mysqlResponses, operation, responseOperation, "mocks", ctx)
	}
	return nil, nil
}
func recordMySQLMessage(h *hooks.Hook, mysqlRequests []models.MySQLRequest, mysqlResponses []models.MySQLResponse, operation string, responseOperation string, name string, ctx context.Context) {
	shouldRecordCalls := true
	if shouldRecordCalls {
		meta := map[string]string{
			"type":              name,
			"operation":         operation,
			"responseOperation": responseOperation,
		}
		mysqlMock := &models.Mock{
			Version: models.GetVersion(),
			Kind:    models.SQL,
			Name:    "mocks",
			Spec: models.MockSpec{
				Metadata:       meta,
				MySqlRequests:  mysqlRequests,
				MySqlResponses: mysqlResponses,
				Created:        time.Now().Unix(),
			},
		}
		h.AppendMocks(mysqlMock, ctx)
	}
}
func bytesToMySQLPacket(buffer []byte) MySQLPacket {
	if buffer == nil || len(buffer) < 4 {
		log.Fatalf("Error: buffer is nil or too short to be a valid MySQL packet")
		return MySQLPacket{}
	}
	tempBuffer := make([]byte, 4)
	copy(tempBuffer, buffer[:3])
	length := binary.LittleEndian.Uint32(tempBuffer)
	sequenceID := buffer[3]
	payload := buffer[4:]
	return MySQLPacket{
		Header: MySQLPacketHeader{
			PayloadLength: length,
			SequenceID:    sequenceID,
		},
		Payload: payload,
	}
}
