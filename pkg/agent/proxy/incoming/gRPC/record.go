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
	syncMock "go.keploy.io/server/v3/pkg/agent/proxy/syncMock"
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
func RecordIncoming(ctx context.Context, logger *zap.Logger, clientConn, destConn net.Conn, t chan *models.TestCase, appPort uint16, _ string, synchronous, mapping bool) error {
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

	// Every chunk teed is being built into a test case, as far as a
	// recording's stop can tell, from before it is stamped until the parser
	// has taken all of it and the emitter has captured the calls it
	// completed, into the recorder's queue: the stop waits for it
	// (syncMock BacklogBefore). Without this, the agent answers "clear" while
	// a gRPC call that ended before the stop is still in this pipeline, and
	// the stop ends the stream without it.
	building := syncMock.FromContextOrGlobal(ctx).BuildingTestCase

	// Forward client → dest, tee copies to channel
	go func() {
		defer close(clientFrameCh)
		forwardAndTee(clientConn, destConn, clientFrameCh, logger, "client→app", false, building)
	}()

	// Forward dest → client, tee copies to channel
	go func() {
		defer close(destFrameCh)
		forwardAndTee(destConn, clientConn, destFrameCh, logger, "app→client", true, building)
	}()

	// captureStream closes over ctx, t, appPort, synchronous, and mapping so
	// emitCompleteStreams does not need to carry those as parameters.
	captureStream := func(stream *pkg.HTTP2Stream) {
		Utils.CaptureGRPC(ctx, logger, t, stream, appPort, synchronous, mapping)
	}

	// Single emitter goroutine to avoid duplicate test case emission. Each
	// pass releases the chunks the parsers had handed it before it began
	// (parsed): every call those chunks completed was in the stream manager
	// by then, and the pass captures it.
	var emitMu sync.Mutex
	var parsedChunks []func()
	emit := func() {
		emitMu.Lock()
		done := parsedChunks
		parsedChunks = nil
		emitMu.Unlock()
		emitCompleteStreams(logger, sm, captureStream)
		for _, d := range done {
			d()
		}
	}
	emitCh := make(chan struct{}, 1)
	emitDone := make(chan struct{})
	go func() {
		defer close(emitDone)
		for range emitCh {
			emit()
		}
		emit()
	}()

	parsed := func(done []func()) {
		if len(done) > 0 {
			emitMu.Lock()
			parsedChunks = append(parsedChunks, done...)
			emitMu.Unlock()
		}
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
		parseFramesFromChan(ctx, logger, clientFrameCh, sm, false, parsed)
	}()

	// Parse app→client frames (responses)
	go func() {
		defer parseWg.Done()
		parseFramesFromChan(ctx, logger, destFrameCh, sm, true, parsed)
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
	// built, when set, says the chunk is through the pipeline: its parser has
	// taken all of it, and the emitter has captured what it completed (or it
	// was let go). See building in RecordIncoming.
	built func()
	// teeEnded marks the end of what the forwarder tees (memory pressure
	// stopped it for good): the parser gives up, as the rest of the frame it
	// is in the middle of will never come.
	teeEnded bool
}

// recordingPaused reports memory pressure (memoryguard): a variable so a test
// can stop the tee.
var recordingPaused = memoryguard.IsRecordingPaused

// release calls built, if set.
func release(built func()) {
	if built != nil {
		built()
	}
}

// forwardAndTee's building, when set, notes each chunk it tees as being built
// into a test case (syncMock BuildingTestCase), from before the chunk is
// stamped, at a time no later than its stamp: a stop that asks about a time
// before its stamp is told of it from then on, however long the forwarder
// waits between the two.
func forwardAndTee(src, dst net.Conn, ch chan<- frameChunk, logger *zap.Logger, direction string, stampAfterWrite bool, building func(time.Time) func()) {
	buf := make([]byte, 32*1024)
	teeActive := !recordingPaused()
	for {
		n, err := src.Read(buf)
		if n > 0 {
			var built func()
			if teeActive && building != nil {
				built = building(time.Now())
			}
			readAt := time.Now()
			if _, wErr := dst.Write(buf[:n]); wErr != nil {
				release(built)
				logger.Debug("forward write error",
					zap.String("direction", direction), zap.Error(wErr))
				return
			}
			timestamp := readAt
			if stampAfterWrite {
				timestamp = time.Now()
			}
			if teeActive {
				if recordingPaused() {
					teeActive = false
					release(built)
					ch <- frameChunk{teeEnded: true}
				} else {
					data := make([]byte, n)
					copy(data, buf[:n])
					ch <- frameChunk{
						data:      data,
						timestamp: timestamp,
						built:     built,
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
// received via channel, then feeds them to the DefaultStreamManager. After the
// frames of each chunk, it calls parsed with what releases the chunks it has
// taken all of (frameChunk.built), for the emitter to release once it has
// captured what their frames completed; parsed also wakes the emitter. When it
// stops, it hands parsed the chunks it took part of at once (it gave up
// mid-chunk, or the stream ended mid-frame), and releases those it never takes
// (drainFrameChunks) as they come: none waits for the connection to end.
func parseFramesFromChan(ctx context.Context, logger *zap.Logger, ch <-chan frameChunk, sm *pkg.DefaultStreamManager, isOutgoing bool, parsed func(built []func())) {
	var buffer []byte
	prefaceRead := isOutgoing

	// inBuffer is the chunks with bytes still in buffer, oldest first, by
	// where each ends in the stream; appended is how many bytes the stream
	// has given so far.
	type bufferedChunk struct {
		end   int64
		built func()
	}
	var inBuffer []bufferedChunk
	var appended int64
	// handOff hands parsed the chunks still in buffer: frames of them may be
	// in the stream manager, and the emitter's next pass captures what they
	// completed before it releases them.
	handOff := func() {
		if len(inBuffer) == 0 {
			return
		}
		done := make([]func(), 0, len(inBuffer))
		for _, c := range inBuffer {
			done = append(done, c.built)
		}
		inBuffer = nil
		parsed(done)
	}
	// giveUp stops parsing: what it took goes on at once, and what is left
	// on ch is released as it comes, until the forwarder closes ch.
	giveUp := func() {
		handOff()
		drainFrameChunks(ch)
	}
	defer handOff()

	for {
		var chunk frameChunk
		select {
		case <-ctx.Done():
			giveUp()
			return
		case next, ok := <-ch:
			if !ok {
				return
			}
			chunk = next
		}

		if chunk.teeEnded {
			giveUp()
			return
		}
		if len(chunk.data) == 0 {
			release(chunk.built)
			continue
		}
		buffer = append(buffer, chunk.data...)
		appended += int64(len(chunk.data))
		if chunk.built != nil {
			inBuffer = append(inBuffer, bufferedChunk{end: appended, built: chunk.built})
		}

		if !prefaceRead {
			if len(buffer) < len(http2.ClientPreface) {
				continue
			}
			if !bytes.Equal(buffer[:len(http2.ClientPreface)], []byte(http2.ClientPreface)) {
				logger.Debug("unexpected bytes instead of HTTP/2 preface")
				giveUp()
				return
			}
			buffer = buffer[len(http2.ClientPreface):]
			prefaceRead = true
		}

		handled := false
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
				giveUp()
				return
			}
			frameSize := int(frameLength) + 9
			if len(buffer) < frameSize {
				break
			}

			frame, consumed, err := pkg.ExtractHTTP2Frame(buffer[:frameSize])
			if err != nil {
				logger.Debug("frame parser finished", zap.Bool("isOutgoing", isOutgoing), zap.Error(err))
				giveUp()
				return
			}
			buffer = buffer[consumed:]
			if len(buffer) == 0 {
				buffer = nil
			}

			if err := sm.HandleFrame(frame, isOutgoing, chunk.timestamp); err != nil {
				logger.Debug("stream manager error", zap.Error(err))
			}
			handled = true
		}

		// The chunks taken all of: their frames are in the stream manager.
		taken := appended - int64(len(buffer))
		var done []func()
		for len(inBuffer) > 0 && inBuffer[0].end <= taken {
			done = append(done, inBuffer[0].built)
			inBuffer = inBuffer[1:]
		}
		if handled || len(done) > 0 {
			parsed(done)
		}
	}
}

// drainFrameChunks takes what is left on ch until it closes, releasing each
// chunk: nothing will be built of it.
func drainFrameChunks(ch <-chan frameChunk) {
	for c := range ch {
		release(c.built)
	}
}

// emitCompleteStreams checks the stream manager for complete request/response
// pairs and dispatches each via capture. Only called from the single emitter
// goroutine to prevent duplicate emissions. ctx, t, appPort, synchronous, and
// mapping are captured by the closure that RecordIncoming passes as capture.
func emitCompleteStreams(logger *zap.Logger, sm *pkg.DefaultStreamManager, capture func(*pkg.HTTP2Stream)) {
	if capture == nil || memoryguard.IsRecordingPaused() {
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
		capture(stream)
		sm.CleanupStream(stream.ID)
	}
}
