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

// globalPassThrough relays client and dest to each other, unread.
//
// A side that ends what it sends (EOF) has its end forwarded: the other side
// is half-closed (CloseWrite: a FIN on TCP, a close_notify on TLS), and the
// direction still open goes on until its own EOF or error, or ctx's end, so a
// client that half-closes to end its request gets the server's whole answer,
// and a server that half-closes is heard as one. Where the other side cannot
// half-close (its conn has no CloseWrite, or it fails), the first EOF ends the
// relay and the caller closes both, as before. A read's error ends the relay at
// once, as does a write's, except a write to a side that already sent its EOF:
// it closed whole, and that ends the relay with no error.
//
// It ends on each of those whatever the other side still has to send or still
// has not read: ctx's end puts both connections' deadlines in the past, so a
// write to a peer that reads nothing returns (closing them is the caller's: a
// TLS layer writes as it closes), and its return ends its readers, so one
// holding a read it can no longer hand on returns too (pUtil.ReadBuffConn).
// The caller's wait for its connection's goroutines (the errgroup in ctx) then
// returns. No clock bounds a half-open relay: it lasts while its open side
// does, or until ctx ends.
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
	// One error channel a side: a side's EOF is forwarded to the other.
	clientErrChan := make(chan error, 1)
	destErrChan := make(chan error, 1)

	// read requests from client
	err := pUtil.ReadFromPeer(ctx, logger, client, clientBuffChan, clientErrChan, pUtil.Client, false)
	if err != nil {
		return fmt.Errorf("error reading from client:%v", err)
	}

	// read responses from destination
	err = pUtil.ReadFromPeer(ctx, logger, dest, destBuffChan, destErrChan, pUtil.Destination, false)
	if err != nil {
		return fmt.Errorf("error reading from destination:%v", err)
	}

	// clientSent and destSent: that side sent its EOF, and the other side was
	// half-closed for it.
	var clientSent, destSent bool
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case buffer, ok := <-clientBuffChan:
			if !ok { // the client's reader ended; its error says why
				clientBuffChan = nil
				continue
			}
			// Write the request message to the destination
			if _, err := dest.Write(buffer); err != nil {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				if destSent {
					logger.Debug("the destination, which had sent all it would, took no more of the client's bytes: it closed", zap.Error(err))
					return nil
				}
				utils.LogError(logger, err, "failed to write request message to the destination server")
				return fmt.Errorf("error writing to destination")
			}
		case buffer, ok := <-destBuffChan:
			if !ok { // the destination's reader ended; its error says why
				destBuffChan = nil
				continue
			}
			// Write the response message to the client
			if _, err := client.Write(buffer); err != nil {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				if clientSent {
					logger.Debug("the client, which had sent all it would, took no more of the destination's bytes: it closed", zap.Error(err))
					return nil
				}
				utils.LogError(logger, err, "failed to write response message to the client")
				return fmt.Errorf("error writing to client")
			}
		case err := <-clientErrChan:
			clientErrChan = nil
			if err != io.EOF {
				return err
			}
			if destSent || !halfClose(dest) {
				return nil
			}
			clientSent = true
		case err := <-destErrChan:
			destErrChan = nil
			if err != io.EOF {
				return err
			}
			if clientSent || !halfClose(client) {
				return nil
			}
			destSent = true
		}
	}
}

// halfClose ends what c sends, its reading side left open (CloseWrite: a FIN
// on TCP, a close_notify on TLS), and reports whether it did. A conn with no
// CloseWrite is looked through to the one it wraps (NetConn, the convention
// *tls.Conn set). A wrapper's CloseWrite answers for the conn it wraps.
func halfClose(c net.Conn) bool {
	type closeWriter interface{ CloseWrite() error }
	for i := 0; i < 8 && c != nil; i++ { // wrappers nest; a cycle must not hang
		if cw, ok := c.(closeWriter); ok {
			return cw.CloseWrite() == nil
		}
		u, ok := c.(interface{ NetConn() net.Conn })
		if !ok || u.NetConn() == c {
			return false
		}
		c = u.NetConn()
	}
	return false
}
