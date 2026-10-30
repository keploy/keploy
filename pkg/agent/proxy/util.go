package proxy

import (
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"sync"
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
// half-close (no conn down its wrappers has a CloseWrite,
// pUtil.ErrCannotHalfClose, or its CloseWrite fails), the first EOF ends the
// relay and the caller closes both, as before. A read's error ends the relay
// at once, as does a write's, except a write to a side that already sent its
// EOF: it closed whole, and that ends the relay with no error.
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
	ctx, endReaders := context.WithCancel(ctx)
	defer endReaders()
	// The interrupt hangs off the relay's own context, not the caller's: it
	// then runs only once this context reads as ended, so the relay and its
	// readers, which judge an interrupted write or read by this context, see
	// the end that caused it and return quietly. Off the caller's context it
	// was a sibling of this one, and could put the deadlines in the past
	// before this one ended: an interrupted write was then logged as a
	// failure. stopInterrupting runs first at return, so endReaders does not
	// set the deadlines.
	stopInterrupting := context.AfterFunc(ctx, func() {
		_ = client.SetDeadline(time.Unix(1, 0))
		_ = dest.SetDeadline(time.Unix(1, 0))
	})
	defer stopInterrupting()

	logger := p.logger.With(
		zap.String("Client ConnectionID", ctx.Value(models.ClientConnectionIDKey).(string)),
		zap.String("Destination ConnectionID", ctx.Value(models.DestConnectionIDKey).(string)),
		zap.String("Client IP Address", client.RemoteAddr().String()),
	)

	// Each side's reads (at most pUtil.WaitReadSize each, the small read a
	// side that waits holds) queue for the write to the other, which takes
	// what has queued in one write (writeQueued).
	clientBuffChan := make(chan []byte, passThroughQueue)
	destBuffChan := make(chan []byte, passThroughQueue)
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
	// toDest and toClient write a read, and those queued behind it, to the
	// other side; end reports that the relay ends, and with what.
	toDest := func(buffer []byte) (end bool, err error) {
		if err := writeQueued(dest, buffer, clientBuffChan); err != nil {
			if ctx.Err() != nil {
				return true, ctx.Err()
			}
			if destSent {
				logger.Debug("the destination, which had sent all it would, took no more of the client's bytes: it closed", zap.Error(err))
				return true, nil
			}
			utils.LogError(logger, err, "failed to write request message to the destination server")
			return true, fmt.Errorf("error writing to destination")
		}
		return false, nil
	}
	toClient := func(buffer []byte) (end bool, err error) {
		if err := writeQueued(client, buffer, destBuffChan); err != nil {
			if ctx.Err() != nil {
				return true, ctx.Err()
			}
			if clientSent {
				logger.Debug("the client, which had sent all it would, took no more of the destination's bytes: it closed", zap.Error(err))
				return true, nil
			}
			utils.LogError(logger, err, "failed to write response message to the client")
			return true, fmt.Errorf("error writing to client")
		}
		return false, nil
	}
	// A side's reads before its error queue ahead of it: they are written
	// before the error is acted on, or its EOF's half-close would overtake
	// its last bytes.
	drain := func(queue chan []byte, write func([]byte) (bool, error)) (end bool, err error) {
		for len(queue) > 0 {
			buffer, ok := <-queue
			if !ok {
				break
			}
			if end, err := write(buffer); end {
				return true, err
			}
		}
		return false, nil
	}
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case buffer, ok := <-clientBuffChan:
			if !ok { // the client's reader ended; its error says why
				clientBuffChan = nil
				continue
			}
			if end, err := toDest(buffer); end {
				return err
			}
		case buffer, ok := <-destBuffChan:
			if !ok { // the destination's reader ended; its error says why
				destBuffChan = nil
				continue
			}
			if end, err := toClient(buffer); end {
				return err
			}
		case err := <-clientErrChan:
			clientErrChan = nil
			if end, werr := drain(clientBuffChan, toDest); end {
				return werr
			}
			if err != io.EOF {
				return err
			}
			if destSent || !halfClose(dest) {
				return nil
			}
			clientSent = true
		case err := <-destErrChan:
			destErrChan = nil
			if end, werr := drain(destBuffChan, toClient); end {
				return werr
			}
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

// passThroughQueue is how many reads of a side wait for globalPassThrough's
// write to the other: as many as one write of pUtil.ReadChunkSize takes.
const passThroughQueue = pUtil.ReadChunkSize / pUtil.WaitReadSize

// gatherScratch holds the buffers writeQueued gathers queued reads into.
var gatherScratch = sync.Pool{New: func() any { b := make([]byte, 0, pUtil.ReadChunkSize); return &b }}

// writeQueued writes b to w with the reads already queued behind it on queue
// gathered in, up to pUtil.ReadChunkSize: a side that streams is written a
// few reads at a time, not one write a read, and what has arrived is written
// at once, never held for more. queue has one taker, its relay, so a queued
// read is taken without waiting.
func writeQueued(w net.Conn, b []byte, queue chan []byte) error {
	if len(queue) == 0 || len(b) >= pUtil.ReadChunkSize {
		_, err := w.Write(b)
		return err
	}
	sp := gatherScratch.Get().(*[]byte)
	buf := append((*sp)[:0], b...)
	for len(queue) > 0 && len(buf)+pUtil.WaitReadSize <= pUtil.ReadChunkSize {
		more, ok := <-queue
		if !ok {
			break
		}
		buf = append(buf, more...)
	}
	_, err := w.Write(buf)
	if cap(buf) == pUtil.ReadChunkSize { // a bigger read (a carry-over drain) grew it: not kept
		*sp = buf[:0]
		gatherScratch.Put(sp)
	}
	return err
}

// halfClose ends what c sends, its reading side left open (CloseWrite: a FIN
// on TCP, a close_notify on TLS), and reports whether it did. The answer is
// pUtil.CloseWriteIfPossible's: the leaf's, looked up through c's wrappers,
// whether or not each has a CloseWrite of its own, and
// pUtil.ErrCannotHalfClose, not nil, where the leaf cannot half-close, so a
// peer told nothing is not waited for.
func halfClose(c net.Conn) bool {
	return c != nil && pUtil.CloseWriteIfPossible(c) == nil
}
