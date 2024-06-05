package mysql

import (
	"context"
	"encoding/base64"
	"errors"
	"io"
	"net"
	"fmt"
	"time"
	"unicode"

	"golang.org/x/sync/errgroup"

	pUtil "go.keploy.io/server/v2/pkg/core/proxy/util"
	"go.keploy.io/server/v2/pkg/models"
	"go.keploy.io/server/v2/utils"
	"go.uber.org/zap"
)

var reqTimestampMock, responseTimestampMock time.Time

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
					logger.Debug("Getting the error at line 62")
					if err == io.EOF {
						logger.Debug("recieved request buffer is empty in record mode for mysql call")
						errCh <- err
						return nil
					}
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
					logger.Debug("Getting the error at line 85")
					if err == io.EOF {
						logger.Debug("recieved request buffer is empty in record mode for mysql call")
						errCh <- err
						return nil
					}
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

				recordMySQLMessage(ctx, mysqlRequests, mysqlResponses, "config", oprRequest, oprResponse2, mocks, reqTimestampMock, responseTimestampMock)
				mysqlRequests = []models.MySQLRequest{}
				mysqlResponses = []models.MySQLResponse{}
				err = handleClientQueries(ctx, logger, nil, clientConn, destConn, mocks, errCh)
				logger.Debug("we got the error here at line 517", zap.Any("err", err))
				if err != nil {
					if err == io.EOF {
						logger.Debug("recieved request buffer is empty in record mode for mysql call")
						errCh <- err
						return nil
					}
					utils.LogError(logger, err, "failed to handle client queries")
					errCh <- err
					return nil
				}
			} else if source == "client" {
				err := handleClientQueries(ctx, logger, nil, clientConn, destConn, mocks, errCh)
				logger.Debug("we got the error here at line 529", zap.Any("err", err))
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
		if err == io.EOF {
			return nil
		}
		return err
	}

}

func handleClientQueries(ctx context.Context, logger *zap.Logger, initialBuffer []byte, clientConn, destConn net.Conn, mocks chan<- *models.Mock, errChan chan error) error {
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
				reqTimestampMock = time.Now()
				if err != nil {
					if err == io.EOF {
						logger.Debug("Received request buffer is empty in record mode for mysql call")
						errChan <- err
						return nil
					}
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
			responseTimestampMock = time.Now()
			if res == 9 {
				return nil
			}
			queryResponse, err := pUtil.ReadBytes(ctx, logger, destConn)
			if err != nil {
				if err == io.EOF {
					logger.Debug("received request buffer is empty in record mode for mysql call on line 593")
					errChan <- err
					return nil
				}
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
			mysqlPacketHeader := &models.MySQLPacketHeader{
				PacketLength: responseHeader.PayloadLength,
				PacketNumber: responseHeader.SequenceID,
				PacketType:   responseOperation,
			}
			responseBin, err := encodeToBinary(mysqlResp, mysqlPacketHeader, mysqlPacketHeader.PacketType, 1)
			fmt.Println("This is the original response and the mock response", string(queryResponse),"\n\n\n", string(responseBin))
			if err != nil {
				utils.LogError(logger, err, "failed to encode the MySQL packet.")
			}
			equal, _ := compareByteSlices(responseBin, queryResponse)
			var payload string
			if !equal {
				payload = base64.StdEncoding.EncodeToString(queryResponse)
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
				Payload: payload,
				Message: mysqlResp,
			})
			recordMySQLMessage(ctx, mysqlRequests, mysqlResponses, "mocks", operation, responseOperation, mocks, reqTimestampMock, responseTimestampMock)
		}
	}
}

func compareByteSlices(a, b []byte) (bool, string) {
	if len(a) != len(b) {
		return false, fmt.Sprintf("Lengths are different: len(a)=%d, len(b)=%d", len(a), len(b))
	}

	differences := ""
	same := true
	for i := range a {
		if a[i] != b[i] {
			same = false
			differences += fmt.Sprintf("Difference at index %d: a=%d (%q), b=%d (%q)\n", i, a[i], printableChar(a[i]), b[i], printableChar(b[i]))
		}
	}

	if same {
		return true, "Slices are equal"
	} else {
		return false, differences
	}
}
func printableChar(b byte) string {
	if unicode.IsPrint(rune(b)) {
		return fmt.Sprintf("%q", rune(b))
	}
	return fmt.Sprintf("non-printable (0x%X)", b)
}
