package proxy

import (
	"bytes"
	"context"
	"io"
	"net"
	"strings"
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

	// Pipes for teeing captured bytes to the frame parsers.
	// The forwarding goroutines write copies of the bytes to these pipes.
	// The parser goroutines read from them using http2.NewFramer.
	clientPipeR, clientPipeW := io.Pipe()
	destPipeR, destPipeW := io.Pipe()
	defer clientPipeW.Close()
	defer destPipeW.Close()

	done := make(chan struct{})

	// Forward client → dest, tee to clientPipe for parsing
	go func() {
		forwardAndTee(clientConn, destConn, clientPipeW, logger, "client→app")
		clientPipeW.Close()
	}()

	// Forward dest → client, tee to destPipe for parsing
	go func() {
		forwardAndTee(destConn, clientConn, destPipeW, logger, "app→client")
		destPipeW.Close()
	}()

	// Parse client→app frames (requests)
	go func() {
		parseFrames(ctx, logger, clientPipeR, sm, false)
		emitCompleteStreams(logger, ctx, sm, t, appPort)
	}()

	// Parse app→client frames (responses) and emit test cases
	go func() {
		defer close(done)
		parseFrames(ctx, logger, destPipeR, sm, true)
		emitCompleteStreams(logger, ctx, sm, t, appPort)
	}()

	// Wait for the response parser to finish (both directions done)
	select {
	case <-ctx.Done():
		logger.Info("gRPC passthrough capture stopped (context cancelled)")
		return ctx.Err()
	case <-done:
		// Emit any remaining complete streams
		emitCompleteStreams(logger, ctx, sm, t, appPort)
		logger.Info("gRPC passthrough capture completed")
		return nil
	}
}

// forwardAndTee reads from src, writes to dst (forwarding), and also writes
// a copy to tee (for frame parsing). This is the core of the passthrough —
// bytes flow from client to app (and back) untouched.
func forwardAndTee(src, dst net.Conn, tee *io.PipeWriter, logger *zap.Logger, direction string) {
	buf := make([]byte, 32*1024)
	for {
		n, err := src.Read(buf)
		if n > 0 {
			// Forward to destination
			if _, wErr := dst.Write(buf[:n]); wErr != nil {
				logger.Debug("forward write error",
					zap.String("direction", direction), zap.Error(wErr))
				return
			}
			// Tee to parser (ignore write errors — parser may have closed)
			_, _ = tee.Write(buf[:n])
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

// parseFrames uses Go's standard http2.NewFramer to parse HTTP/2 frames from
// a reader and feeds them to the DefaultStreamManager. No manual binary
// parsing — the standard library handles frame boundaries, types, and flags.
func parseFrames(ctx context.Context, logger *zap.Logger, reader io.Reader, sm *pkg.DefaultStreamManager, isOutgoing bool) {
	// The client side starts with the HTTP/2 preface (24-byte magic string)
	// which is NOT a frame. We need to skip it before creating the Framer.
	if !isOutgoing {
		preface := make([]byte, len(http2.ClientPreface))
		if _, err := io.ReadFull(reader, preface); err != nil {
			logger.Debug("failed to read HTTP/2 client preface", zap.Error(err))
			return
		}
		if !bytes.Equal(preface, []byte(http2.ClientPreface)) {
			logger.Debug("unexpected bytes instead of HTTP/2 preface")
			return
		}
	}

	framer := http2.NewFramer(nil, reader)
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
			// Don't stop — continue parsing other frames
		}

		// Check for complete streams after each frame
		emitCompleteStreams(logger, ctx, sm, nil, 0)
	}
}

// emitCompleteStreams checks the stream manager for complete request/response
// pairs and sends them as test cases.
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
		logger.Info("captured gRPC test case (passthrough)", zap.String("method", method))
		Utils.CaptureGRPC(ctx, logger, t, stream, appPort)
		sm.CleanupStream(stream.ID)
	}
}
