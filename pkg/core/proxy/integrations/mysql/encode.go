//go:build linux

package mysql

import (
	"context"
	"errors"
	"io"
	"net"
	"time"

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

	//for keeping conn alive
	g.Go(func() error {
		defer pUtil.Recover(logger, clientConn, destConn)
		defer close(errCh)

		// Helper struct for decoding packets
		decodeCtx := &operation.DecodeContext{
			Mode:       models.MODE_RECORD,
			ClientConn: clientConn,
			// Map for storing last operation per connection
			LastOp: operation.NewLastOpMap(),
			// Map for storing server greetings (inc capabilities, auth plugin, etc) per initial handshake (per connection)
			ServerGreetings: operation.NewGreetings(),
			// Map for storing prepared statements per connection
			PreparedStatements: make(map[uint32]*mysql.StmtPrepareOkPacket),
		}

		for {

			decodeCtx.LastOp.Store(clientConn, operation.RESET) //resetting last command for new loop

			data, source, err := readFirstBuffer(ctx, logger, clientConn, destConn)
			if len(data) == 0 {
				break
			}
			if err != nil {
				utils.LogError(logger, err, "failed to read initial data")
				errCh <- err
				return nil
			}

			// Getting timestamp for the request
			reqTimestamp := time.Now()

			switch source {
			case "destination":
				// handle the initial client-server handshake
				req, resp, err := handleInitialHandshake(ctx, logger, data, clientConn, destConn, decodeCtx)
				if err != nil {
					utils.LogError(logger, err, "failed to handle initial handshake")
					errCh <- err
					return nil
				}
				requests = append(requests, req...)
				responses = append(responses, resp...)
			case "client":
			}

			// if source == "destination" {

			// 	err = handleClientQueries(ctx, logger, nil, clientConn, destConn, mocks, lastCommand, reqTimestamp, preparedStatements, serverGreetings)
			// 	if err != nil {
			// 		if err == io.EOF {
			// 			logger.Debug("recieved request buffer is empty in record mode for mysql call")
			// 			errCh <- err
			// 			return nil
			// 		}
			// 		utils.LogError(logger, err, "failed to handle client queries")
			// 		errCh <- err
			// 		return nil
			// 	}
			// } else if source == "client" {
			// 	err := handleClientQueries(ctx, logger, nil, clientConn, destConn, mocks, lastCommand, reqTimestamp, preparedStatements, serverGreetings)
			// 	if err != nil {
			// 		utils.LogError(logger, err, "failed to handle client queries")
			// 		errCh <- err
			// 		return nil
			// 	}
			// }
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
