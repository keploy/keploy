package proxy

import (
	"context"
	"io"
	"net"
	"os"

	pUtil "go.keploy.io/server/v3/pkg/agent/proxy/util"
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

	// Use io.Copy for bidirectional forwarding. On Linux with *net.TCPConn
	// pairs, Go's io.Copy internally uses splice(2) for zero-copy
	// kernel-to-kernel forwarding, eliminating userspace buffer copies.
	// This replaces the previous channel-based approach which allocated
	// per-packet (make([]byte, n) + copy) and used channel send/receive
	// overhead (~50-100ns per packet).
	errCh := make(chan error, 2)

	// client → dest
	go func() {
		defer pUtil.Recover(p.logger, client, dest)
		_, err := io.Copy(dest, client)
		errCh <- err
	}()

	// dest → client
	go func() {
		defer pUtil.Recover(p.logger, client, dest)
		_, err := io.Copy(client, dest)
		errCh <- err
	}()

	// Wait for first direction to complete or context cancellation.
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
