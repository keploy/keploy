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
	//closing the destination conn
	defer func(destConn net.Conn) {
		err := destConn.Close()
		if err != nil {
			logger.Error("failed to close the destination connection", zap.Error(err))
		}
	}(destConn)

	var (
		mysqlRequests  []models.MySQLRequest
		mysqlResponses []models.MySQLResponse
	)
	for {
		select {
		case <-ctx.Done():
			return nil
		default:
			lastCommand = 0x00 //resetting last command for new loop
			data, source, err := readFirstBuffer(ctx, clientConn, destConn)
			if len(data) == 0 {
				break
			}
			if err != nil {
				logger.Error("failed to read initial data", zap.Error(err))
				return err
			}
			if source == "destination" {
				handshakeResponseBuffer := data
				_, err = clientConn.Write(handshakeResponseBuffer)
				if err != nil {
					logger.Error("failed to write handshake request to client", zap.Error(err))
					return err
				}
				handshakeResponseFromClient, err := util.ReadBytes(ctx, clientConn)
				if err != nil {
					logger.Error("failed to read handshake response from client", zap.Error(err))
					return err
				}
				_, err = destConn.Write(handshakeResponseFromClient)
				if err != nil {
					logger.Error("failed to write handshake response to server", zap.Error(err))
					return err
				}
				//TODO: why is this sleep here?
				time.Sleep(100 * time.Millisecond)
				okPacket1, err := util.ReadBytes(ctx, destConn)
				if err != nil {
					logger.Error("failed to read packet from server after handshake", zap.Error(err))
					return err
				}
				_, err = clientConn.Write(okPacket1)
				if err != nil {
					logger.Error("failed to write auth switch request to client", zap.Error(err))
					return err
				}
				expectingHandshakeResponse = true
				oprRequest, requestHeader, mysqlRequest, err := DecodeMySQLPacket(logger, bytesToMySQLPacket(handshakeResponseFromClient))
				if err != nil {
					logger.Error("failed to decode MySQL packet from client", zap.Error(err))
					return err
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
				oprResponse1, responseHeader1, mysqlResp1, err := DecodeMySQLPacket(logger, bytesToMySQLPacket(handshakeResponseBuffer))
				if err != nil {
					logger.Error("failed to decode MySQL packet from destination", zap.Error(err))
					return err
				}
				mysqlResponses = append(mysqlResponses, models.MySQLResponse{
					Header: &models.MySQLPacketHeader{
						PacketLength: responseHeader1.PayloadLength,
						PacketNumber: responseHeader1.SequenceID,
						PacketType:   oprResponse1,
					},
					Message: mysqlResp1,
				})
				oprResponse2, responseHeader2, mysqlResp2, err := DecodeMySQLPacket(logger, bytesToMySQLPacket(okPacket1))
				if err != nil {
					logger.Error("failed to decode MySQL packet from OK packet", zap.Error(err))
					return err
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

					authSwitchResponse, err := util.ReadBytes(ctx, clientConn)
					if err != nil {
						logger.Error("failed to read AuthSwitchResponse from client", zap.Error(err))
						return err
					}
					_, err = destConn.Write(authSwitchResponse)
					if err != nil {
						logger.Error("failed to write AuthSwitchResponse to server", zap.Error(err))
						return err
					}
					serverResponse, err := util.ReadBytes(ctx, destConn)
					if err != nil {
						logger.Error("failed to read final response from server", zap.Error(err))
						return err
					}
					_, err = clientConn.Write(serverResponse)
					if err != nil {
						logger.Error("failed to write final response to client", zap.Error(err))
						return err
					}
					expectingAuthSwitchResponse = true

					oprRequestFinal, requestHeaderFinal, mysqlRequestFinal, err := DecodeMySQLPacket(logger, bytesToMySQLPacket(authSwitchResponse))
					if err != nil {
						logger.Error("failed to decode MySQL packet from client after full authentication", zap.Error(err))
						return err
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
					oprResponse, responseHeader, mysqlResp, err := DecodeMySQLPacket(logger, bytesToMySQLPacket(serverResponse))
					isPluginData = false
					if err != nil {
						logger.Error("failed to decode MySQL packet from destination after full authentication", zap.Error(err))
						return err
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

						clientResponse, err := util.ReadBytes(ctx, clientConn)
						if err != nil {
							logger.Error("failed to read response from client", zap.Error(err))
							return err
						}
						_, err = destConn.Write(clientResponse)
						if err != nil {
							logger.Error("failed to write client's response to server", zap.Error(err))
							return err
						}
						finalServerResponse, err := util.ReadBytes(ctx, destConn)
						if err != nil {
							logger.Error("failed to read final response from server", zap.Error(err))
							return err
						}
						_, err = clientConn.Write(finalServerResponse)
						if err != nil {
							logger.Error("failed to write final response to client", zap.Error(err))
							return err
						}
						oprRequestFinal, requestHeaderFinal, mysqlRequestFinal, err := DecodeMySQLPacket(logger, bytesToMySQLPacket(clientResponse))
						if err != nil {
							logger.Error("failed to decode MySQL packet from client after full authentication", zap.Error(err))
							return err
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
						oprResponseFinal, responseHeaderFinal, mysqlRespFinal, err := DecodeMySQLPacket(logger, bytesToMySQLPacket(finalServerResponse))
						isPluginData = false
						if err != nil {
							logger.Error("failed to decode MySQL packet from destination after full authentication", zap.Error(err))
							return err
						}
						mysqlResponses = append(mysqlResponses, models.MySQLResponse{
							Header: &models.MySQLPacketHeader{
								PacketLength: responseHeaderFinal.PayloadLength,
								PacketNumber: responseHeaderFinal.SequenceID,
								PacketType:   oprResponseFinal,
							},
							Message: mysqlRespFinal,
						})
						clientResponse1, err := util.ReadBytes(ctx, clientConn)
						if err != nil {
							logger.Error("failed to read response from client", zap.Error(err))
							return err
						}
						_, err = destConn.Write(clientResponse1)
						if err != nil {
							logger.Error("failed to write client's response to server", zap.Error(err))
							return err
						}
						finalServerResponse1, err := util.ReadBytes(ctx, destConn)
						if err != nil {
							logger.Error("failed to read final response from server", zap.Error(err))
							return err
						}
						_, err = clientConn.Write(finalServerResponse1)
						if err != nil {
							logger.Error("failed to write final response to client", zap.Error(err))
							return err
						}
						finalServerResponsetype1, finalServerResponseHeader1, mysqlRespfinalServerResponse, err := DecodeMySQLPacket(logger, bytesToMySQLPacket(finalServerResponse1))
						if err != nil {
							logger.Error("failed to decode MySQL packet from final server response", zap.Error(err))
							return err
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
							return err
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
						finalServerResponse, err := util.ReadBytes(ctx, destConn)
						if err != nil {
							logger.Error("failed to read final response from server", zap.Error(err))
							return err
						}
						_, err = clientConn.Write(finalServerResponse)
						if err != nil {
							logger.Error("failed to write final response to client", zap.Error(err))
							return err
						}
						oprResponseFinal, responseHeaderFinal, mysqlRespFinal, err := DecodeMySQLPacket(logger, bytesToMySQLPacket(finalServerResponse))
						isPluginData = false
						if err != nil {
							logger.Error("failed to decode MySQL packet from destination after full authentication", zap.Error(err))
							return err
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

					clientResponse, err := util.ReadBytes(ctx, clientConn)
					if err != nil {
						logger.Error("failed to read response from client", zap.Error(err))
						return err
					}
					_, err = destConn.Write(clientResponse)
					if err != nil {
						logger.Error("failed to write client's response to server", zap.Error(err))
						return err
					}
					finalServerResponse, err := util.ReadBytes(ctx, destConn)
					if err != nil {
						logger.Error("failed to read final response from server", zap.Error(err))
						return err
					}
					_, err = clientConn.Write(finalServerResponse)
					if err != nil {
						logger.Error("failed to write final response to client", zap.Error(err))
						return err
					}
					oprRequestFinal, requestHeaderFinal, mysqlRequestFinal, err := DecodeMySQLPacket(logger, bytesToMySQLPacket(clientResponse))
					if err != nil {
						logger.Error("failed to decode MySQL packet from client after full authentication", zap.Error(err))
						return err
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
					oprResponseFinal, responseHeaderFinal, mysqlRespFinal, err := DecodeMySQLPacket(logger, bytesToMySQLPacket(finalServerResponse))
					isPluginData = false
					if err != nil {
						logger.Error("failed to decode MySQL packet from destination after full authentication", zap.Error(err))
						return err
					}
					mysqlResponses = append(mysqlResponses, models.MySQLResponse{
						Header: &models.MySQLPacketHeader{
							PacketLength: responseHeaderFinal.PayloadLength,
							PacketNumber: responseHeaderFinal.SequenceID,
							PacketType:   oprResponseFinal,
						},
						Message: mysqlRespFinal,
					})
					clientResponse1, err := util.ReadBytes(ctx, clientConn)
					if err != nil {
						logger.Error("failed to read response from client", zap.Error(err))
						return err
					}
					_, err = destConn.Write(clientResponse1)
					if err != nil {
						logger.Error("failed to write client's response to server", zap.Error(err))
						return err
					}
					finalServerResponse1, err := util.ReadBytes(ctx, destConn)
					if err != nil {
						logger.Error("failed to read final response from server", zap.Error(err))
						return err
					}
					_, err = clientConn.Write(finalServerResponse1)
					if err != nil {
						logger.Error("failed to write final response to client", zap.Error(err))
						return err
					}
					finalServerResponsetype1, finalServerResponseHeader1, mysqlRespfinalServerResponse, err := DecodeMySQLPacket(logger, bytesToMySQLPacket(finalServerResponse1))
					if err != nil {
						logger.Error("failed to decode MySQL packet from final server response", zap.Error(err))
						return err
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
						return err
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

				recordMySQLMessage(ctx, mysqlRequests, mysqlResponses, "config", oprRequest, oprResponse2, mocks)
				mysqlRequests = []models.MySQLRequest{}
				mysqlResponses = []models.MySQLResponse{}
				err = handleClientQueries(ctx, logger, nil, clientConn, destConn, mocks)
				if err != nil {
					logger.Error("failed to handle client queries", zap.Error(err))
					return err
				}
			} else if source == "client" {
				err := handleClientQueries(ctx, logger, nil, clientConn, destConn, mocks)
				if err != nil {
					logger.Error("failed to handle client queries", zap.Error(err))
					return err
				}
			}
		}
	}
}

func handleClientQueries(ctx context.Context, logger *zap.Logger, initialBuffer []byte, clientConn, destConn net.Conn, mocks chan<- *models.Mock) error {
	firstIteration := true
	var (
		mysqlRequests  []models.MySQLRequest
		mysqlResponses []models.MySQLResponse
	)
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
			var queryBuffer []byte
			var err error
			if firstIteration && initialBuffer != nil {
				queryBuffer = initialBuffer
				firstIteration = false
			} else {
				queryBuffer, err = util.ReadBytes(ctx, clientConn)
				if err != nil {
					logger.Error("failed to read query from the mysql client", zap.Error(err))
					return err
				}
			}
			if len(queryBuffer) == 0 {
				break
			}
			operation, requestHeader, mysqlRequest, err := DecodeMySQLPacket(logger, bytesToMySQLPacket(queryBuffer))
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
				return err
			}
			if res == 9 {
				return nil
			}
			queryResponse, err := util.ReadBytes(ctx, destConn)
			if err != nil {
				logger.Error("failed to read query response from mysql server", zap.Error(err))
				return err
			}
			_, err = clientConn.Write(queryResponse)
			if err != nil {
				logger.Error("failed to write query response to mysql client", zap.Error(err))
				return err
			}
			if len(queryResponse) == 0 {
				break
			}
			responseOperation, responseHeader, mysqlResp, err := DecodeMySQLPacket(logger, bytesToMySQLPacket(queryResponse))
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
			recordMySQLMessage(ctx, mysqlRequests, mysqlResponses, "mocks", operation, responseOperation, mocks)
		}
	}
}
