// Package recorder is used to record the MySQL traffic between the client and the server.
package recorder

import (
	"context"
	"errors"
	"io"
	"net"
	"time"

	"golang.org/x/sync/errgroup"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/mysql/wire"
	syncMock "go.keploy.io/server/v3/pkg/agent/proxy/syncMock"
	pUtil "go.keploy.io/server/v3/pkg/agent/proxy/util"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/models/mysql"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
)

// Record records the MySQL traffic between the client and the server.
func Record(ctx context.Context, logger *zap.Logger, clientConn, destConn net.Conn, mocks chan<- *models.Mock, opts models.OutgoingOptions) error {

	var (
		requests  []mysql.Request
		responses []mysql.Response
	)

	errCh := make(chan error, 1)

	//get the error group from the context
	g, ok := ctx.Value(models.ErrGroupKey).(*errgroup.Group)
	if !ok {
		return errors.New("failed to get the error group from the context")
	}

	// Check if this is post-TLS mode (decrypted data from SSL/GoTLS uprobes).
	// In this mode the TLS handshake already happened; the uprobe provides
	// decrypted plaintext starting from HandshakeResponse41.
	isPostTLS := false
	if v, ok := ctx.Value(models.PostTLSModeKey).(bool); ok && v {
		isPostTLS = true
	}

	g.Go(func() error {
		defer pUtil.Recover(logger, clientConn, destConn)
		defer close(errCh)

		// Helper struct for decoding packets
		decodeCtx := &wire.DecodeContext{
			Mode: models.MODE_RECORD,
			// Map for storing last operation per connection
			LastOp: wire.NewLastOpMap(),
			// Map for storing server greetings (inc capabilities, auth plugin, etc) per initial handshake (per connection)
			ServerGreetings: wire.NewGreetings(),
			// Map for storing prepared statements per connection
			PreparedStatements: make(map[uint32]*mysql.StmtPrepareOkPacket),
		}
		decodeCtx.LastOp.Store(clientConn, wire.RESET) //resetting last command for new loop

		if isPostTLS {
			// Post-TLS path: decrypted uprobe data starts after the TLS handshake.
			// Recover server greeting context from TLSHandshakeStore.
			err := handlePostTLSRecord(ctx, logger, clientConn, destConn, mocks, decodeCtx, opts)
			if err != nil && err != io.EOF {
				utils.LogError(logger, err, "failed to handle post-TLS MySQL recording")
				errCh <- err
			}
			return nil
		}

		// handle the initial client-server handshake (connection phase)
		result, err := handleInitialHandshake(ctx, logger, clientConn, destConn, decodeCtx, opts)
		if err != nil {
			utils.LogError(logger, err, "failed to handle initial handshake")
			errCh <- err
			return nil
		}
		requests = append(requests, result.req...)
		responses = append(responses, result.resp...)

		reqTimestamp := result.reqTimestamp
		resTimestamp := result.resTimestamp
		if !result.skipConfigMock {
			recordMock(ctx, requests, responses, "config", result.requestOperation, result.responseOperation, mocks, reqTimestamp, resTimestamp, opts)
		}

		// reset the requests and responses
		requests = []mysql.Request{}
		responses = []mysql.Response{}

		if decodeCtx.UseSSL {
			if result.tlsClientConn == nil || result.tlsDestConn == nil {
				// TLS connections are nil — this is expected in sockmap/low-latency
				// mode where the proxy doesn't MITM the TLS session. The pre-TLS
				// config mock has already been recorded above. Post-TLS command
				// phase data is captured by SSL/GoTLS uprobes independently.
				// Also handles the case where the client disconnected before TLS.
				logger.Debug("TLS connections not established after SSL request; pre-TLS config mock recorded, skipping command phase",
					zap.Any("tlsClientConn", result.tlsClientConn),
					zap.Any("tlsDestConn", result.tlsDestConn))
				return nil
			}
			clientConn = result.tlsClientConn
			destConn = result.tlsDestConn
		}

		lstOp, _ := decodeCtx.LastOp.Load(clientConn)
		logger.Debug("last operation after initial handshake", zap.Any("last operation", lstOp))

		// handle the client-server interaction (command phase)
		err = handleClientQueries(ctx, logger, clientConn, destConn, mocks, decodeCtx, opts)
		if err != nil {
			if err != io.EOF {
				utils.LogError(logger, err, "failed to handle client queries")
			}
			errCh <- err
			return nil
		}
		return nil
	})

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

func recordMock(ctx context.Context, requests []mysql.Request, responses []mysql.Response, mockType, requestOperation, responseOperation string, mocks chan<- *models.Mock, reqTimestampMock time.Time, resTimestampMock time.Time, opts models.OutgoingOptions) {
	meta := map[string]string{
		"type":              mockType,
		"requestOperation":  requestOperation,
		"responseOperation": responseOperation,
		"connID":            ctx.Value(models.ClientConnectionIDKey).(string),
	}
	if opts.DstCfg != nil && opts.DstCfg.Addr != "" {
		meta["destAddr"] = opts.DstCfg.Addr
	}
	mysqlMock := &models.Mock{
		Version: models.GetVersion(),
		Kind:    models.MySQL,
		Name:    mockType,
		Spec: models.MockSpec{
			Metadata:         meta,
			MySQLRequests:    requests,
			MySQLResponses:   responses,
			Created:          time.Now().Unix(),
			ReqTimestampMock: reqTimestampMock,
			ResTimestampMock: resTimestampMock,
		},
	}
	// Send to the mocks channel for YAML output (used by both normal and
	// sockmap proxy paths). Also add to syncMock for request-mock correlation.
	if mocks != nil {
		select {
		case mocks <- mysqlMock:
		case <-ctx.Done():
			return
		}
	}
	mgr := syncMock.Get()
	mgr.AddMock(mysqlMock)
	return
}
