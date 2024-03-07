package mysql

import (
	"context"
	"net"
	"time"

	"go.keploy.io/server/v2/pkg/core/proxy/integrations"
	"go.keploy.io/server/v2/utils"

	"go.keploy.io/server/v2/pkg/core/proxy/util"
	"go.keploy.io/server/v2/pkg/models"
	"go.uber.org/zap"
)

func init() {
	integrations.Register("mysql", NewMySql)
}

//TODO: seggregate the packet types and the operations into meaningful packages.
//TODO: follow the same pattern for naming the functions and the variables & structs.

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
	logger := m.logger.With(zap.Any("Client IP Address", src.RemoteAddr().String()), zap.Any("Client ConnectionID", util.GetNextID()), zap.Any("Destination ConnectionID", util.GetNextID()))

	err := encodeMySql(ctx, logger, src, dst, mocks, opts)
	if err != nil {
		utils.LogError(logger, err, "failed to encode the mysql message into the yaml")
		return err
	}
	return nil
}

func (m *MySql) MockOutgoing(ctx context.Context, src net.Conn, dstCfg *integrations.ConditionalDstCfg, mockDb integrations.MockMemDb, opts models.OutgoingOptions) error {
	logger := m.logger.With(zap.Any("Client IP Address", src.RemoteAddr().String()), zap.Any("Client ConnectionID", util.GetNextID()), zap.Any("Destination ConnectionID", util.GetNextID()))

	err := decodeMySql(ctx, logger, src, dstCfg, mockDb, opts)
	if err != nil {
		utils.LogError(logger, err, "failed to decode the mysql message from the yaml")
		return err
	}
	return nil
}

func recordMySQLMessage(ctx context.Context, mysqlRequests []models.MySQLRequest, mysqlResponses []models.MySQLResponse, name, operation, responseOperation string, mocks chan<- *models.Mock) {

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
