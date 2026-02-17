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

	// Create rawMocks channel for async processing
	rawMocks := make(chan *models.Mock, 100)

	// Start the async worker to process raw mocks and send to final output
	g.Go(func() error {
		// Close rawMocks when this goroutine exits?
		// No, producers should close rawMocks.
		// But in this pattern, we want to ensure rawMocks is closed when the main logic finishes.
		ProcessRawMocks(ctx, logger, rawMocks, mocks)
		return nil
	})

	g.Go(func() error {
		defer pUtil.Recover(logger, clientConn, destConn)
		defer close(errCh)

		// Ensure rawMocks is closed when the main recording logic finishes
		defer close(rawMocks)

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
		recordMock(ctx, requests, responses, "config", result.requestOperation, result.responseOperation, rawMocks, reqTimestamp, resTimestamp, opts)

		// reset the requests and responses
		requests = []mysql.Request{}
		responses = []mysql.Response{}

		// TeeForwardConns were created inside handleInitialHandshake right
		// after SSL detection.  The auth phase already flowed through them
		// at wire speed.  Reuse them for the command phase.
		clientTeeConn := result.clientTeeConn
		destTeeConn := result.destTeeConn

		if clientTeeConn == nil || destTeeConn == nil {
			errCh <- errors.New("TeeForwardConns not created during handshake")
			return nil
		}

		// handle the client-server interaction (command phase)
		err = handleClientQueries(ctx, logger, clientTeeConn, destTeeConn, rawMocks, decodeCtx, opts)
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
	if opts.Synchronous {
		mgr := syncMock.Get()
		mgr.AddMock(mysqlMock)
		return
	}

	// Non-blocking send: if the channel buffer is full, fall back to a
	// goroutine so the parser loop (and hence forwarding) is never stalled
	// waiting for the mock consumer to drain the channel.
	select {
	case mocks <- mysqlMock:
	default:
		go func() { mocks <- mysqlMock }()
	}
}
