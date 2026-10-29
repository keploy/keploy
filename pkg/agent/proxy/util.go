package proxy

import (
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"time"

	pUtil "go.keploy.io/server/v3/pkg/agent/proxy/util"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
)

// writeNsswitchConfig writes the content to nsswitch.conf file
func writeNsswitchConfig(logger *zap.Logger, nsSwitchConfig string, data []byte, perm os.FileMode) error {

	err := os.WriteFile(nsSwitchConfig, data, perm)
	if err != nil {
		logger.Error("failed to write the configuration to the nsswitch.conf file to redirect the DNS queries to proxy", zap.Error(err))
		return err
	}
	return nil
}

// globalPassThrough relays client and dest to each other, unread, until either
// side closes or errs, or ctx ends. It ends on each of those whatever the other
// side still has to send or still has not read: ctx's end puts both
// connections' deadlines in the past, so a write to a peer that reads nothing
// returns (closing them is the caller's: a TLS layer writes as it closes), and
// its return ends its readers, so one holding a read it can no longer hand on
// returns too (pUtil.ReadBuffConn). The caller's wait for its connection's
// goroutines (the errgroup in ctx) then returns.
func (p *Proxy) globalPassThrough(ctx context.Context, client, dest net.Conn) error {
	stopInterrupting := context.AfterFunc(ctx, func() {
		_ = client.SetDeadline(time.Unix(1, 0))
		_ = dest.SetDeadline(time.Unix(1, 0))
	})
	defer stopInterrupting()
	ctx, endReaders := context.WithCancel(ctx)
	defer endReaders()

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
				if ctx.Err() != nil {
					return ctx.Err()
				}
				utils.LogError(logger, err, "failed to write request message to the destination server")
				return fmt.Errorf("error writing to destination")
			}
		case buffer := <-destBuffChan:
			// Write the response message to the client
			_, err := client.Write(buffer)
			if err != nil {
				if ctx.Err() != nil {
					return ctx.Err()
				}
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
