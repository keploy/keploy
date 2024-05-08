package mysql

import (
	"context"
	"errors"
	"net"
	"time"

	"golang.org/x/sync/errgroup"

	pUtil "go.keploy.io/server/v2/pkg/core/proxy/util"
	"go.keploy.io/server/v2/pkg/models"
	"go.keploy.io/server/v2/utils"
	"go.uber.org/zap"
)

func encodeMySQL(ctx context.Context, logger *zap.Logger, clientConn, destConn net.Conn, mocks chan<- *models.Mock, _ models.OutgoingOptions) error {

	var (
		mysqlRequests  []models.MySQLRequest
		mysqlResponses []models.MySQLResponse
	)

	errCh := make(chan error, 1)

	//get the error group from the context
	g, ok := ctx.Value(models.ErrGroupKey).(*errgroup.Group)
	if !ok {
		return errors.New("failed to get the error group from the context")
	}

	//for keeping conn alive
	g.Go(func() error {
		defer pUtil.Recover(logger, clientConn, destConn)
		defer close(errCh)
		for {
			lastCommand[clientConn] = 0x00 //resetting last command for new loop
			data, source, err := readFirstBuffer(ctx, logger, clientConn, destConn)
			if len(data) == 0 {
				break
			}
			if err != nil {
				utils.LogError(logger, err, "failed to read initial data")
				errCh <- err
				return nil
			}
			if source == "destination" {
				handshakeResponseBuffer := data
				_, err = clientConn.Write(handshakeResponseBuffer)
				if err != nil {
					if ctx.Err() != nil {
						return ctx.Err()
					}
					utils.LogError(logger, err, "failed to write handshake response to client")
					errCh <- err
					return nil
				}
				handshakeResponseFromClient, err := pUtil.ReadBytes(ctx, logger, clientConn)
				if err != nil {
					utils.LogError(logger, err, "failed to read handshake response from client")
					errCh <- err
					return nil
				}
				_, err = destConn.Write(handshakeResponseFromClient)
				if err != nil {
					if ctx.Err() != nil {
						return ctx.Err()
					}
					utils.LogError(logger, err, "failed to write handshake response to server")
					errCh <- err
					return nil
				}
				//TODO: why is this sleep here?
				time.Sleep(100 * time.Millisecond)
				okPacket1, err := pUtil.ReadBytes(ctx, logger, destConn)
				if err != nil {
					utils.LogError(logger, err, "failed to read packet from server after handshake")
					errCh <- err
					return nil
				}
				_, err = clientConn.Write(okPacket1)
				if err != nil {
					if ctx.Err() != nil {
						return ctx.Err()
					}
					utils.LogError(logger, err, "failed to write packet to client after handshake")
					errCh <- err
					return nil
				}
				expectingHandshakeResponse = true
				oprRequest, requestHeader, mysqlRequest, err := DecodeMySQLPacket(logger, bytesToMySQLPacket(handshakeResponseFromClient), clientConn, models.MODE_RECORD)
				if err != nil {
					utils.LogError(logger, err, "failed to decode MySQL packet from client")
					errCh <- err
					return nil
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
				oprResponse1, responseHeader1, mysqlResp1, err := DecodeMySQLPacket(logger, bytesToMySQLPacket(handshakeResponseBuffer), clientConn, models.MODE_RECORD)
				if err != nil {
					utils.LogError(logger, err, "failed to decode MySQL packet from destination")
					errCh <- err
					return nil
				}
				mysqlResponses = append(mysqlResponses, models.MySQLResponse{
					Header: &models.MySQLPacketHeader{
						PacketLength: responseHeader1.PayloadLength,
						PacketNumber: responseHeader1.SequenceID,
						PacketType:   oprResponse1,
					},
					Message: mysqlResp1,
				})
				oprResponse2, responseHeader2, mysqlResp2, err := DecodeMySQLPacket(logger, bytesToMySQLPacket(okPacket1), clientConn, models.MODE_RECORD)
				if err != nil {
					utils.LogError(logger, err, "failed to decode MySQL packet from destination")
					errCh <- err
					return nil
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

					authSwitchResponse, err := pUtil.ReadBytes(ctx, logger, clientConn)
					if err != nil {
						utils.LogError(logger, err, "failed to read AuthSwitchResponse from client")
						errCh <- err
						return nil
					}
					_, err = destConn.Write(authSwitchResponse)
					if err != nil {
						if ctx.Err() != nil {
							return ctx.Err()
						}
						utils.LogError(logger, err, "failed to write AuthSwitchResponse to server")
						errCh <- err
						return nil
					}
					serverResponse, err := pUtil.ReadBytes(ctx, logger, destConn)
					if err != nil {
						utils.LogError(logger, err, "failed to read response from server")
						errCh <- err
						return nil
					}
					_, err = clientConn.Write(serverResponse)
					if err != nil {
						if ctx.Err() != nil {
							return ctx.Err()
						}
						utils.LogError(logger, err, "failed to write response to client")
						errCh <- err
						return nil
					}
					expectingAuthSwitchResponse = true

					oprRequestFinal, requestHeaderFinal, mysqlRequestFinal, err := DecodeMySQLPacket(logger, bytesToMySQLPacket(authSwitchResponse), clientConn, models.MODE_RECORD)
					if err != nil {
						utils.LogError(logger, err, "failed to decode MySQL packet from client after full authentication")
						errCh <- err
						return nil
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
					oprResponse, responseHeader, mysqlResp, err := DecodeMySQLPacket(logger, bytesToMySQLPacket(serverResponse), clientConn, models.MODE_RECORD)
					isPluginData = false
					if err != nil {
						utils.LogError(logger, err, "failed to decode MySQL packet from destination after full authentication")
						errCh <- err
						return nil
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

						clientResponse, err := pUtil.ReadBytes(ctx, logger, clientConn)
						if err != nil {
							utils.LogError(logger, err, "failed to read response from client")
							errCh <- err
							return nil
						}
						_, err = destConn.Write(clientResponse)
						if err != nil {
							if ctx.Err() != nil {
								return ctx.Err()
							}
							utils.LogError(logger, err, "failed to write client's response to server")
							errCh <- err
							return nil
						}
						finalServerResponse, err := pUtil.ReadBytes(ctx, logger, destConn)
						if err != nil {
							utils.LogError(logger, err, "failed to read final response from server")
							errCh <- err
							return nil
						}
						_, err = clientConn.Write(finalServerResponse)
						if err != nil {
							if ctx.Err() != nil {
								return ctx.Err()
							}
							utils.LogError(logger, err, "failed to write final response to client")
							errCh <- err
							return nil
						}
						oprRequestFinal, requestHeaderFinal, mysqlRequestFinal, err := DecodeMySQLPacket(logger, bytesToMySQLPacket(clientResponse), clientConn, models.MODE_RECORD)
						if err != nil {
							utils.LogError(logger, err, "failed to decode MySQL packet from client after full authentication")
							errCh <- err
							return nil
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
						oprResponseFinal, responseHeaderFinal, mysqlRespFinal, err := DecodeMySQLPacket(logger, bytesToMySQLPacket(finalServerResponse), clientConn, models.MODE_RECORD)
						isPluginData = false
						if err != nil {
							utils.LogError(logger, err, "failed to decode MySQL packet from destination after full authentication")
							errCh <- err
							return nil
						}
						mysqlResponses = append(mysqlResponses, models.MySQLResponse{
							Header: &models.MySQLPacketHeader{
								PacketLength: responseHeaderFinal.PayloadLength,
								PacketNumber: responseHeaderFinal.SequenceID,
								PacketType:   oprResponseFinal,
							},
							Message: mysqlRespFinal,
						})
						clientResponse1, err := pUtil.ReadBytes(ctx, logger, clientConn)
						if err != nil {
							utils.LogError(logger, err, "failed to read response from client")
							errCh <- err
							return nil
						}
						_, err = destConn.Write(clientResponse1)
						if err != nil {
							if ctx.Err() != nil {
								return ctx.Err()
							}
							utils.LogError(logger, err, "failed to write client's response to server")
							errCh <- err
							return nil
						}
						finalServerResponse1, err := pUtil.ReadBytes(ctx, logger, destConn)
						if err != nil {
							utils.LogError(logger, err, "failed to read final response from server")
							errCh <- err
							return nil
						}
						_, err = clientConn.Write(finalServerResponse1)
						if err != nil {
							if ctx.Err() != nil {
								return ctx.Err()
							}
							utils.LogError(logger, err, "failed to write final response to client")
							errCh <- err
							return nil
						}
						finalServerResponsetype1, finalServerResponseHeader1, mysqlRespfinalServerResponse, err := DecodeMySQLPacket(logger, bytesToMySQLPacket(finalServerResponse1), clientConn, models.MODE_RECORD)
						if err != nil {
							utils.LogError(logger, err, "failed to decode MySQL packet from final server response")
							errCh <- err
							return nil
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
							utils.LogError(logger, err, "failed to decode MySQL packet from client after full authentication")
							errCh <- err
							return nil
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
						finalServerResponse, err := pUtil.ReadBytes(ctx, logger, destConn)
						if err != nil {
							utils.LogError(logger, err, "failed to read final response from server")
							errCh <- err
							return nil
						}
						_, err = clientConn.Write(finalServerResponse)
						if err != nil {
							if ctx.Err() != nil {
								return ctx.Err()
							}
							utils.LogError(logger, err, "failed to write final response to client")
							errCh <- err
							return nil
						}
						oprResponseFinal, responseHeaderFinal, mysqlRespFinal, err := DecodeMySQLPacket(logger, bytesToMySQLPacket(finalServerResponse), clientConn, models.MODE_RECORD)
						isPluginData = false
						if err != nil {
							utils.LogError(logger, err, "failed to decode MySQL packet from destination after full authentication")
							errCh <- err
							return nil
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

					clientResponse, err := pUtil.ReadBytes(ctx, logger, clientConn)
					if err != nil {
						utils.LogError(logger, err, "failed to read response from client")
						errCh <- err
						return nil
					}
					_, err = destConn.Write(clientResponse)
					if err != nil {
						if ctx.Err() != nil {
							return ctx.Err()
						}
						utils.LogError(logger, err, "failed to write client's response to server")
						errCh <- err
						return nil
					}
					finalServerResponse, err := pUtil.ReadBytes(ctx, logger, destConn)
					if err != nil {
						utils.LogError(logger, err, "failed to read final response from server")
						errCh <- err
						return nil
					}
					_, err = clientConn.Write(finalServerResponse)
					if err != nil {
						if ctx.Err() != nil {
							return ctx.Err()
						}
						utils.LogError(logger, err, "failed to write final response to client")
						errCh <- err
						return nil
					}
					oprRequestFinal, requestHeaderFinal, mysqlRequestFinal, err := DecodeMySQLPacket(logger, bytesToMySQLPacket(clientResponse), clientConn, models.MODE_RECORD)
					if err != nil {
						utils.LogError(logger, err, "failed to decode MySQL packet from client after full authentication")
						errCh <- err
						return nil
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
					oprResponseFinal, responseHeaderFinal, mysqlRespFinal, err := DecodeMySQLPacket(logger, bytesToMySQLPacket(finalServerResponse), clientConn, models.MODE_RECORD)
					isPluginData = false
					if err != nil {
						utils.LogError(logger, err, "failed to decode MySQL packet from destination after full authentication")
						errCh <- err
						return nil
					}
					mysqlResponses = append(mysqlResponses, models.MySQLResponse{
						Header: &models.MySQLPacketHeader{
							PacketLength: responseHeaderFinal.PayloadLength,
							PacketNumber: responseHeaderFinal.SequenceID,
							PacketType:   oprResponseFinal,
						},
						Message: mysqlRespFinal,
					})
					clientResponse1, err := pUtil.ReadBytes(ctx, logger, clientConn)
					if err != nil {
						utils.LogError(logger, err, "failed to read response from client")
						errCh <- err
						return nil
					}
					_, err = destConn.Write(clientResponse1)
					if err != nil {
						if ctx.Err() != nil {
							return ctx.Err()
						}
						utils.LogError(logger, err, "failed to write client's response to server")
						errCh <- err
						return nil
					}
					finalServerResponse1, err := pUtil.ReadBytes(ctx, logger, destConn)
					if err != nil {
						utils.LogError(logger, err, "failed to read final response from server")
						errCh <- err
						return nil
					}
					_, err = clientConn.Write(finalServerResponse1)
					if err != nil {
						if ctx.Err() != nil {
							return ctx.Err()
						}
						utils.LogError(logger, err, "failed to write final response to client")
						errCh <- err
						return nil
					}
					finalServerResponsetype1, finalServerResponseHeader1, mysqlRespfinalServerResponse, err := DecodeMySQLPacket(logger, bytesToMySQLPacket(finalServerResponse1), clientConn, models.MODE_RECORD)
					if err != nil {
						utils.LogError(logger, err, "failed to decode MySQL packet from final server response")
						errCh <- err
						return nil
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
						utils.LogError(logger, err, "failed to decode MySQL packet from client after full authentication")
						errCh <- err
						return nil
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
					utils.LogError(logger, err, "failed to handle client queries")
					errCh <- err
					return nil
				}
			} else if source == "client" {
				err := handleClientQueries(ctx, logger, nil, clientConn, destConn, mocks)
				if err != nil {
					utils.LogError(logger, err, "failed to handle client queries")
					errCh <- err
					return nil
				}
			}
		}
		return nil
	})

	select {
	case <-ctx.Done():
		return ctx.Err()
	case err := <-errCh:
		return err
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
				queryBuffer, err = pUtil.ReadBytes(ctx, logger, clientConn)
				if err != nil {
					utils.LogError(logger, err, "failed to read query from the mysql client")
					return err
				}
			}
			if len(queryBuffer) == 0 {
				break
			}
			operation, requestHeader, mysqlRequest, err := DecodeMySQLPacket(logger, bytesToMySQLPacket(queryBuffer), clientConn, models.MODE_RECORD)
			if err != nil {
				utils.LogError(logger, err, "failed to decode the MySQL packet from the client")
				return err
			}
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
				if ctx.Err() != nil {
					return ctx.Err()
				}
				utils.LogError(logger, err, "failed to write query to mysql server")
				return err
			}
			if res == 9 {
				return nil
			}
			queryResponse, err := pUtil.ReadBytes(ctx, logger, destConn)
			if err != nil {
				utils.LogError(logger, err, "failed to read query response from mysql server")
				return err
			}
			_, err = clientConn.Write(queryResponse)
			if err != nil {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				utils.LogError(logger, err, "failed to write query response to mysql client")
				return err
			}
			if len(queryResponse) == 0 {
				break
			}
			responseOperation, responseHeader, mysqlResp, err := DecodeMySQLPacket(logger, bytesToMySQLPacket(queryResponse), clientConn, models.MODE_RECORD)
			if err != nil {
				utils.LogError(logger, err, "failed to decode the MySQL packet from the destination server")
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
