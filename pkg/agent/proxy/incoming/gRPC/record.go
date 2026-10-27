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
	"go.keploy.io/server/v3/pkg/agent/proxy/fakeconn"
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
	// recording's stop can tell, from before it is stamped until its frames
	// are handled and the emitter has captured the calls they completed,
	// into the recorder's queue (parseFramesFromChan): the stop waits for it
	// (syncMock BacklogBefore). Without this, the agent answers "clear" while
	// a gRPC call that ended before the stop is still in this pipeline, and
	// the stop ends the stream without it.
	building := syncMock.FromContextOrGlobal(ctx).BuildingTestCase
	// The latest instant a stop asked about: a read that ends a stream, noted
	// after it, is stamped later than it when the clock is within the wait's
	// bound of it (forwardAndTee).
	stopAsked := syncMock.FromContextOrGlobal(ctx).StopAsked

	// Forward client → dest, tee copies to channel
	go func() {
		defer close(clientFrameCh)
		forwardAndTee(clientConn, destConn, clientFrameCh, logger, "client→app", false, building, stopAsked)
	}()

	// Forward dest → client, tee copies to channel; what ends a stream is
	// stamped before its client can see it (toClient).
	go func() {
		defer close(destFrameCh)
		forwardAndTee(destConn, clientConn, destFrameCh, logger, "app→client", true, building, stopAsked)
	}()

	// captureStream closes over ctx, t, appPort, synchronous, and mapping so
	// emitCompleteStreams does not need to carry those as parameters.
	// The forwarder stamps each client chunk with the proxy's time.Now
	// before it writes it on (forwardAndTee), so a call's request stamp,
	// its window's start, is taken before the app sees it, on the proxy's
	// clock.
	windowCtx := Utils.WithWindowClock(ctx, fakeconn.NameProxyClock())
	captureStream := func(stream *pkg.HTTP2Stream) {
		Utils.CaptureGRPC(windowCtx, logger, t, stream, appPort, synchronous, mapping)
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

// frameChunk is a read the forwarder tees to a parser (forwardAndTee).
type frameChunk struct {
	data      []byte
	timestamp time.Time
	// paused: the memory guard had paused recording as the forwarder read
	// the chunk. A call whose request's first frame it completes is a call
	// the pause costs (pkg.HTTP2Stream.RequestPaused).
	paused bool
	// built, when set, says the chunk is through the pipeline: its frames
	// are handled, and the emitter has captured the calls they completed (or
	// it was let go). See building in RecordIncoming.
	built func()
}

// release calls built, if set.
func release(built func()) {
	if built != nil {
		built()
	}
}

// forwardAndTee reads from src, writes to dst (forwarding), and sends a copy
// to the channel for frame parsing. The channel send blocks if full to preserve
// HTTP/2 frame stream integrity (dropping bytes would corrupt parser alignment).
// This is safe because the parser is faster than network I/O, and ctx cancellation
// closes the connections which unblocks the reads.
//
// It tees the whole stream, through a pause of the memory guard too. A pause
// costs the calls that come during it, which the capture leaves out and
// counts (CaptureGRPC), never the connection: HTTP/2 cannot be read again
// from the middle of a stream (a frame cut in two, a header block whose HPACK
// table the parser never saw), so a tee that stopped for a pause stopped the
// capture of every later call on the connection, and gRPC clients pool their
// connections for as long as they run. A connection that opens during a
// pause is teed from its first byte for the same reason. Each chunk carries
// whether recording was paused as it was read (frameChunk.paused): that order
// of events, not the chunk's stamp, puts a call in a pause. A pause's span
// ends at the instant the pause does, and on a clock as coarse as Windows'
// the next call's request is stamped in that same tick; judged by its stamp,
// the first call after every pause was in it.
//
// Its building, when set, notes each chunk it tees as being built
// into a test case (syncMock BuildingTestCase) before the chunk is stamped,
// at a time read before the note: a stop that asks about a time at or after
// its stamp is told of it, however long the forwarder waits between the two.
//
// What the client sends the app (toClient false) is noted and stamped as it
// is read, before its write.
//
// What the app sends its client (toClient true) is noted and stamped after
// the write that may wait on the client, so a write blocked on a client that
// stopped reading holds no stop, but never after the client can see a stream's
// end. A call's window ends at the frame that ends its stream (its trailers'
// END_STREAM, at the end of their header block: endScanner): stamped once its
// client had it, the window ran on for as long as the forwarder waited to run
// again after its write, and a sequential client's next call, and the calls
// the app made for it, fell in it. So a read that ends any stream is written
// up to, not including, the last byte of the first frame in it that does; the
// read is then noted, stamped and handed to the parser whole, and the rest of
// it goes to the client once the clock reads past the stamp (waitPast). One stamp serves every end in the read: it is
// later than every byte of it the app sent, so each call's window holds every
// call the app made for it, and earlier than the client can see any of its
// ends, so it holds nothing the client does on one. The write of the rest
// waits on the client with nothing noted that it holds: the parser has the
// read already. A read that ends no stream is noted and stamped after its
// write, as before.
func forwardAndTee(src, dst net.Conn, ch chan<- frameChunk, logger *zap.Logger, direction string, toClient bool, building func(time.Time) func(), stopAsked func() time.Time) {
	buf := make([]byte, 32*1024)
	var ends endScanner
	// noteAndStamp notes a chunk as being built, then reads its stamp: a
	// stop that asks before the note is in finds a stamp later than its own
	// instant. On a clock as coarse as Windows' the stamp of a read that ends
	// a stream (ends), a call's end, can read the very instant a stop asked
	// about, which the stop counts as before it: one that asked before the
	// note was in heard "clear", and then a call from its instant came. So
	// that stamp is read again once the clock has passed the latest instant a
	// stop asked about (stopAsked), when it has not and is within the wait's
	// bound of it (clockWaitMax): an instant further ahead (a stop's clock
	// ahead of this one's) the wait could not pass, and would only hold the
	// read. Only a read that ends a stream gives a call its end.
	noteAndStamp := func(ends bool) (func(), time.Time) {
		var built func()
		if building != nil {
			built = building(clockNow())
		}
		at := clockNow()
		if ends && stopAsked != nil {
			if asked := stopAsked(); !asked.IsZero() && !at.After(asked) && asked.Sub(at) < clockWaitMax {
				waitPast(asked)
				at = clockNow()
			}
		}
		return built, at
	}
	write := func(b []byte, built func()) bool {
		if len(b) == 0 {
			return true
		}
		if _, err := dst.Write(b); err != nil {
			release(built)
			logger.Debug("forward write error",
				zap.String("direction", direction), zap.Error(err))
			return false
		}
		return true
	}
	// tee hands b, forwarded or about to be, to the parser stamped at, and
	// read while recording was paused or not.
	tee := func(b []byte, at time.Time, built func(), paused bool) {
		data := make([]byte, len(b))
		copy(data, b)
		ch <- frameChunk{data: data, timestamp: at, built: built, paused: paused} // blocking send — preserves stream integrity
	}
	for {
		n, err := src.Read(buf)
		if n > 0 {
			paused := memoryguard.IsRecordingPaused()
			b := buf[:n]
			first := -1 // just past the first frame in b that ends a stream
			if toClient {
				if e := ends.scan(b); len(e) > 0 {
					first = e[0]
				}
			}
			switch {
			case !toClient:
				built, at := noteAndStamp(false)
				if !write(b, built) {
					return
				}
				tee(b, at, built, paused)
			case first < 0:
				if !write(b, nil) {
					return
				}
				built, at := noteAndStamp(false)
				tee(b, at, built, paused)
			default:
				if !write(b[:first-1], nil) {
					return
				}
				built, at := noteAndStamp(true)
				tee(b, at, built, paused)
				waitPast(at)
				if !write(b[first-1:], nil) {
					return
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

// endScanner follows the HTTP/2 frames an app sends its client across the
// reads they arrive in, and finds where each frame that ends a stream ends: a
// DATA frame with END_STREAM, a HEADERS frame with END_STREAM and
// END_HEADERS, or the CONTINUATION frame that ends a header block a HEADERS
// frame with END_STREAM began (RFC 9113 §8.1). Its last byte is what lets the
// client see the end of its call. The app's side of a connection is frames
// from its first byte (no preface).
type endScanner struct {
	hdr  [frameHeaderSize]byte
	have int  // bytes held of the header of the frame in progress
	left int  // bytes left of the payload of the frame in progress
	ends bool // the frame in progress ends its stream
	// blockEnds: the header block in progress was begun by a HEADERS frame
	// with END_STREAM and without END_HEADERS. Its stream ends with the
	// block (RFC 9113 §8.1), at the CONTINUATION frame that ends it: the
	// client sees the end only with that frame, and the call's end is that
	// frame's time (pkg.DefaultStreamManager).
	blockEnds bool
}

// frameHeaderSize is an HTTP/2 frame header's length (RFC 9113 §4.1).
const frameHeaderSize = 9

// scan follows b, the next bytes the app sent, and returns, in order, the
// offset in b just past the last byte of each frame that ends a stream which
// b completes.
func (s *endScanner) scan(b []byte) []int {
	var out []int
	for i := 0; i < len(b); {
		if s.have < frameHeaderSize {
			k := copy(s.hdr[s.have:], b[i:])
			s.have += k
			i += k
			if s.have < frameHeaderSize {
				break
			}
			s.left = int(s.hdr[0])<<16 | int(s.hdr[1])<<8 | int(s.hdr[2])
			typ, flags := http2.FrameType(s.hdr[3]), http2.Flags(s.hdr[4])
			switch typ {
			case http2.FrameData:
				s.ends = flags.Has(http2.FlagDataEndStream)
			case http2.FrameHeaders:
				endsStream := flags.Has(http2.FlagHeadersEndStream)
				s.ends = endsStream && flags.Has(http2.FlagHeadersEndHeaders)
				s.blockEnds = endsStream && !flags.Has(http2.FlagHeadersEndHeaders)
			case http2.FrameContinuation:
				s.ends = s.blockEnds && flags.Has(http2.FlagContinuationEndHeaders)
				if flags.Has(http2.FlagContinuationEndHeaders) {
					s.blockEnds = false
				}
			default:
				s.ends = false
			}
		}
		k := min(s.left, len(b)-i)
		s.left -= k
		i += k
		if s.left == 0 {
			if s.ends {
				out = append(out, i)
			}
			s.have = 0
		}
	}
	return out
}

// parseFramesFromChan reconstructs HTTP/2 frames from timestamped byte chunks
// received via channel, then feeds them to the DefaultStreamManager.
//
// Each chunk's note (frameChunk.built) is let go once its frames are handled:
// to parsed when it completed any, for the emitter to release once it has
// decided what they completed (parsed also wakes the emitter), and at once
// when it completed none. A chunk that ends mid-frame needs no note past
// that: the frame is stamped with the time of the chunk that completes it
// (HandleFrame), whose own note covers it. Held instead, it would keep a stop
// waiting on a client that stopped reading mid-response, whose next chunk is
// never forwarded. When the parser stops, what it never takes is released as
// it comes (drainFrameChunks).
func parseFramesFromChan(ctx context.Context, logger *zap.Logger, ch <-chan frameChunk, sm *pkg.DefaultStreamManager, isOutgoing bool, parsed func(built []func())) {
	var buffer []byte
	prefaceRead := isOutgoing
	// One reader for all of the direction's frames: a header block split
	// across a HEADERS frame and CONTINUATION frames is read as one.
	frames := pkg.NewFrameReader()

	// letGo lets go of a chunk's note once its frames are handled.
	letGo := func(c frameChunk, handled bool) {
		switch {
		case handled:
			var done []func()
			if c.built != nil {
				done = []func(){c.built}
			}
			parsed(done)
		default:
			release(c.built)
		}
	}

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
			release(chunk.built)
			continue
		}
		buffer = append(buffer, chunk.data...)

		if !prefaceRead {
			if len(buffer) < len(http2.ClientPreface) {
				release(chunk.built)
				continue
			}
			if !bytes.Equal(buffer[:len(http2.ClientPreface)], []byte(http2.ClientPreface)) {
				logger.Debug("unexpected bytes instead of HTTP/2 preface")
				release(chunk.built)
				drainFrameChunks(ch)
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
				letGo(chunk, handled)
				drainFrameChunks(ch)
				return
			}
			frameSize := int(frameLength) + 9
			if len(buffer) < frameSize {
				break
			}

			frame, err := frames.ReadFrame(buffer[:frameSize])
			if err != nil {
				logger.Debug("frame parser finished", zap.Bool("isOutgoing", isOutgoing), zap.Error(err))
				letGo(chunk, handled)
				drainFrameChunks(ch)
				return
			}
			buffer = buffer[frameSize:]
			if len(buffer) == 0 {
				buffer = nil
			}

			if err := sm.HandleFrameRead(frame, isOutgoing, chunk.timestamp, chunk.paused); err != nil {
				logger.Debug("stream manager error", zap.Error(err))
			}
			handled = true
		}
		letGo(chunk, handled)
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
//
// Every complete call is decided in the pass that finds it, under memory
// pressure too, when capture leaves it out (CaptureGRPC): a pass that skipped
// it would leave it for a later pass after the chunks it was built of were
// released, so a stop told nothing was pending could be followed by it.
func emitCompleteStreams(logger *zap.Logger, sm *pkg.DefaultStreamManager, capture func(*pkg.HTTP2Stream)) {
	if capture == nil {
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
