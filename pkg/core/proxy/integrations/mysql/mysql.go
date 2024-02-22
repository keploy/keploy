package mysql

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"go.keploy.io/server/v2/pkg/core/proxy/integrations"
	"log"
	"net"
	"time"

	"go.keploy.io/server/v2/pkg/core/hooks"
	"go.keploy.io/server/v2/pkg/core/proxy/util"
	"go.keploy.io/server/v2/pkg/models"
	"go.uber.org/zap"
)

func init() {
	integrations.Register("mysql", NewMySql)
}

type MySql struct {
	logger *zap.Logger
}

func NewMySql(logger *zap.Logger) integrations.Integrations {
	return &MySql{
		logger: logger,
	}
}

func (m *MySql) MatchType(ctx context.Context, reqBuf []byte) bool {
	//Returning false here because sql parser is using the ports to check if the packet is mysql or not.
	return false
}

func (m *MySql) RecordOutgoing(ctx context.Context, src net.Conn, dst net.Conn, mocks chan<- *models.Mock, opts models.OutgoingOptions) error {
	err := encodeMySql(ctx, m.logger, src, dst, mocks, opts)
	if err != nil {
		m.logger.Error("failed to encode the mysql message into the yaml", zap.Error(err))
		return errors.New("failed to record the outgoing mysql call")
	}
	return nil
}

func (m *MySql) MockOutgoing(ctx context.Context, src net.Conn, dstCfg *integrations.ConditionalDstCfg, mockDb integrations.MockMemDb, opts models.OutgoingOptions) error {
	err := decodeMySql(ctx, m.logger, src, dstCfg, mockDb, opts)
	if err != nil {
		m.logger.Error("failed to decode the mysql message from the yaml", zap.Error(err))
		return errors.New("failed to mock the outgoing mysql call")
	}
	return nil
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
				if !h.IsUserAppTerminateInitiated() {
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
