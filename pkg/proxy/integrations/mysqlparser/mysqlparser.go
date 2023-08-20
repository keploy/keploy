package mysqlparser

import (
	"encoding/binary"
	"fmt"
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

func ProcessOutgoingMySql(clientConnId, destConnId int, requestBuffer []byte, clientConn, destConn net.Conn, h *hooks.Hook, started time.Time, readRequestDelay time.Duration, logger *zap.Logger) {
	// fmt.Println("into processing mongo. clientConnId: ", clientConnId)
	switch models.GetMode() {
	case models.MODE_RECORD:
		// capturedDeps := encodeOutgoingMongo(requestBuffer, clientConn, destConn, logger)
		encodeOutgoingMySql(clientConnId, destConnId, requestBuffer, clientConn, destConn, h, started, readRequestDelay, logger)

		// *deps = append(*deps, capturedDeps...)
		// for _, v := range capturedDeps {
		// 	h.AppendDeps(v)
		// 	// h.WriteMock(v)
		// }
	case models.MODE_TEST:
		fmt.Println("into test mode. clientConnId: ", clientConnId)
		decodeOutgoingMySQL(clientConnId, destConnId, requestBuffer, clientConn, destConn, h, started, readRequestDelay, logger)
	default:
	}
}

var (
	isConfigRecorded = false
)

func encodeOutgoingMySql(clientConnId, destConnId int, requestBuffer []byte, clientConn, destConn net.Conn, h *hooks.Hook, started time.Time, readRequestDelay time.Duration, logger *zap.Logger) {
	var (
		mysqlRequests  = []models.MySQLRequest{}
		mysqlResponses = []models.MySQLResponse{}
	)
	for {
		data, source, err := ReadFirstBuffer(clientConn, destConn)
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
			time.Sleep(1000 * time.Millisecond)
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

			oprRequest, requestHeader, mysqlRequest, err := DecodeMySQLPacket(bytesToMySQLPacket(handshakeResponseFromClient), logger, destConn)
			mysqlRequests = append(mysqlRequests, models.MySQLRequest{
				Header: &models.MySQLPacketHeader{
					PacketLength: requestHeader.PayloadLength,
					PacketNumber: requestHeader.SequenceID,
					PacketType:   oprRequest,
				},
				Message: mysqlRequest,
			})

			oprResponse1, responseHeader1, mysqlResp1, err := DecodeMySQLPacket(bytesToMySQLPacket(handshakeResponseBuffer), logger, destConn)
			mysqlResponses = append(mysqlResponses, models.MySQLResponse{
				Header: &models.MySQLPacketHeader{
					PacketLength: responseHeader1.PayloadLength,
					PacketNumber: responseHeader1.SequenceID,
					PacketType:   oprResponse1,
				},
				Message: mysqlResp1,
			})

			if oprResponse1 == "AuthSwitchRequest" {

				// Reading client's response to the auth switch request
				clientResponse, err := util.ReadBytes(clientConn)
				if err != nil {
					logger.Error("failed to read response from client", zap.Error(err))
					return
				}

				// Forwarding client's response to the server
				_, err = destConn.Write(clientResponse)
				if err != nil {
					logger.Error("failed to write client's response to server", zap.Error(err))
					return
				}

				// Reading server's final response
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

				oprResponse, responseHeader, mysqlResp, err := DecodeMySQLPacket(bytesToMySQLPacket(finalServerResponse), logger, destConn)
				mysqlResponses = append(mysqlResponses, models.MySQLResponse{
					Header: &models.MySQLPacketHeader{
						PacketLength: responseHeader.PayloadLength,
						PacketNumber: responseHeader.SequenceID,
						PacketType:   oprResponse,
					},
					Message: mysqlResp,
				})
			}

			oprResponse2, responseHeader2, mysqlResp2, err := DecodeMySQLPacket(bytesToMySQLPacket(okPacket1), logger, destConn)
			mysqlResponses = append(mysqlResponses, models.MySQLResponse{
				Header: &models.MySQLPacketHeader{
					PacketLength: responseHeader2.PayloadLength,
					PacketNumber: responseHeader2.SequenceID,
					PacketType:   oprResponse2,
				},
				Message: mysqlResp2,
			})
			if !isConfigRecorded {
				recordMySQLMessage(h, mysqlRequests, mysqlResponses, oprRequest, oprResponse2, "config")
				isConfigRecorded = true
			}
			handleClientQueries(h, nil, clientConn, destConn, logger)

		} else if source == "client" {
			handleClientQueries(h, nil, clientConn, destConn, logger)
		}
	}
	return
}

var (
	mockResponseRead = 0
)

func decodeOutgoingMySQL(clientConnId, destConnId int, requestBuffer []byte, clientConn, destConn net.Conn, h *hooks.Hook, started time.Time, readRequestDelay time.Duration, logger *zap.Logger) {
	// startedDecoding := time.Now()
	startedDecoding := time.Now()
	firstLoop := true
	doHandshakeAgain := false

	for {
		configMocks := h.GetConfigMocks()
		tcsMocks := h.GetTcsMocks()
		fmt.Println(configMocks, tcsMocks)
		var (
			mysqlRequests = []models.MySQLRequest{}
			// mongoResponses = []models.MongoResponse{}
		)

		fmt.Println(mysqlRequests)
		if firstLoop || doHandshakeAgain {
			packet := configMocks[0].Spec.MySqlResponses[0].Message
			opr := configMocks[0].Spec.MySqlResponses[0].Header.PacketType
			binaryPacket, err := encodeToBinary(&packet, opr, 0)
			if err != nil {
				fmt.Println("Error:", err)
				return
			}
			_, err = clientConn.Write(binaryPacket)
			requestBuffer, err = util.ReadBytes(clientConn)
			fmt.Println(requestBuffer)
			// oprRequest, requestHeader, mysqlRequest, err := DecodeMySQLPacket(bytesToMySQLPacket(requestBuffer), logger, destConn)
			handshakeResponseFromConfig := configMocks[0].Spec.MySqlResponses[1].Message
			opr2 := configMocks[0].Spec.MySqlResponses[1].Header.PacketType
			handshakeResponseBinary, err := encodeToBinary(&handshakeResponseFromConfig, opr2, 1)
			// _, err = destConn.Write(requestBuffer)
			//fmt.Println(oprRequest, requestHeader, mysqlRequest, handshakeResponseFromConfig, err1)
			_, err = clientConn.Write(handshakeResponseBinary)
			if doHandshakeAgain {
				doHandshakeAgain = false
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
				return
			}
			fmt.Println(oprRequest, requestHeader, mysqlRequest, err)
			handshakeResponseFromConfig := tcsMocks[mockResponseRead].Spec.MySqlResponses[0].Message
			opr2 := tcsMocks[mockResponseRead].Spec.MySqlResponses[0].Header.PacketType
			responseBinary, err := encodeToBinary(&handshakeResponseFromConfig, opr2, mockResponseRead+1)

			_, err = clientConn.Write(responseBinary)

			mockResponseRead++
		}

		firstLoop = false
	}
	fmt.Println(startedDecoding, firstLoop)
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
func handleClientQueries(h *hooks.Hook, initialBuffer []byte, clientConn, destConn net.Conn, logger *zap.Logger) ([]*models.Mock, error) {
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
		operation, requestHeader, mysqlRequest, err := DecodeMySQLPacket(bytesToMySQLPacket(queryBuffer), logger, destConn)
		if len(queryBuffer) == 0 || operation == "COM_STMT_CLOSE" {
			break
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

		responseOperation, responseHeader, mysqlResp, err := DecodeMySQLPacket(bytesToMySQLPacket(queryResponse), logger, destConn)
		if err != nil {
			logger.Error("Failed to decode the MySQL packet from the destination server", zap.Error(err))
			continue
		}
		mysqlResponses = append([]models.MySQLResponse{}, models.MySQLResponse{
			Header: &models.MySQLPacketHeader{
				PacketLength: responseHeader.PayloadLength,
				PacketNumber: responseHeader.SequenceID,
				PacketType:   responseOperation,
			},
			Message: mysqlResp,
		})
		recordMySQLMessage(h, mysqlRequests, mysqlResponses, operation, responseOperation, "mocks")

	}
	return nil, nil
}

func recordMySQLMessage(h *hooks.Hook, mysqlRequests []models.MySQLRequest, mysqlResponses []models.MySQLResponse, operation string, responseOperation string, name string) {
	shouldRecordCalls := true

	if shouldRecordCalls {
		meta := map[string]string{
			"operation":         operation,
			"responseOperation": responseOperation,
		}
		mysqlMock := &models.Mock{
			Version: models.V1Beta2,
			Kind:    models.SQL,
			Name:    name,
			Spec: models.MockSpec{
				Metadata:       meta,
				MySqlRequests:  mysqlRequests,
				MySqlResponses: mysqlResponses,
				Created:        time.Now().Unix(),
			},
		}
		h.AppendMocks(mysqlMock)
	}
}

func bytesToMySQLPacket(buffer []byte) MySQLPacket {
	if buffer == nil || len(buffer) < 4 {
		// Log an error, return an empty packet
		fmt.Println("Error: buffer is nil or too short to be a valid MySQL packet")
		return MySQLPacket{}
	}
	length := binary.LittleEndian.Uint32(append(buffer[0:3], 0))
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
