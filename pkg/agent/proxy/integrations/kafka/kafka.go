package kafka

import (
	"context"
	"net"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/kafka/recorder"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/kafka/replayer"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

func init() {
	integrations.Register(integrations.KAFKA, &integrations.Parsers{
		Initializer: New,
		Priority:    100,
	})
}

type Kafka struct {
	logger *zap.Logger
}

func New(logger *zap.Logger) integrations.Integrations {
	return &Kafka{
		logger: logger,
	}
}

func (k *Kafka) MatchType(_ context.Context, _ []byte) bool {
	return false
}

func (k *Kafka) RecordOutgoing(ctx context.Context, src net.Conn, dst net.Conn, mocks chan<- *models.Mock, clientClose chan bool, opts models.OutgoingOptions) error {
	return recorder.Record(ctx, k.logger, src, dst, mocks, clientClose, opts)
}

func (k *Kafka) MockOutgoing(ctx context.Context, src net.Conn, dstCfg *models.ConditionalDstCfg, mockDb integrations.MockMemDb, opts models.OutgoingOptions) error {
	return replayer.Replay(ctx, k.logger, src, dstCfg, mockDb, opts)
}
