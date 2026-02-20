// Package recorder is used to record the MySQL traffic between the client and the server.
package recorder

import (
	"context"
	"errors"
	"io"
	"net"
	"runtime"
	"time"

	"golang.org/x/sync/errgroup"

	"go.keploy.io/server/v3/pkg/agent/proxy/orchestrator"
	syncMock "go.keploy.io/server/v3/pkg/agent/proxy/syncMock"
	pUtil "go.keploy.io/server/v3/pkg/agent/proxy/util"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// Record records the MySQL traffic between the client and the server.
//
// Architecture: "TeeForward & Defer"
//
//  1. Handshake runs synchronously (once per connection, amortised by pools).
//  2. Two TeeForwardConns forward traffic at wire speed while buffering data
//     in pre-allocated ring buffers (zero heap allocs in forwarding path).
//  3. A reassembler goroutine reads from the ring buffers, frames MySQL
//     packets into request-response pairs (byte-level, no struct decode).
//  4. A decoder goroutine fully decodes the raw pairs into models.Mock.
//
// The forwarding path does zero heap allocations → identical latency to
// bare io.Copy (~12-13ms P50).
func Record(ctx context.Context, logger *zap.Logger, clientConn, destConn net.Conn, mocks chan<- *models.Mock, opts models.OutgoingOptions) error {
	errCh := make(chan error, 1)

	// Get the error group from the context.
	g, ok := ctx.Value(models.ErrGroupKey).(*errgroup.Group)
	if !ok {
		return errors.New("failed to get the error group from the context")
	}

	g.Go(func() error {
		defer pUtil.Recover(logger, clientConn, destConn)
		defer close(errCh)

		// ── Phase 1: Synchronous handshake ───────────────────────
		hsResult, err := handleHandshake(ctx, logger, clientConn, destConn, opts)
		if err != nil {
			if err != io.EOF {
				logger.Error("handshake failed. Check MySQL server credentials and ensure the server is accepting connections", zap.Error(err))
			}
			errCh <- err
			return nil
		}

		cmdClientConn := hsResult.ClientConn
		cmdDestConn := hsResult.DestConn

		// ── Phase 2: TeeForwardConn-based forwarding ──
		// Two TeeForwardConns: one per direction.
		// Each reads from src, forwards to dest at wire speed, and
		// buffers data in a 2 MB pre-allocated ring buffer (ZERO heap
		// allocations in the forwarding goroutine).
		//
		// clientTee: client→server (captures requests)
		//   → forwards queries to MySQL at wire speed BEFORE the pipeline
		//     wakes up, which is critical for P50 latency.
		// serverTee: server→client (captures responses)
		clientTee := orchestrator.NewTeeForwardConn(ctx, logger, cmdClientConn, cmdDestConn)
		serverTee := orchestrator.NewTeeForwardConn(ctx, logger, cmdDestConn, cmdClientConn)

		// ── Phase 3: Merged reassembler+decoder (single goroutine) ──
		// Send handshake mocks. These use raw packet representation for config
		// type, so decode is very fast (just wrapping bytes).
		if len(hsResult.Mocks) > 0 {
			connID := ""
			if v := ctx.Value(models.ClientConnectionIDKey); v != nil {
				connID = v.(string)
			}
			for _, entry := range hsResult.Mocks {
				mock, err := decodeRawMockEntry(ctx, logger, entry, nil, nil)
				if err != nil {
					logger.Debug("failed to decode handshake mock", zap.Error(err))
					continue
				}
				setConnID(mock, connID)
				mocks <- mock
			}
		}

		// The command-phase is handled by a SINGLE merged goroutine that
		// reads from both ring buffers, frames packets using slab allocation,
		// decodes inline, and sends mocks.
		pipelineDone := make(chan struct{})
		go func() {
			// Pin this goroutine to its own OS thread so the Go scheduler
			// cannot time-slice it against the two TeeForwardConn forwarding
			// goroutines.  Forwarding goroutines spend most of their time
			// blocked on network I/O (handled by the netpoller, not an OS
			// thread), so the extra thread here is low-cost.
			runtime.LockOSThread()
			defer runtime.UnlockOSThread()
			defer close(pipelineDone)
			runRecordPipeline(ctx, logger, clientTee, serverTee, mocks, opts, hsResult.State)
		}()

		// ── Phase 4: Wait for completion ─────────────────────────
		<-pipelineDone

		errCh <- nil
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

// recordMockDirect creates a models.Mock from a RawMockEntry's decoded data
// and sends it to the output channel. Used by ProcessRawMocksV2.
func recordMockDirect(ctx context.Context, mock *models.Mock, mocks chan<- *models.Mock, opts models.OutgoingOptions) {
	if opts.Synchronous {
		mgr := syncMock.Get()
		mgr.AddMock(mock)
		return
	}

	// Non-blocking send: if the channel buffer is full, fall back to a
	// goroutine so the decoder loop is never stalled.
	select {
	case mocks <- mock:
	default:
		go func() {
			select {
			case mocks <- mock:
			case <-ctx.Done():
			}
		}()
	}
}

// Ensure time is used (for mock timestamps).
var _ = time.Now
