package grpc

import (
	"context"
	"go.keploy.io/server/v2/pkg/models"
	"go.keploy.io/server/v2/utils"
	"go.uber.org/zap"
	"io"
	"net"
	"sync"
)

func encodeGrpc(ctx context.Context, logger *zap.Logger, reqBuf []byte, clientConn, destConn net.Conn, mocks chan<- *models.Mock, _ models.OutgoingOptions) error {
	//closing the destination conn
	defer func(destConn net.Conn) {
		err := destConn.Close()
		if err != nil {
			utils.LogError(logger, err, "failed to close the destination connection")
		}
	}(destConn)

	// Send the client preface to the server. This should be the first thing sent from the client.
	_, err := destConn.Write(reqBuf)
	if err != nil {
		utils.LogError(logger, err, "Could not write preface onto the destination server")
		return err
	}

	var wg sync.WaitGroup
	streamInfoCollection := NewStreamInfoCollection()
	reqFromClient := true

	// Route requests from the client to the server.
	serverSideDecoder := NewDecoder()
	wg.Add(1)
	go func() {
		defer utils.Recover(logger)
		defer wg.Done()
		err := transferFrame(ctx, destConn, clientConn, streamInfoCollection, reqFromClient, serverSideDecoder, mocks)
		if err != nil {
			// check for EOF error
			if err == io.EOF {
				logger.Debug("EOF error received from client. Closing conn")
				return
			}
			utils.LogError(logger, err, "failed to transfer frame from client to server")
		}
	}()

	// Route response from the server to the client.
	clientSideDecoder := NewDecoder()
	wg.Add(1)
	go func() {
		defer utils.Recover(logger)
		defer wg.Done()
		err := transferFrame(ctx, clientConn, destConn, streamInfoCollection, !reqFromClient, clientSideDecoder, mocks)
		if err != nil {
			utils.LogError(logger, err, "failed to transfer frame from server to client")
		}
	}()

	// This would practically be an infinite loop, unless the client closes the grpc conn
	// during the runtime of the application.
	// A grpc server/client terminating after some time maybe intentional.
	wg.Wait()
	return nil
}
