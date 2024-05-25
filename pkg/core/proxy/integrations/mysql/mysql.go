package mysql

import (
	"context"
	"net"
	"time"

	"go.keploy.io/server/v2/pkg/core/proxy/integrations"
	"go.keploy.io/server/v2/utils"

	"go.keploy.io/server/v2/pkg/models"
	"go.uber.org/zap"
)

func init() {
	integrations.Register("mysql", NewMySQL)
}

//TODO: seggregate the packet types and the operations into meaningful packages.
//TODO: follow the same pattern for naming the functions and the variables & structs.

type MySQL struct {
	logger *zap.Logger
}

func NewMySQL(logger *zap.Logger) integrations.Integrations {
	return &MySQL{
		logger: logger,
	}
}

func (m *MySQL) MatchType(_ context.Context, _ []byte) bool {
	//Returning false here because sql parser is using the ports to check if the packet is mysql or not.
	return false
}

func (m *MySQL) RecordOutgoing(ctx context.Context, src net.Conn, dst net.Conn, mocks chan<- *models.Mock, opts models.OutgoingOptions) error {
	logger := m.logger.With(zap.Any("Client IP Address", src.RemoteAddr().String()), zap.Any("Client ConnectionID", ctx.Value(models.ClientConnectionIDKey).(string)), zap.Any("Destination ConnectionID", ctx.Value(models.DestConnectionIDKey).(string)))

	err := encodeMySQL(ctx, logger, src, dst, mocks, opts)
	if err != nil {
		utils.LogError(logger, err, "failed to encode the mysql message into the yaml")
		return err
	}
	return nil
}

func (m *MySQL) MockOutgoing(ctx context.Context, src net.Conn, dstCfg *integrations.ConditionalDstCfg, mockDb integrations.MockMemDb, opts models.OutgoingOptions) error {
	logger := m.logger.With(zap.Any("Client IP Address", src.RemoteAddr().String()), zap.Any("Client ConnectionID", ctx.Value(models.ClientConnectionIDKey).(string)), zap.Any("Destination ConnectionID", ctx.Value(models.DestConnectionIDKey).(string)))
	err := decodeMySQL(ctx, logger, src, dstCfg, mockDb, opts)
	if err != nil {
		utils.LogError(logger, err, "failed to decode the mysql message from the yaml")
		return err
	}
	return nil
}

func recordMySQLMessage(_ context.Context, mysqlRequests []models.MySQLRequest, mysqlResponses []models.MySQLResponse, name, operation, responseOperation string, mocks chan<- *models.Mock) {

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
			MySQLRequests:  mysqlRequests,
			MySQLResponses: mysqlResponses,
			Created:        time.Now().Unix(),
		},
	}
	mocks <- mysqlMock
}
