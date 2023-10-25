package mysqlparser

import (
	"context"
	"encoding/binary"
	"log"
	"net"
	"time"

	"go.keploy.io/server/pkg/hooks"
	"go.keploy.io/server/pkg/models"
	"go.keploy.io/server/pkg/proxy/util"
	"go.uber.org/zap"
)

func IsOutgoingMySQL(buffer []byte) bool {
	if len(buffer) < 5 {
		return false
	}
	packetLength := uint32(buffer[0]) | uint32(buffer[1])<<8 | uint32(buffer[2])<<16
	return int(packetLength) == len(buffer)-4
}
func ProcessOutgoingMySql(clientConnId int64, destConnId int64, requestBuffer []byte, clientConn, destConn net.Conn, h *hooks.Hook, started time.Time, readRequestDelay time.Duration, logger *zap.Logger, ctx context.Context) {
	switch models.GetMode() {
	case models.MODE_RECORD:
		encodeOutgoingMySql(clientConnId, destConnId, requestBuffer, clientConn, destConn, h, started, readRequestDelay, logger, ctx)
	case models.MODE_TEST:
		decodeOutgoingMySQL(clientConnId, destConnId, requestBuffer, clientConn, destConn, h, started, readRequestDelay, logger, ctx)
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

func encodeOutgoingMySql(clientConnId int64, destConnId int64, requestBuffer []byte, clientConn, destConn net.Conn, h *hooks.Hook, started time.Time, readRequestDelay time.Duration, logger *zap.Logger, ctx context.Context) {
	var (
		mysqlRequests  = []models.MySQLRequest{}
		mysqlResponses = []models.MySQLResponse{}
	)
	for {
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

func decodeOutgoingMySQL(clientConnId int64, destConnId int64, requestBuffer []byte, clientConn, destConn net.Conn, h *hooks.Hook, started time.Time, readRequestDelay time.Duration, logger *zap.Logger, ctx context.Context) {
	firstLoop := true
	doHandshakeAgain := false
	configResponseRead := 0
	for {
		configMocks := h.GetConfigMocks()
		tcsMocks := h.GetTcsMocks()
		logger.Debug("Config and TCS Mocks", zap.Any("configMocks", configMocks), zap.Any("tcsMocks", tcsMocks))
		var (
			mysqlRequests = []models.MySQLRequest{}
		)
		logger.Debug("MySQL requests", zap.Any("mysqlRequests", mysqlRequests))
		if firstLoop || doHandshakeAgain {
			header := configMocks[0].Spec.MySqlResponses[configResponseRead].Header
			packet := configMocks[0].Spec.MySqlResponses[configResponseRead].Message
			opr := configMocks[0].Spec.MySqlResponses[configResponseRead].Header.PacketType
			binaryPacket, err := encodeToBinary(&packet, header, opr, 0)
			if err != nil {
				logger.Error("Failed to encode to binary", zap.Error(err))
				return
			}
			_, err = clientConn.Write(binaryPacket)
			configResponseRead++
			requestBuffer, err = util.ReadBytes(clientConn)
			// oprRequest, requestHeader, mysqlRequest, err := DecodeMySQLPacket(bytesToMySQLPacket(requestBuffer), logger, destConn)
			header = configMocks[0].Spec.MySqlResponses[configResponseRead].Header
			handshakeResponseFromConfig := configMocks[0].Spec.MySqlResponses[configResponseRead].Message
			opr2 := configMocks[0].Spec.MySqlResponses[configResponseRead].Header.PacketType
			handshakeResponseBinary, err := encodeToBinary(&handshakeResponseFromConfig, header, opr2, 1)
			// _, err = destConn.Write(requestBuffer)
			//fmt.Println(oprRequest, requestHeader, mysqlRequest, handshakeResponseFromConfig, err1)
			_, err = clientConn.Write(handshakeResponseBinary)

			if doHandshakeAgain && (configResponseRead == len(configMocks[0].Spec.MySqlResponses)) {
				doHandshakeAgain = false
			} else {
				if opr2 == "AUTH_SWITCH_REQUEST" {
					configResponseRead++
					//Private Key
					requestBuffer, err = util.ReadBytes(clientConn)
					header = configMocks[0].Spec.MySqlResponses[configResponseRead].Header
					handshakeResponseFromConfig := configMocks[0].Spec.MySqlResponses[configResponseRead].Message
					opr2 := configMocks[0].Spec.MySqlResponses[configResponseRead].Header.PacketType
					encodedResponseBinary, _ := encodeToBinary(&handshakeResponseFromConfig, header, opr2, 1)
					_, err = clientConn.Write(encodedResponseBinary)
				}
				if doHandshakeAgain && (configResponseRead == len(configMocks[0].Spec.MySqlResponses)) {
					doHandshakeAgain = false
				} else {
					configResponseRead++
					//Private Key
					requestBuffer, err = util.ReadBytes(clientConn)
					header := configMocks[0].Spec.MySqlResponses[configResponseRead].Header
					handshakeResponseFromConfig := configMocks[0].Spec.MySqlResponses[configResponseRead].Message
					opr2 := configMocks[0].Spec.MySqlResponses[configResponseRead].Header.PacketType
					encodedResponseBinary, _ := encodeToBinary(&handshakeResponseFromConfig, header, opr2, 1)
					_, err = clientConn.Write(encodedResponseBinary)
					configResponseRead++
					//Encrypted Password
					requestBuffer, err = util.ReadBytes(clientConn)
					ResponseFromConfigNext := configMocks[0].Spec.MySqlResponses[configResponseRead].Message
					opr3 := configMocks[0].Spec.MySqlResponses[configResponseRead].Header.PacketType
					encodedResponseBinary, _ = encodeMySQLOKConnectionPhase(&ResponseFromConfigNext, opr3, 6)
					_, err = clientConn.Write(encodedResponseBinary)
				}

			}
			if err != nil {
				logger.Error("failed to write query response to mysql client", zap.Error(err))
				return
			}
		} else {
			requestBuffer, _ = util.ReadBytes(clientConn)
			if len(requestBuffer) == 0 {
				return
			}
			oprRequest, requestHeader, mysqlRequest, err := DecodeMySQLPacket(bytesToMySQLPacket(requestBuffer), logger, destConn)
			if oprRequest == "COM_STMT_CLOSE" {
				if len(tcsMocks) == mockResponseRead {
					mockResponseRead = 0
				}
				return
			}
			logger.Debug("Decoded MySQL packet details",
				zap.String("oprRequest", oprRequest),
				zap.Any("requestHeader", requestHeader),
				zap.Any("mysqlRequest", mysqlRequest),
				zap.Error(err))
			if mockResponseRead >= len(tcsMocks) {
				logger.Error("Mock response reading pointer out of bounds")
				return
			}
			header := tcsMocks[mockResponseRead].Spec.MySqlResponses[0].Header
			handshakeResponseFromConfig := tcsMocks[mockResponseRead].Spec.MySqlResponses[0].Message
			opr2 := tcsMocks[mockResponseRead].Spec.MySqlResponses[0].Header.PacketType
			responseBinary, err := encodeToBinary(&handshakeResponseFromConfig, header, opr2, mockResponseRead+1)
			_, err = clientConn.Write(responseBinary)
			mockResponseRead++
			time.Sleep(1000 * time.Millisecond)
		}
		firstLoop = false
	}
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
				logger.Error("failed to read query from the mysql client", zap.Error(err))
				return nil, err
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
			Version: models.V1Beta2,
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
