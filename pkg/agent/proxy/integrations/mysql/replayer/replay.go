// Package replayer is used to mock the MySQL traffic between the client and the server.
package replayer

import (
	"context"
	"errors"
	"fmt"
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

func Replay(ctx context.Context, logger *zap.Logger, clientConn net.Conn, dstCfg *models.ConditionalDstCfg, mockDb integrations.MockMemDb, opts models.OutgoingOptions) error {
	errCh := make(chan error, 1)

	unfiltered, err := mockDb.GetSessionMocks()
	if err != nil {
		utils.LogError(logger, err, "failed to get session mocks")
		return err
	}

	var configMocks []*models.Mock
	var hasMySQLMocks bool
	var totalMySQLMocks int
	var dataMocks int
	// Partition MySQL mocks into session/config vs data using the
	// typed Lifetime with a raw-tag fallback. The fallback handles
	// mocks that reach the pool without DeriveLifetime having set
	// Lifetime (inline test constructions, legacy paths). Functionally
	// identical to the pre-unification "type == config" read for any
	// correctly-tagged mock.
	for _, mock := range unfiltered {
		if mock.Kind == models.MySQL {
			hasMySQLMocks = true
			totalMySQLMocks++
			isSession := mock.TestModeInfo.Lifetime == models.LifetimeSession ||
				(mock.TestModeInfo.Lifetime == models.LifetimePerTest &&
					mock.Spec.Metadata != nil &&
					mock.Spec.Metadata["type"] == "config")
			if isSession {
				configMocks = append(configMocks, mock)
			} else {
				dataMocks++
			}
		}
	}

	logger.Debug("MySQL replay session starting",
		zap.Int("total_unfiltered_mocks", len(unfiltered)),
		zap.Int("total_mysql_mocks", totalMySQLMocks),
		zap.Int("config_mocks", len(configMocks)),
		zap.Int("data_mocks", dataMocks),
		zap.Bool("has_mysql_mocks", hasMySQLMocks))

	// A client that opens a MySQL connection waits to be greeted, and with no
	// session (handshake) mock there is nothing to greet it with: the proxy
	// closes the connection unanswered. That is a miss, returned so the proxy
	// queues it before it closes the connection. These two returned nil, and a
	// test whose application was refused its database passed with no miss.
	dst := destinationOf(dstCfg)
	if !hasMySQLMocks {
		utils.LogError(logger, nil, "no mysql mocks found")
		return newConnectionMiss(dst, stepGreeting, "no MySQL session mock is loaded, so there is no recorded server greeting", models.MatchPhaseNoMocks, 0)
	}

	if len(configMocks) == 0 {
		utils.LogError(logger, nil, "no mysql config mocks found for handshake")
		return newConnectionMiss(dst, stepGreeting,
			fmt.Sprintf("none of the %d MySQL mocks in the session pool is a handshake (config) mock, so there is no recorded server greeting", totalMySQLMocks),
			models.MatchPhaseNoMocks, 0)
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

		res, err := simulateInitialHandshake(ctx, logger, clientConn, configMocks, mockDb, decodeCtx, opts, dstCfg)
		if err != nil {
			if errors.Is(err, models.ErrNoMockMatched) {
				// A miss is not a failure to simulate the handshake: it is
				// logged where it is found, and the proxy reports it with
				// its mismatch report.
				logger.Debug("the initial handshake missed", zap.Error(err))
			} else {
				utils.LogError(logger, err, "failed to simulate initial handshake")
			}
			errCh <- err
			return
		}

		if res.refused {
			// The recorded refusal of the login was served: the server
			// closes a refused connection, and the proxy closes this one
			// once Replay returns.
			logger.Debug("served the recorded refusal of the login; ending the connection")
			errCh <- nil
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
		err = simulateCommandPhase(ctx, logger, clientConn, dst, mockDb, decodeCtx, opts)
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
