//go:build linux

package mysql

import (
	"context"
	"errors"
	"io"
	"net"

	"golang.org/x/sync/errgroup"

	"go.keploy.io/server/v2/pkg/core/proxy/integrations/mysql/operation"
	pUtil "go.keploy.io/server/v2/pkg/core/proxy/util"
	"go.keploy.io/server/v2/pkg/models"
	"go.keploy.io/server/v2/pkg/models/mysql"
	"go.keploy.io/server/v2/utils"
	"go.uber.org/zap"
)

func encode(ctx context.Context, logger *zap.Logger, clientConn, destConn net.Conn, mocks chan<- *models.Mock, _ models.OutgoingOptions) error {

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

	g.Go(func() error {
		defer pUtil.Recover(logger, clientConn, destConn)
		defer close(errCh)

		// Helper struct for decoding packets
		decodeCtx := &operation.DecodeContext{
			Mode: models.MODE_RECORD,
			// Map for storing last operation per connection
			LastOp: operation.NewLastOpMap(),
			// Map for storing server greetings (inc capabilities, auth plugin, etc) per initial handshake (per connection)
			ServerGreetings: operation.NewGreetings(),
			// Map for storing prepared statements per connection
			PreparedStatements: make(map[uint32]*mysql.StmtPrepareOkPacket),
		}
		decodeCtx.LastOp.Store(clientConn, operation.RESET) //resetting last command for new loop

		// handle the initial client-server handshake (connection phase)
		result, err := handleInitialHandshake(ctx, logger, clientConn, destConn, decodeCtx)
		if err != nil {
			utils.LogError(logger, err, "failed to handle initial handshake")
			errCh <- err
			return nil
		}
		requests = append(requests, result.req...)
		responses = append(responses, result.resp...)

		reqTimestamp := result.reqTimestamp

		recordMock(ctx, requests, responses, "config", result.requestOperation, result.responseOperation, mocks, reqTimestamp)

		// reset the requests and responses
		requests = []mysql.Request{}
		responses = []mysql.Response{}

		//debug log
		lstOp, _ := decodeCtx.LastOp.Load(clientConn)
		logger.Info("last operation after initial handshake", zap.Any("last operation", lstOp))

		// handle the client-server interaction (command phase)
		err = handleClientQueries(ctx, logger, clientConn, destConn, mocks, decodeCtx)
		if err != nil {
			utils.LogError(logger, err, "failed to handle client queries")
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
