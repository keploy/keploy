package proxy

import (
	"bytes"
	"context"
	"io"
	"net"
	"strings"
	"sync"
	"time"

	"go.keploy.io/server/v3/pkg"
	Utils "go.keploy.io/server/v3/pkg/agent/hooks/conn"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
	"golang.org/x/net/http2"
)

// RecordIncoming captures an incoming gRPC conversation by forwarding raw bytes
// bidirectionally between the client and the upstream app, while parsing HTTP/2
// frames using Go's standard library (http2.NewFramer) and tracking stream state
// via DefaultStreamManager.
//
// This passthrough approach is compatible with cmux and any other listener wrapper
// because it forwards the client's original HTTP/2 handshake directly to the app
// without creating a separate gRPC session.
func RecordIncoming(ctx context.Context, logger *zap.Logger, clientConn, destConn net.Conn, t chan *models.TestCase, appPort uint16, _ string) error {
	defer func() {
		if err := clientConn.Close(); err != nil &&
			!strings.Contains(err.Error(), "use of closed network connection") {
			logger.Debug("failed to close client connection", zap.Error(err))
		}
		if err := destConn.Close(); err != nil &&
			!strings.Contains(err.Error(), "use of closed network connection") {
			logger.Debug("failed to close destination connection", zap.Error(err))
		}
	}()

	logger.Info("starting gRPC test case capture (passthrough mode)")

	sm := pkg.NewStreamManager(logger)

	// Channels for decoupled teeing — forwarders write to buffered channels
	// instead of io.Pipe to avoid backpressure from parsers blocking forwarding.
	clientFrameCh := make(chan []byte, 64)
	destFrameCh := make(chan []byte, 64)

	var forwardWg sync.WaitGroup
	forwardWg.Add(2)

	// Forward client → dest, tee copies to channel
	go func() {
		defer forwardWg.Done()
		defer close(clientFrameCh)
		forwardAndTee(clientConn, destConn, clientFrameCh, logger, "client→app")
	}()

	// Forward dest → client, tee copies to channel
	go func() {
		defer forwardWg.Done()
		defer close(destFrameCh)
		forwardAndTee(destConn, clientConn, destFrameCh, logger, "app→client")
	}()

	// Single emitter goroutine to avoid duplicate test case emission.
	// Only this goroutine calls emitCompleteStreams.
	emitCh := make(chan struct{}, 1)
	emitDone := make(chan struct{})
	go func() {
		defer close(emitDone)
		for range emitCh {
			emitCompleteStreams(logger, ctx, sm, t, appPort)
		}
		// Final drain
		emitCompleteStreams(logger, ctx, sm, t, appPort)
	}()

	// Signal the emitter to check for complete streams (non-blocking)
	triggerEmit := func() {
		select {
		case emitCh <- struct{}{}:
		default:
		}
	}

	var parseWg sync.WaitGroup
	parseWg.Add(2)

	// Parse client→app frames (requests)
	go func() {
		defer parseWg.Done()
		parseFramesFromChan(ctx, logger, clientFrameCh, sm, false, triggerEmit)
	}()

	// Parse app→client frames (responses)
	go func() {
		defer parseWg.Done()
		parseFramesFromChan(ctx, logger, destFrameCh, sm, true, triggerEmit)
	}()

	// Wait for parsers to finish, then close emitter
	parseWg.Wait()
	close(emitCh)
	<-emitDone

	logger.Info("gRPC passthrough capture completed")
	return nil
}

// forwardAndTee reads from src, writes to dst (forwarding), and sends a copy
// to the channel for frame parsing. The channel is buffered so the forwarder
// is not blocked by slow parsing. If the channel is full, the copy is dropped
// (forwarding is never interrupted).
func forwardAndTee(src, dst net.Conn, ch chan<- []byte, logger *zap.Logger, direction string) {
	buf := make([]byte, 32*1024)
	for {
		n, err := src.Read(buf)
		if n > 0 {
			// Forward to destination — this is the critical path
			if _, wErr := dst.Write(buf[:n]); wErr != nil {
				logger.Debug("forward write error",
					zap.String("direction", direction), zap.Error(wErr))
				return
			}
			// Tee copy to parser (non-blocking — drop if channel full)
			data := make([]byte, n)
			copy(data, buf[:n])
			select {
			case ch <- data:
			default:
				logger.Debug("parser channel full, dropping frame data",
					zap.String("direction", direction))
			}
		}
		if err != nil {
			if err != io.EOF {
				logger.Debug("forward read finished",
					zap.String("direction", direction), zap.Error(err))
			}
			return
		}
	}
}

// parseFramesFromChan reconstructs HTTP/2 frames from byte chunks received
// via channel, then feeds them to the DefaultStreamManager.
func parseFramesFromChan(ctx context.Context, logger *zap.Logger, ch <-chan []byte, sm *pkg.DefaultStreamManager, isOutgoing bool, triggerEmit func()) {
	pr, pw := io.Pipe()
	defer pr.Close()

	// Writer goroutine: drains channel into pipe
	go func() {
		defer pw.Close()
		for data := range ch {
			if _, err := pw.Write(data); err != nil {
				return
			}
		}
	}()

	// Skip HTTP/2 client preface (24-byte magic string) on client side
	if !isOutgoing {
		preface := make([]byte, len(http2.ClientPreface))
		if _, err := io.ReadFull(pr, preface); err != nil {
			logger.Debug("failed to read HTTP/2 client preface", zap.Error(err))
			return
		}
		if !bytes.Equal(preface, []byte(http2.ClientPreface)) {
			logger.Debug("unexpected bytes instead of HTTP/2 preface")
			return
		}
	}

	framer := http2.NewFramer(nil, pr)
	framer.ReadMetaHeaders = nil       // we handle headers ourselves
	framer.MaxHeaderListSize = 1 << 20 // 1MB max header list

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		frame, err := framer.ReadFrame()
		if err != nil {
			if err != io.EOF && !strings.Contains(err.Error(), "use of closed") {
				logger.Debug("frame parser finished", zap.Bool("isOutgoing", isOutgoing), zap.Error(err))
			}
			return
		}

		now := time.Now()
		if err := sm.HandleFrame(frame, isOutgoing, now); err != nil {
			logger.Debug("stream manager error", zap.Error(err))
		}

		triggerEmit()
	}
}

// emitCompleteStreams checks the stream manager for complete request/response
// pairs and sends them as test cases. Only called from the single emitter
// goroutine to prevent duplicate emissions.
func emitCompleteStreams(logger *zap.Logger, ctx context.Context, sm *pkg.DefaultStreamManager, t chan *models.TestCase, appPort uint16) {
	if t == nil {
		return
	}

	streams := sm.GetCompleteStreams()
	for _, stream := range streams {
		if stream.GRPCReq == nil || stream.GRPCResp == nil {
			continue
		}

		method := ""
		if path, ok := stream.GRPCReq.Headers.PseudoHeaders[":path"]; ok {
			method = path
		}
		logger.Debug("captured gRPC test case (passthrough)", zap.String("method", method))
		Utils.CaptureGRPC(ctx, logger, t, stream, appPort)
		sm.CleanupStream(stream.ID)
	}
}
