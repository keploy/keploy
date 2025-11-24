// Package replayer is used to mock the MySQL traffic between the client and the server.
package replayer

import (
	"context"
	"io"
	"net"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/mysql/wire"
	pUtil "go.keploy.io/server/v3/pkg/agent/proxy/util"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/models/mysql"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
)

// Mock Yaml to Binary

func Replay(ctx context.Context, logger *zap.Logger, clientConn net.Conn, _ *models.ConditionalDstCfg, mockDb integrations.MockMemDb, opts models.OutgoingOptions) error {
	errCh := make(chan error, 1)

	unfiltered, err := mockDb.GetUnFilteredMocks()
	if err != nil {
		utils.LogError(logger, err, "failed to get unfiltered mocks")
		return err
	}

	var configMocks []*models.Mock
	var hasMySQLMocks bool
	var totalMySQLMocks int
	var dataMocks int
	// Get the mocks having "config" metadata and check for any MySQL mocks in a single pass.
	for _, mock := range unfiltered {
		if mock.Kind == models.MySQL {
			hasMySQLMocks = true
			totalMySQLMocks++
			if mock.Spec.Metadata["type"] == "config" {
				configMocks = append(configMocks, mock)
			} else {
				dataMocks++
			}
		}
	}

	logger.Info("MySQL replay session starting",
		zap.Int("total_unfiltered_mocks", len(unfiltered)),
		zap.Int("total_mysql_mocks", totalMySQLMocks),
		zap.Int("config_mocks", len(configMocks)),
		zap.Int("data_mocks", dataMocks),
		zap.Bool("has_mysql_mocks", hasMySQLMocks))

	if !hasMySQLMocks {
		utils.LogError(logger, nil, "no mysql mocks found")
		return nil
	}

	if len(configMocks) == 0 {
		utils.LogError(logger, nil, "no mysql config mocks found for handshake")
		return nil
	}

	go func(errCh chan error, configMocks []*models.Mock, mockDb integrations.MockMemDb, opts models.OutgoingOptions) {
		defer pUtil.Recover(logger, clientConn, nil)
		defer close(errCh)

		// Helper struct for decoding packets
		decodeCtx := &wire.DecodeContext{
			Mode: models.MODE_TEST,
			// Map for storing last operation per connection
			LastOp: wire.NewLastOpMap(),
			// Map for storing server greetings (inc capabilities, auth plugin, etc) per initial handshake (per connection)
			ServerGreetings: wire.NewGreetings(),
			// Map for storing prepared statements per connection
			PreparedStatements: make(map[uint32]*mysql.StmtPrepareOkPacket),
			PluginName:         string(mysql.CachingSha2), // usually a default plugin in newer versions of MySQL
			PreferRecordedCaps: true,
			StmtIDToQuery:      make(map[uint32]string),
			NextStmtID:         1,
		}
		decodeCtx.LastOp.Store(clientConn, wire.RESET) //resetting last command for new loop

		// Simulate the initial client-server handshake (connection phase)

		res, err := simulateInitialHandshake(ctx, logger, clientConn, configMocks, mockDb, decodeCtx, opts)
		if err != nil {
			utils.LogError(logger, err, "failed to simulate initial handshake")
			errCh <- err
			return
		}

		if decodeCtx.UseSSL {
			if res.tlsClientConn == nil {
				logger.Error("SSL is enabled but could not get the tls client connection")
				errCh <- nil
				return
			}
			clientConn = res.tlsClientConn
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
