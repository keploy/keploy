//go:build linux

// Package decoder provides the decoding functions for the MySQL integration.
// Mock Yaml to Binary
package decoder

import (
	"context"
	"io"

	"net"

	"go.keploy.io/server/v2/pkg/core/proxy/integrations"
	"go.keploy.io/server/v2/pkg/models/mysql"
	"go.keploy.io/server/v2/utils"

	"go.keploy.io/server/v2/pkg/core/proxy/integrations/mysql/constant"
	"go.keploy.io/server/v2/pkg/core/proxy/integrations/mysql/operation"
	intgUtil "go.keploy.io/server/v2/pkg/core/proxy/integrations/util"
	pUtil "go.keploy.io/server/v2/pkg/core/proxy/util"
	"go.keploy.io/server/v2/pkg/models"

	"go.uber.org/zap"
)

func Decode(ctx context.Context, logger *zap.Logger, clientConn net.Conn, _ *integrations.ConditionalDstCfg, mockDb integrations.MockMemDb, opts models.OutgoingOptions) error {
	errCh := make(chan error, 1)

	unfiltered, err := mockDb.GetUnFilteredMocks()
	if err != nil {
		utils.LogError(logger, err, "failed to get unfiltered mocks")
		return err
	}

	// Get the mysql mocks
	mocks := intgUtil.GetMockByKind(unfiltered, "MySQL")

	if len(mocks) == 0 {
		utils.LogError(logger, nil, "no mysql mocks found")
		return nil
	}

	var configMocks []*models.Mock
	// Get the mocks having "config" metadata
	for _, mock := range mocks {
		if mock.Spec.Metadata["type"] == "config" {
			configMocks = append(configMocks, mock)
		}
	}

	if len(configMocks) == 0 {
		utils.LogError(logger, nil, "no mysql config mocks found for handshake")
	}

	go func(errCh chan error, configMocks []*models.Mock, mockDb integrations.MockMemDb, opts models.OutgoingOptions) {
		defer pUtil.Recover(logger, clientConn, nil)
		defer close(errCh)

		// Helper struct for decoding packets
		decodeCtx := &operation.DecodeContext{
			Mode: models.MODE_TEST,
			// Map for storing last operation per connection
			LastOp: operation.NewLastOpMap(),
			// Map for storing server greetings (inc capabilities, auth plugin, etc) per initial handshake (per connection)
			ServerGreetings: operation.NewGreetings(),
			// Map for storing prepared statements per connection
			PreparedStatements: make(map[uint32]*mysql.StmtPrepareOkPacket),
			PluginName:         constant.CachingSha2Password, // Only supported plugin for now
		}
		decodeCtx.LastOp.Store(clientConn, operation.RESET) //resetting last command for new loop

		// Simulate the initial client-server handshake (connection phase)

		err := simulateInitialHandshake(ctx, logger, clientConn, configMocks, mockDb, decodeCtx)
		if err != nil {
			utils.LogError(logger, err, "failed to simulate initial handshake")
			errCh <- err
			return
		}

		logger.Debug("Initial handshake completed successfully")

		// Simulate the client-server interaction (command phase)
		err = simulateCommandPhase(ctx, logger, clientConn, mockDb, decodeCtx, opts)
		if err != nil {
			if err != io.EOF {
				utils.LogError(logger, err, "failed to simulate command phase")
			}
			errCh <- err
			return
		}

	}(errCh, configMocks, mockDb, opts)

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
