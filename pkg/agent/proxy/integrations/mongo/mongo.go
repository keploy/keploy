//go:build linux

package mongo

import (
	"context"
	"encoding/binary"
	"net"
	"sync"
	"time"

	"go.keploy.io/server/v2/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v2/utils"

	"go.keploy.io/server/v2/pkg/agent/proxy/util"
	"go.keploy.io/server/v2/pkg/models"
	"go.mongodb.org/mongo-driver/x/mongo/driver/wiremessage"
	"go.uber.org/zap"
)

func init() {
	integrations.Register("mongo", NewMongo)
}

type Mongo struct {
	logger                 *zap.Logger
	recordedConfigRequests sync.Map
}

func NewMongo(logger *zap.Logger) integrations.Integrations {
	return &Mongo{
		logger:                 logger,
		recordedConfigRequests: sync.Map{},
	}
}

// MatchType determines if the outgoing network call is Mongo by comparing the
// message format with that of a mongo wire message.
func (m *Mongo) MatchType(_ context.Context, buffer []byte) bool {
	if len(buffer) < 4 {
		return false
	}
	// identifies by the starting 4 bytes of the message, since that
	// are the length of the message.
	messageLength := binary.LittleEndian.Uint32(buffer[0:4])
	return int(messageLength) == len(buffer)
}

// RecordOutgoing records the outgoing mongo messages of the client connection into the yaml file.
// The database connection is keep-alive so, this function will be called during the connection establishment.
func (m *Mongo) RecordOutgoing(ctx context.Context, src net.Conn, dst net.Conn, mocks chan<- *models.Mock, clientClose chan bool, opts models.OutgoingOptions) error {
	logger := m.logger.With(zap.Any("Client IP Address", src.RemoteAddr().String()), zap.Any("Client ConnectionID", ctx.Value(models.ClientConnectionIDKey).(string)), zap.Any("Destination ConnectionID", ctx.Value(models.DestConnectionIDKey).(string)))
	reqBuf, err := util.ReadInitialBuf(ctx, logger, src)
	if err != nil {
		utils.LogError(logger, err, "failed to read the initial mongo message")
		return err
	}

	// the mongo messages are converted to the yaml format.
	//
	// initially the reqBuf contains the first network packet
	// from the client connection which is used to determine
	// the packet type in MatchType.
	err = m.encodeMongo(ctx, logger, reqBuf, src, dst, mocks, clientClose, opts)
	if err != nil {
		utils.LogError(logger, err, "failed to encode the mongo message into the yaml")
		return err
	}
	return nil
}

// MockOutgoing reads the outgoing mongo requests of the client connection and
// mocks the responses from the yaml file. The database connection is keep-alive
func (m *Mongo) MockOutgoing(ctx context.Context, src net.Conn, dstCfg *models.ConditionalDstCfg, mockDb integrations.MockMemDb, opts models.OutgoingOptions) error {
	// read the initial buffer from the client connection. Initially the
	// reqBuf contains the first network packet from the client connection
	// which is used to determine the packet type in MatchType.
	logger := m.logger.With(zap.Any("Client IP Address", src.RemoteAddr().String()), zap.Any("Client ConnectionID", ctx.Value(models.ClientConnectionIDKey).(string)), zap.Any("Destination ConnectionID", ctx.Value(models.DestConnectionIDKey).(string)))
	reqBuf, err := util.ReadInitialBuf(ctx, logger, src)
	if err != nil {
		utils.LogError(logger, err, "failed to read the initial mongo message")
		return err
	}

	m.logger.Info("Mocking the mongo message")
	// converts the yaml string into the binary packet
	err = decodeMongo(ctx, logger, reqBuf, src, dstCfg, mockDb, opts)
	if err != nil {
		utils.LogError(logger, err, "failed to decode the mongo message")
		return err
	}
	return nil
}

// recordMessage records the mongo messages into the yaml file.
func (m *Mongo) recordMessage(_ context.Context, logger *zap.Logger, mongoRequests []models.MongoRequest, mongoResponses []models.MongoResponse, opReq Operation, reqTimestampMock time.Time, mocks chan<- *models.Mock) {
	shouldRecordCalls := true // boolean to check for already saved config mocks
	name := "mocks"
	meta1 := map[string]string{
		"operation": opReq.String(),
	}

	// check that the packet is heartbeat or not.
	// See: https://github.com/mongodb/mongo-go-driver/blob/8489898c64a2d8c2e2160006eb851a11a9db9e9d/x/mongo/driver/operation/hello.go#L503
	if isHeartBeat(logger, opReq, *mongoRequests[0].Header, mongoRequests[0].Message) {
		meta1["type"] = "config"

		for _, req := range mongoRequests {

			switch req.Header.Opcode {
			case wiremessage.OpQuery:
				// check the opReq in the recorded config requests. if present then, skip recording
				if _, ok := m.recordedConfigRequests.Load(req.Message.(*models.MongoOpQuery).Query); ok {
					shouldRecordCalls = false
					break
				}
				m.recordedConfigRequests.Store(req.Message.(*models.MongoOpQuery).Query, true)
			case wiremessage.OpMsg:
				// check the opReq in the recorded config requests. if present then, skip recording
				if _, ok := m.recordedConfigRequests.Load(req.Message.(*models.MongoOpMessage).Sections[0]); ok {
					shouldRecordCalls = false
					break
				}
				m.recordedConfigRequests.Store(req.Message.(*models.MongoOpMessage).Sections[0], true)
			default:
				// check the opReq in the recorded config requests. if present then, skip recording
				if _, ok := m.recordedConfigRequests.Load(opReq.String()); ok {
					shouldRecordCalls = false
					break
				}
				m.recordedConfigRequests.Store(opReq.String(), true)
			}
		}
	}
	// record the mongo messages
	if shouldRecordCalls {
		mongoMock := &models.Mock{
			Version: models.GetVersion(),
			Kind:    models.Mongo,
			Name:    name,
			Spec: models.MockSpec{
				Metadata:         meta1,
				MongoRequests:    mongoRequests,
				MongoResponses:   mongoResponses,
				Created:          time.Now().Unix(),
				ReqTimestampMock: reqTimestampMock,
				ResTimestampMock: time.Now(),
			},
		}
		// Save the mock
		mocks <- mongoMock
	}
}
