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
	"go.keploy.io/server/v3/pkg/agent/memoryguard"
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
	// Close connections on ctx cancellation to unblock all goroutines.
	// This ensures no goroutine leaks during shutdown/StopIngress.
	go func() {
		<-ctx.Done()
		clientConn.Close()
		destConn.Close()
	}()

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

	logger.Debug("starting gRPC test case capture (passthrough mode)")

	sm := pkg.NewStreamManager(logger)

	// Channels for teeing bytes to parsers. Large buffer (512) so the forwarder
	// is never blocked by the parser in practice. If the channel fills up, the
	// forwarder blocks (preserving stream integrity) rather than dropping bytes
	// which would corrupt the HTTP/2 frame alignment for the parser.
	clientFrameCh := make(chan frameChunk, 512)
	destFrameCh := make(chan frameChunk, 512)

	// Forward client → dest, tee copies to channel
	go func() {
		defer close(clientFrameCh)
		forwardAndTee(clientConn, destConn, clientFrameCh, logger, "client→app")
	}()

	// Forward dest → client, tee copies to channel
	go func() {
		defer close(destFrameCh)
		forwardAndTee(destConn, clientConn, destFrameCh, logger, "app→client")
	}()

	// Single emitter goroutine to avoid duplicate test case emission.
	emitCh := make(chan struct{}, 1)
	emitDone := make(chan struct{})
	go func() {
		defer close(emitDone)
		for range emitCh {
			emitCompleteStreams(logger, ctx, sm, t, appPort)
		}
		emitCompleteStreams(logger, ctx, sm, t, appPort)
	}()

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

	parseWg.Wait()
	close(emitCh)
	<-emitDone

	logger.Debug("gRPC passthrough capture completed")
	return nil
}

// forwardAndTee reads from src, writes to dst (forwarding), and sends a copy
// to the channel for frame parsing. The channel send blocks if full to preserve
// HTTP/2 frame stream integrity (dropping bytes would corrupt parser alignment).
// This is safe because the parser is faster than network I/O, and ctx cancellation
// closes the connections which unblocks the reads.
//
// When memory pressure is detected, the tee stops permanently for this
// connection (cannot resume without corrupting the parser's frame alignment).
// Forwarding continues unimpacted.
type frameChunk struct {
	data      []byte
	timestamp time.Time
}

func forwardAndTee(src, dst net.Conn, ch chan<- frameChunk, logger *zap.Logger, direction string) {
	buf := make([]byte, 32*1024)
	teeActive := !memoryguard.IsRecordingPaused()
	for {
		n, err := src.Read(buf)
		if n > 0 {
			readAt := time.Now()
			if _, wErr := dst.Write(buf[:n]); wErr != nil {
				logger.Debug("forward write error",
					zap.String("direction", direction), zap.Error(wErr))
				return
			}
			if teeActive {
				if memoryguard.IsRecordingPaused() {
					teeActive = false
				} else {
					data := make([]byte, n)
					copy(data, buf[:n])
					ch <- frameChunk{
						data:      data,
						timestamp: readAt,
					} // blocking send — preserves stream integrity
				}
			}
		}
		if err != nil {
			if err != io.EOF && !strings.Contains(err.Error(), "use of closed") {
				logger.Debug("forward read finished",
					zap.String("direction", direction), zap.Error(err))
			}
			return
		}
	}
}

// parseFramesFromChan reconstructs HTTP/2 frames from timestamped byte chunks
// received via channel, then feeds them to the DefaultStreamManager.
func parseFramesFromChan(ctx context.Context, logger *zap.Logger, ch <-chan frameChunk, sm *pkg.DefaultStreamManager, isOutgoing bool, triggerEmit func()) {
	var buffer []byte
	prefaceRead := isOutgoing

	for {
		var chunk frameChunk
		select {
		case <-ctx.Done():
			drainFrameChunks(ch)
			return
		case next, ok := <-ch:
			if !ok {
				return
			}
			chunk = next
		}

		if len(chunk.data) == 0 {
			continue
		}
		buffer = append(buffer, chunk.data...)

		if !prefaceRead {
			if len(buffer) < len(http2.ClientPreface) {
				continue
			}
			if !bytes.Equal(buffer[:len(http2.ClientPreface)], []byte(http2.ClientPreface)) {
				logger.Debug("unexpected bytes instead of HTTP/2 preface")
				drainFrameChunks(ch)
				return
			}
			buffer = buffer[len(http2.ClientPreface):]
			prefaceRead = true
		}

		for {
			if len(buffer) < 9 {
				break
			}
			frameLength := (uint32(buffer[0]) << 16) | (uint32(buffer[1]) << 8) | uint32(buffer[2])
			if frameLength > pkg.MaxFrameSize {
				logger.Debug("frame parser finished",
					zap.Bool("isOutgoing", isOutgoing),
					zap.Uint32("frameLength", frameLength),
					zap.Uint32("maxFrameSize", pkg.MaxFrameSize))
				drainFrameChunks(ch)
				return
			}
			frameSize := int(frameLength) + 9
			if len(buffer) < frameSize {
				break
			}

			frame, consumed, err := pkg.ExtractHTTP2Frame(buffer[:frameSize])
			if err != nil {
				logger.Debug("frame parser finished", zap.Bool("isOutgoing", isOutgoing), zap.Error(err))
				drainFrameChunks(ch)
				return
			}
			buffer = buffer[consumed:]
			if len(buffer) == 0 {
				buffer = nil
			}

			if err := sm.HandleFrame(frame, isOutgoing, chunk.timestamp); err != nil {
				logger.Debug("stream manager error", zap.Error(err))
			}

			triggerEmit()
		}
	}
}

func drainFrameChunks(ch <-chan frameChunk) {
	for range ch {
	}
}

// emitCompleteStreams checks the stream manager for complete request/response
// pairs and sends them as test cases. Only called from the single emitter
// goroutine to prevent duplicate emissions.
func emitCompleteStreams(logger *zap.Logger, ctx context.Context, sm *pkg.DefaultStreamManager, t chan *models.TestCase, appPort uint16) {
	if t == nil || memoryguard.IsRecordingPaused() {
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
