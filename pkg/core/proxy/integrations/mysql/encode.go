package mysql

import (
	"context"
	"go.keploy.io/server/v2/pkg/core/proxy/util"
	"go.keploy.io/server/v2/pkg/models"
	"go.uber.org/zap"
	"net"
	"time"
)

func encodeMySql(ctx context.Context, logger *zap.Logger, clientConn, destConn net.Conn, mocks chan<- *models.Mock, opts models.OutgoingOptions) error {
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
