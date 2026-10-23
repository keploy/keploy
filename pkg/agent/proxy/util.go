package proxy

import (
	"context"
	"fmt"
	"io"
	"net"

	pUtil "go.keploy.io/server/v3/pkg/agent/proxy/util"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
)

func (p *Proxy) globalPassThrough(ctx context.Context, client, dest net.Conn) error {

	logger := p.logger.With(
		zap.String("Client ConnectionID", ctx.Value(models.ClientConnectionIDKey).(string)),
		zap.String("Destination ConnectionID", ctx.Value(models.DestConnectionIDKey).(string)),
		zap.String("Client IP Address", client.RemoteAddr().String()),
	)

	clientBuffChan := make(chan []byte)
	destBuffChan := make(chan []byte)
	errChan := make(chan error, 2)

	// read requests from client
	err := pUtil.ReadFromPeer(ctx, logger, client, clientBuffChan, errChan, pUtil.Client, false)
	if err != nil {
		return fmt.Errorf("error reading from client:%v", err)
	}

	// read responses from destination
	err = pUtil.ReadFromPeer(ctx, logger, dest, destBuffChan, errChan, pUtil.Destination, false)
	if err != nil {
		return fmt.Errorf("error reading from destination:%v", err)
	}

	//write the request or response buffer to the respective destination
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case buffer := <-clientBuffChan:
			// Write the request message to the destination
			_, err := dest.Write(buffer)
			if err != nil {
				utils.LogError(logger, err, "failed to write request message to the destination server")
				return fmt.Errorf("error writing to destination")
			}
		case buffer := <-destBuffChan:
			// Write the response message to the client
			_, err := client.Write(buffer)
			if err != nil {
				utils.LogError(logger, err, "failed to write response message to the client")
				return fmt.Errorf("error writing to client")
			}
		case err := <-errChan:
			if err == io.EOF {
				return nil
			}
			return err
		}
	}
}
