// Package mysql provides the MySQL integration.
package mysql

import (
	"context"
	"io"
	"net"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/mysql/recorder"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/mysql/replayer"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
)

func init() {
	integrations.Register(integrations.MYSQL, &integrations.Parsers{
		Initializer: New,
		Priority:    100,
	})
}

type MySQL struct {
	logger *zap.Logger
}

func New(logger *zap.Logger) integrations.Integrations {
	return &MySQL{
		logger: logger,
	}
}

func (m *MySQL) MatchType(_ context.Context, _ []byte) bool {
	// MySQL is a server-speaks-first protocol — the server sends a
	// HandshakeV10 greeting before the client sends any data. Protocol
	// detection from client bytes is therefore impossible. MySQL is
	// detected in the proxy via server greeting inspection (record mode)
	// or mock metadata (test mode) before the generic MatchType loop runs.
	return false
}

func (m *MySQL) RecordOutgoing(ctx context.Context, session *integrations.RecordSession) error {
	logger := session.Logger

	err := recorder.Record(ctx, logger, session.Ingress, session.Egress, session.Mocks, session.Opts, session.TLSUpgrader)

	if err != nil {
		utils.LogError(logger, err, "failed to encode the mysql message into the yaml")
		return err
	}
	return nil
}

func (m *MySQL) MockOutgoing(ctx context.Context, src net.Conn, dstCfg *models.ConditionalDstCfg, mockDb integrations.MockMemDb, opts models.OutgoingOptions) error {
	logger := m.logger.With(zap.Any("Client ConnectionID", ctx.Value(models.ClientConnectionIDKey).(string)), zap.Any("Destination ConnectionID", ctx.Value(models.DestConnectionIDKey).(string)), zap.Any("Client IP Address", src.RemoteAddr().String()))
	err := replayer.Replay(ctx, logger, src, dstCfg, mockDb, opts)
	if err != nil && err != io.EOF {
		utils.LogError(logger, err, "failed to decode the mysql message from the yaml")
		return err
	}
	return nil
}
