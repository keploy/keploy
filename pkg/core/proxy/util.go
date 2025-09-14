//go:build linux

package proxy

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"time"

	"go.keploy.io/server/v2/pkg/core/proxy/util"
	pUtil "go.keploy.io/server/v2/pkg/core/proxy/util"
	"go.keploy.io/server/v2/pkg/models"
	"go.keploy.io/server/v2/utils"
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
	err := pUtil.ReadFromPeer(ctx, logger, client, clientBuffChan, errChan, pUtil.Client)
	if err != nil {
		return fmt.Errorf("error reading from client:%v", err)
	}

	// read responses from destination
	err = pUtil.ReadFromPeer(ctx, logger, dest, destBuffChan, errChan, pUtil.Destination)
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

// peekN peeks up to n bytes from br with a read deadline on c, returns a COPY of bytes (non-consuming).
func peekN(c net.Conn, br *bufio.Reader, n int, d time.Duration) ([]byte, error) {
	if c == nil || br == nil || n <= 0 {
		return nil, nil
	}
	_ = c.SetReadDeadline(time.Now().Add(d))
	b, err := br.Peek(n)
	_ = c.SetReadDeadline(time.Time{})
	// Common non-fatal cases: EOF (empty), ErrBufferFull (we still got some), nil
	if err != nil && err != io.EOF && !errors.Is(err, bufio.ErrBufferFull) {
		return nil, err
	}
	cp := make([]byte, len(b))
	copy(cp, b)
	return cp, nil
}

// mkUtilConn builds a util.Conn from conn, br, logger.
func mkUtilConn(conn net.Conn, br *bufio.Reader, logger *zap.Logger) *util.Conn {
	var r io.Reader = br

	return &util.Conn{
		Conn:   conn,
		Reader: r,
		Logger: logger,
	}
}
