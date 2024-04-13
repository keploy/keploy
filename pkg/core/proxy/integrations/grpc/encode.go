package grpc

import (
	"context"
	"io"
	"net"

	pUtil "go.keploy.io/server/v2/pkg/core/proxy/util"
	"go.keploy.io/server/v2/pkg/models"
	"go.keploy.io/server/v2/utils"
	"go.uber.org/zap"
	"golang.org/x/sync/errgroup"
)

func encodeGrpc(ctx context.Context, logger *zap.Logger, reqBuf []byte, clientConn, destConn net.Conn, mocks chan<- *models.Mock, _ models.OutgoingOptions) error {

	// Send the client preface to the server. This should be the first thing sent from the client.
	_, err := destConn.Write(reqBuf)
	if err != nil {
		utils.LogError(logger, err, "Could not write preface onto the destination server")
		return err
	}

	if ctx.Err() != nil {
		return ctx.Err()
	}

	streamInfoCollection := NewStreamInfoCollection()
	reqFromClient := true

	serverSideDecoder := NewDecoder()

	// get the error group from the context
	g := ctx.Value(models.ErrGroupKey).(*errgroup.Group)
	errCh := make(chan error, 2)
	defer close(errCh)

	// Route requests from the client to the server.
	g.Go(func() error {
		defer pUtil.Recover(logger, clientConn, destConn)
		err := transferFrame(ctx, destConn, clientConn, streamInfoCollection, reqFromClient, serverSideDecoder, mocks)
		if err != nil {
			// check for EOF error
			if err == io.EOF {
				logger.Debug("EOF error received from client. Closing conn")
				return nil
			}
			utils.LogError(logger, err, "failed to transfer frame from client to server")
			if ctx.Err() != nil { //to avoid sending error to the closed channel if the context is cancelled
				return ctx.Err()
			}
			errCh <- err
		}
		return nil
	})

	// Route response from the server to the client.
	clientSideDecoder := NewDecoder()
	g.Go(func() error {
		defer pUtil.Recover(logger, clientConn, destConn)
		err := transferFrame(ctx, clientConn, destConn, streamInfoCollection, !reqFromClient, clientSideDecoder, mocks)
		if err != nil {
			utils.LogError(logger, err, "failed to transfer frame from server to client")
			if ctx.Err() != nil { //to avoid sending error to the closed channel if the context is cancelled
				return ctx.Err()
			}
			errCh <- err
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
	// This would practically be an infinite loop, unless the client closes the grpc conn
	// during the runtime of the application.
	// A grpc server/client terminating after some time maybe intentional.
}
