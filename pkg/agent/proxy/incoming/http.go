package proxy

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"go.keploy.io/server/v3/pkg"
	hooksUtils "go.keploy.io/server/v3/pkg/agent/hooks/conn"
	"go.keploy.io/server/v3/pkg/agent/memoryguard"
	syncMock "go.keploy.io/server/v3/pkg/agent/proxy/syncMock"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

var isIngressRecordingPaused = memoryguard.IsRecordingPaused

type httpCaptureState struct {
	mu        sync.Mutex
	maxBytes  int
	usedBytes int
	aborted   bool
}

func newHTTPCaptureState(maxBytes int) *httpCaptureState {
	return &httpCaptureState{maxBytes: maxBytes}
}

func (s *httpCaptureState) reserve(n int) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.aborted {
		return false
	}
	if isIngressRecordingPaused() {
		s.aborted = true
		return false
	}
	if s.maxBytes > 0 && s.usedBytes+n > s.maxBytes {
		s.aborted = true
		return false
	}

	s.usedBytes += n
	return true
}

func (s *httpCaptureState) isAborted() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.aborted
}

type httpBodyCaptureBuffer struct {
	state *httpCaptureState
	buf   bytes.Buffer
}

func (b *httpBodyCaptureBuffer) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if b.state == nil || !b.state.reserve(len(p)) {
		b.buf = bytes.Buffer{} // release backing array for GC
		return len(p), nil
	}

	if _, err := b.buf.Write(p); err != nil {
		return 0, err
	}

	return len(p), nil
}

func (b *httpBodyCaptureBuffer) Bytes() []byte {
	return b.buf.Bytes()
}

func (b *httpBodyCaptureBuffer) Reset() {
	b.buf.Reset()
}

// timedChunk pairs a forwarded byte slice with the time the proxy's
// io.Copy goroutine read it from the source socket. asyncPipeFeeder.Read
// stores chunk.readAt into lastReadNano BEFORE returning any bytes from
// the chunk, so a subsequent LastReadTime call from the same parser
// goroutine sees the chunk's timestamp.
type timedChunk struct {
	data   []byte
	readAt time.Time
}

// wireTimeConn wraps a net.Conn and stamps the time of the most recent
// non-empty Read on an atomic field. The sync-path HTTP capture loop
// uses this to sample the wire-arrival time of an HTTP request: when
// http.ReadRequest had to drive at least one new socket Read during
// this iteration, the latest stamp is the tightest available upper
// bound on actual wire arrival of THIS request's bytes, and is < the
// time at which parsing completed.
//
// The capture loop clamps the resulting timestamp at no earlier than
// the loop iteration's entry time (iterStart) — see the call site for
// why: on HTTP/1.1 keepalive, bufio.Reader may serve request N
// entirely from bytes prefetched during the prior iteration's Read
// (which was consuming request N-1's tail). In that case lastReadNano
// is from request N-1's read window — DURING the previous test's
// handler — so using it raw would push the per-test mock-window left
// edge backwards across the previous test boundary.
//
// This matters because per-test mock windowing (mockmanager
// GetFilteredMocksInWindow) does strict containment of recorded
// invocation reqTimestampMock against [HTTPReq.Timestamp,
// HTTPResp.Timestamp]. If the HTTP recorder stamps reqTimestamp from
// time.Now() AFTER parse, downstream parser recorders (postgres v3)
// that stamp their own captures at decode time can produce
// reqTimestampMock values a few microseconds EARLIER than the HTTP
// reqTimestamp, falling outside the window's left edge and causing
// otherwise-correct mocks to be filtered out at replay. See
// https://github.com/keploy/integrations/pull/151 for the related
// pgtype-tour-postgres flake symptom (`candidates: 0` for SQL hashes
// that exist in the recorded mocks.yaml).
type wireTimeConn struct {
	net.Conn
	lastReadNano atomic.Int64
}

func (c *wireTimeConn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	if n > 0 {
		c.lastReadNano.Store(time.Now().UnixNano())
	}
	return n, err
}

// LastReadTime returns the time of the most recent non-empty Read on
// the wrapped conn, or the zero time if no bytes have been read yet.
func (c *wireTimeConn) LastReadTime() time.Time {
	n := c.lastReadNano.Load()
	if n == 0 {
		return time.Time{}
	}
	return time.Unix(0, n)
}

// Global cap on the bytes held in flight across every asyncPipeFeeder's
// channel. The per-feeder channel is sized to tolerate large response
// bodies (5 MB needs ~160 slots at 32 KB/chunk), so under stress workloads
// with many concurrent connections (go-memory-load: hundreds of in-flight
// requests, parser unable to drain channels at wire speed) memory growth
// per feeder × N feeders rapidly exceeds the agent's 250 MiB record-time
// guard. Counter is decremented when Read pulls a chunk off a channel
// or when shutdown drains residual chunks; pre-check in Write is the
// fast path that prevents new allocations once the cap is reached.
//
// Value chosen to leave headroom for the rest of the agent (proxy state,
// mock storage, postgres v3 cohort indexes) under the 200 MiB memory
// limit set by the CI memory-load harness, while still allowing >2 MB of
// in-flight capture per active connection in the steady state.
const feederGlobalLimitBytes = 80 * 1024 * 1024 // 80 MB

var feederInFlightBytes atomic.Int64

// captureHookConcurrency caps the number of CaptureHook goroutines
// running at the same time across every parseStreamingHTTP invocation.
// Each goroutine takes a reference to the parsed *http.Request /
// *http.Response (each carrying up to MaxTestCaseSize=5MB of body) and
// runs io.ReadAll a second time inside Capture to materialise its own
// reqBody/respBody copy, so peak transient memory per in-flight
// goroutine is ~10 MB. Without a cap the parser launches one goroutine
// per HTTP exchange unconditionally; the go-memory-load workload
// (k6 firing 42 concurrent VUs against /large_payload endpoints)
// piles hundreds of these goroutines up faster than the unbuffered
// tcChan can drain them, taking the agent past the 250 MiB CI guard
// even with the 80 MB feeder cap holding firm. 16 in-flight × ~10 MB
// = ~160 MB worst-case, leaving headroom inside the 200 MB Docker
// memory limit alongside the feeder cap (80 MB) and the rest of agent
// state.
const captureHookConcurrency = 16

var captureHookSem = make(chan struct{}, captureHookConcurrency)

// asyncPipeFeeder is the parser-side reader for streaming HTTP capture.
// io.Copy on the forwarding path writes into it via Write (non-blocking,
// drops on backpressure); the parseStreamingHTTP goroutine reads from
// it via Read.
//
// Earlier revisions placed an io.Pipe + a separate bridge goroutine
// between Write and Read so the forwarding path was decoupled from
// parser stalls. That design had a window-attribution bug: the bridge
// stored chunk N+1's readAt into lastReadNano BEFORE pipe.Write blocked
// on chunk N+1, so a parser that finished consuming chunk N and then
// queried LastReadTime could observe chunk N+1's ts (the *next*
// request's arrival time) as the *current* request's reqTimestamp.
// Empty-body GETs hit this every time because the parser does not
// trigger another pipe.Read between ReadRequest's return and its
// LastReadTime call, so there is no scheduling barrier protecting
// the read of lastReadNano. The integration listmonk-postgres workload
// reproduced it as 403-invalid-session failures on tightly packed
// per-test GETs whose session-lookup mocks ended up attributed to the
// previous test's window.
//
// The simpler design below keeps the channel for backpressure but
// removes the bridge: Read pulls one chunk at a time from the channel
// and updates lastReadNano synchronously before returning data. The
// timestamp store and the data hand-off happen on the same goroutine
// as the parser's eventual LastReadTime call, so there is no race.
//
// Graceful degradation is unchanged: memory pressure, exceeding
// maxSize, or a full channel cause the feeder to silently stop
// capturing while forwarding continues unimpacted.
type asyncPipeFeeder struct {
	ch             chan timedChunk
	closed         atomic.Bool
	written        atomic.Int64
	maxSize        int64
	logger         *zap.Logger
	closeOnce      sync.Once
	parserExited   chan struct{}
	parserExitOnce sync.Once

	// cur and lastReadNano are touched only from the parser goroutine
	// (the single Read caller), so no synchronization is needed for
	// either. The atomic is for Go's memory model rather than any
	// cross-goroutine visibility concern: we want LastReadTime callers
	// from the same goroutine to see the most recent store, which a
	// plain field would also provide, but atomic.Int64 keeps the type
	// honest if a future caller queries from another goroutine.
	cur          []byte
	lastReadNano atomic.Int64
}

// newAsyncPipeFeeder creates a feeder used as both the forwarding-side
// io.Writer and the parser-side io.Reader. There is no separate bridge
// goroutine; Read pulls chunks from the channel directly.
func newAsyncPipeFeeder(maxSize int, logger *zap.Logger) *asyncPipeFeeder {
	return &asyncPipeFeeder{
		// 512 slots ≈ 16MB at 32KB/chunk. Must be large enough that
		// the channel never overflows during normal operation — a single
		// 5MB response body needs ~160 slots, and brief pipeline stalls
		// (parser transitioning between request/response reads) can
		// cause data to accumulate. Overflowing kills capture for the
		// rest of the connection because the HTTP stream becomes
		// unrecoverable.
		ch:           make(chan timedChunk, 512),
		maxSize:      int64(maxSize),
		logger:       logger,
		parserExited: make(chan struct{}),
	}
}

// Read returns bytes from the current chunk's data, pulling a new chunk
// from the channel when the current one is exhausted. The new chunk's
// readAt is stored into lastReadNano BEFORE any of its data is returned,
// so a parser that calls LastReadTime after a successful Read sees the
// timestamp of the chunk that delivered the most recent byte.
//
// Read is the only consumer of the channel and only ever runs on the
// parseStreamingHTTP goroutine; cur and lastReadNano are not shared.
// On channel close, Read returns io.EOF, which propagates through the
// bufio.Reader to terminate ReadRequest/ReadResponse cleanly.
func (f *asyncPipeFeeder) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if len(f.cur) == 0 {
		chunk, ok := <-f.ch
		if !ok {
			return 0, io.EOF
		}
		// Bytes leave the feeder's in-flight pool the moment they're
		// pulled off the channel — they live on the parser goroutine's
		// stack/heap until consumed and freed naturally by GC.
		feederInFlightBytes.Add(-int64(len(chunk.data)))
		f.cur = chunk.data
		if !chunk.readAt.IsZero() {
			f.lastReadNano.Store(chunk.readAt.UnixNano())
		}
	}
	n := copy(p, f.cur)
	f.cur = f.cur[n:]
	return n, nil
}

// Write copies p and enqueues it for the parser. It never blocks the
// caller. Called only from the forwarding goroutine (via io.MultiWriter).
func (f *asyncPipeFeeder) Write(p []byte) (int, error) {
	if f.closed.Load() {
		return len(p), nil
	}
	if isIngressRecordingPaused() {
		f.shutdown() // close channel so the parser exits promptly
		return len(p), nil
	}
	newTotal := f.written.Add(int64(len(p)))
	if f.maxSize > 0 && newTotal > f.maxSize {
		f.shutdown()
		return len(p), nil
	}
	// Pre-allocation pressure check: if the global feeder in-flight
	// counter is already saturated, refuse this chunk and shut the
	// feeder down rather than allocating into a memory-pressured agent.
	// Approximate on the Load side (a parallel feeder could push us
	// over by one chunk) — exactly accurate increment happens below
	// after the channel send succeeds. Captures-lost is an acceptable
	// trade for not OOM'ing the agent under stress workloads.
	chunkSize := int64(len(p))
	if feederInFlightBytes.Load()+chunkSize > feederGlobalLimitBytes {
		f.shutdown()
		if f.logger != nil {
			f.logger.Debug("Global feeder in-flight cap reached — dropping remaining capture on this connection.",
				zap.Int64("global_limit_bytes", feederGlobalLimitBytes),
				zap.Int64("current_in_flight", feederInFlightBytes.Load()),
				zap.Int("chunk_size", len(p)),
			)
		}
		return len(p), nil
	}
	// Copy data — the original slice belongs to io.Copy's reusable buffer.
	// Capture the arrival time NOW (right after io.Copy.Read returned bytes
	// from the source socket); this is the timestamp the parser will see
	// via LastReadTime once these bytes are consumed downstream.
	buf := make([]byte, len(p))
	copy(buf, p)
	chunk := timedChunk{data: buf, readAt: time.Now()}
	select {
	case f.ch <- chunk:
		// Account for the bytes now sitting in the channel; Read or the
		// shutdown drain decrement on consumption.
		feederInFlightBytes.Add(chunkSize)
	default:
		// Channel full — parser can't keep up. Stop capture entirely
		// so the parser goroutine sees io.EOF and exits.
		f.shutdown()
		if f.logger != nil {
			f.logger.Debug("Capture channel full — dropping remaining test cases on this connection. "+
				"This may indicate responses too large for the channel buffer.",
				zap.Int64("total_bytes_written", f.written.Load()),
				zap.Int("chunk_size", len(p)),
				zap.Int("channel_capacity", cap(f.ch)),
			)
		}
	}
	return len(p), nil
}

// shutdown idempotently stops the feeder: sets the closed flag and closes
// the channel. Safe to call from both Write() (capture disabled mid-stream)
// and Close() (connection ended). The sync.Once ensures the channel is
// closed exactly once regardless of which path fires first.
//
// After close, residual chunks still sitting in f.ch are drained off-thread
// and their bytes returned to the global in-flight counter. Without the
// drain those bytes would stay accounted-for (counter never decrements)
// even though the parser goroutine has already exited on io.EOF — which
// would slowly leak the global cap over the lifetime of the agent.
//
// The drain MUST wait for the parser to exit before consuming chunks. If the
// drain races the parser, it can steal chunks that the parser has not yet
// read — `<-f.ch` happily returns buffered chunks even after the channel is
// closed, so whichever goroutine wins the read takes the data. Under
// short-lived HTTP/1.0 + Connection: close exchanges (response arrives,
// upstream closes immediately, shutdown fires) the drain frequently wins on
// connections whose parser hasn't yet been scheduled by the Go runtime,
// silently dropping a request/response pair from the test set. python-schema-match
// reproduced this as a missing capture (/edge/nested_null) under the burst of
// 12 sequential urllib calls. Sequencing the drain after the parser's exit
// removes the race while still reclaiming the in-flight counter.
func (f *asyncPipeFeeder) shutdown() {
	f.closed.Store(true)
	f.closeOnce.Do(func() {
		close(f.ch)
		go func() {
			<-f.parserExited
			for chunk := range f.ch {
				feederInFlightBytes.Add(-int64(len(chunk.data)))
			}
		}()
	})
}

// Close signals the parser goroutine to exit by closing the chunk channel.
// Must be called after the forwarding goroutine finishes (after io.Copy returns).
func (f *asyncPipeFeeder) Close() {
	f.shutdown()
}

// signalParserExit must be called by the parser goroutine on exit (typically
// via defer) so the shutdown drain can safely proceed without racing the
// parser for channel data. Idempotent — safe to call multiple times. If
// no parser is ever attached to this feeder (e.g. capture disabled before
// the goroutine launches) the deferred close in handleHttp1ZeroCopy must
// still fire this so shutdown's drain doesn't block forever.
func (f *asyncPipeFeeder) signalParserExit() {
	f.parserExitOnce.Do(func() { close(f.parserExited) })
}

// LastReadTime returns the arrival timestamp of the most recent chunk
// that Read returned bytes from. Zero time means no chunk has been read
// yet — callers should fall back to time.Now() in that case.
//
// Intended call site: from the parser goroutine, immediately after
// http.ReadRequest / http.ReadResponse returns. Because the timestamp
// store happens inside Read (same goroutine, before bytes are returned),
// the parser is guaranteed to see the right chunk's timestamp without
// any synchronization beyond Go's memory model.
func (f *asyncPipeFeeder) LastReadTime() time.Time {
	n := f.lastReadNano.Load()
	if n == 0 {
		return time.Time{}
	}
	return time.Unix(0, n)
}

func serializeCapturedRequest(req *http.Request, body []byte) ([]byte, error) {
	clone := new(http.Request)
	*clone = *req
	clone.Header = req.Header.Clone()
	clone.Trailer = req.Trailer.Clone()
	clone.GetBody = nil
	if len(body) == 0 {
		clone.Body = http.NoBody
	} else {
		clone.Body = io.NopCloser(bytes.NewReader(body))
	}

	var buf bytes.Buffer
	if err := clone.Write(&buf); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func serializeCapturedResponse(resp *http.Response, body []byte) ([]byte, error) {
	clone := new(http.Response)
	*clone = *resp
	clone.Header = resp.Header.Clone()
	clone.Trailer = resp.Trailer.Clone()
	if len(body) == 0 {
		clone.Body = http.NoBody
	} else {
		clone.Body = io.NopCloser(bytes.NewReader(body))
	}

	var buf bytes.Buffer
	if err := clone.Write(&buf); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func (pm *IngressProxyManager) handleHttp1Connection(ctx context.Context, clientConn net.Conn, newAppAddr string, logger *zap.Logger, t chan *models.TestCase, sem chan struct{}, appPort uint16) {
	var acquiredLock bool

	// 1. Non-Blocking Concurrency Control
	if pm.synchronous {
		// Sync mode (Concurrency 1): Blocks until the slot is free
		select {
		case sem <- struct{}{}:
			acquiredLock = true
		case <-ctx.Done():
			return
		}
	} else if pm.sampling && pm.samplingSem != nil {
		// Sampling mode (Concurrency 5): Non-blocking lock attempt
		select {
		case pm.samplingSem <- struct{}{}:
			acquiredLock = true
		case <-ctx.Done():
			return
		default:
			// Non-blocking bypass: The 5 slots are FULL.
			// We do not block. We set acquiredLock to false.
			// This connection will be proxied normally and marked closed, but WILL NOT be captured.
			acquiredLock = false
		}
	}

	var releaseOnce sync.Once
	releaseLock := func() {
		if acquiredLock {
			releaseOnce.Do(func() {
				if pm.synchronous {
					<-sem
				} else if pm.sampling && pm.samplingSem != nil {
					<-pm.samplingSem
				}
			})
		}
	}
	// Ensure lock is released eventually if we exit early or finish normally
	defer releaseLock()

	// Get the actual destination address
	finalAppAddr := pm.getActualDestination(ctx, clientConn, newAppAddr, logger)

	// Determine the correct port for the test case:
	// On Windows, getActualDestination resolves the real destination dynamically,
	// so we extract the port from the resolved address.
	// On non-Windows (Linux/Docker), getActualDestination returns the fallback (newAppAddr)
	// which contains the eBPF-redirected port, NOT the original app port.
	// In that case, we use the passed-in appPort which carries the correct OrigAppPort.
	actualPort := appPort
	if finalAppAddr != newAppAddr {
		// Destination was dynamically resolved (Windows) — extract port from resolved address
		actualPort = extractPortFromAddr(finalAppAddr, appPort)
	}

	// Dial Upstream
	upConn, err := net.DialTimeout("tcp4", finalAppAddr, 3*time.Second)
	if err != nil {
		logger.Error("Failed to connect to upstream application. Verify the application is listening on the resolved address.",
			zap.String("final_app_addr", finalAppAddr),
			zap.Error(err),
		)
		return
	}
	defer upConn.Close()

	// forceCloseMode: only sync mode needs the traditional HTTP parsing loop
	// (strict one-at-a-time ordering with forced close). Sampling mode now
	// uses the zero-copy path for both tracked and bypass connections.
	forceCloseMode := pm.synchronous

	if !forceCloseMode {
		// Sampling bypass: no lock, no capture — raw TCP passthrough.
		if pm.sampling && !acquiredLock {
			forwardRawTCP(ctx, clientConn, upConn)
			return
		}
		// Normal mode OR sampling-tracked: zero-copy TCP passthrough with
		// streaming capture. Forwarding runs at wire speed via io.Copy;
		// capture is fully decoupled via non-blocking asyncPipeFeeders.
		// For sampling-tracked, the semaphore stays held (via defer
		// releaseLock) until the connection closes, limiting concurrent
		// captures to the configured slot count.
		if !pm.sampling {
			releaseLock() // Normal mode has no sampling lock to hold
		}
		pm.handleHttp1ZeroCopy(ctx, clientConn, upConn, logger, t, actualPort)
		return
	}

	// Wrap clientConn so the sync-path bufio.Reader's underlying socket
	// reads stamp wire-arrival time onto wireConn.lastReadNano. We sample
	// that AFTER http.ReadRequest returns rather than time.Now() so the
	// recorded HTTPReq.Timestamp tracks when the request bytes arrived
	// on the wire — not when parsing completed. Strict per-test windowing
	// of downstream parser captures (postgres v3, etc.) depends on this
	// not lagging the wire by parser-iteration time + bufio fill jitter.
	wireConn := &wireTimeConn{Conn: clientConn}
	clientReader := bufio.NewReader(wireConn)
	upstreamReader := bufio.NewReader(upConn)

	for {
		// iterStart is the loop entry time, captured BEFORE ReadRequest
		// blocks. It serves two purposes downstream:
		//   1. If ReadRequest blocks waiting for bytes, iterStart < the
		//      eventual wireConn.LastReadTime; the LastReadTime path is
		//      preferred — it's a tighter upper bound on actual wire
		//      arrival.
		//   2. On HTTP/1.1 keepalive, bufio.Reader may serve request N
		//      ENTIRELY from bytes that arrived during the read which
		//      consumed request N-1's tail. In that case
		//      wireConn.LastReadTime is from request N-1's read window —
		//      i.e. DURING the previous test's handler. Using it as
		//      request N's reqTimestamp pushes the per-test mock-window
		//      left edge backwards into the previous test's territory,
		//      so mocks captured during test N-1's handler fall inside
		//      both windows. Clamping the floor at iterStart prevents
		//      that: iterStart is always AFTER the previous iteration
		//      finished (i.e. after the previous response was written),
		//      so the previous test's mocks are guaranteed to be outside
		//      this test's window.
		iterStart := time.Now()
		req, err := http.ReadRequest(clientReader)
		if err != nil {
			if !errors.Is(err, io.EOF) {
				logger.Debug("Failed to read client request; ignoring this connection. Verify the client is sending valid HTTP if this persists.", zap.Error(err))
			}
			return
		}
		// LastReadTime() is the timestamp of the most recent non-empty
		// underlying socket Read. If a Read fired during this iteration
		// (the common case — bufio buffer was empty when ReadRequest
		// was called and had to refill from the socket), LastReadTime
		// > iterStart and is the tightest available upper bound on
		// when THIS request's bytes arrived on the wire. If no Read
		// fired during this iteration (bufio served entirely from a
		// buffer prefetched on a prior iteration's Read), LastReadTime
		// is from that prior iteration — possibly DURING the previous
		// test's handler — so we fall back to iterStart, which is after
		// the previous response was written and is a safe left edge for
		// the per-test mock window.
		reqTimestamp := wireConn.LastReadTime()
		if !reqTimestamp.After(iterStart) {
			reqTimestamp = iterStart
		}

		// streamingExchange tracks whether EITHER side (request or response)
		// lacked a concrete Content-Length or used Transfer-Encoding:
		// chunked. This covers the full "no upfront size known" superset
		// that needs early lock release. At the debug log site below we
		// also report the narrower 'chunked_transfer' bit separately so
		// operators can distinguish true chunked encoding from simple
		// unknown-length streams — the flag name used to be 'chunked',
		// which conflated the two.
		var streamingExchange bool = false
		// pressureCloseMode unifies forceCloseMode with memory pressure.
		// When true, expected close errors are handled gracefully (DEBUG level),
		// the sampling lock is released early, and the loop exits after one exchange.
		pressureCloseMode := forceCloseMode || isIngressRecordingPaused()
		captureEnabled := !isIngressRecordingPaused()

		// Request modifications for sync/sampling modes.
		if forceCloseMode {
			if req.ContentLength == -1 || isChunked(req.TransferEncoding) {
				// Release the lock early for streaming exchanges so a
				// single slow/streaming upload doesn't wedge the sync
				// semaphore. Capture stays enabled: tee reads the
				// already-decoded body bytes, and downstream capture-budget
				// checks on the request/response capture buffers handle
				// genuinely oversized streams.
				releaseLock()
				streamingExchange = true
			} else if captureEnabled && pm.synchronous && acquiredLock {
				mgr := syncMock.Get()
				if !mgr.GetFirstReqSeen() {
					mgr.SetFirstRequestSignaled()
				}
			}

			// Mark the connection closed for keep-alive in these modes even if
			// we are only proxying and not capturing.
			req.Close = true
			req.Header.Set("Connection", "close")
		}
		// Note: the pressureCloseMode && !forceCloseMode branch was removed
		// because forceCloseMode is always true here (the !forceCloseMode
		// path returns early via handleHttp1ZeroCopy/forwardRawTCP).

		// Determine whether capture is eligible for this exchange.
		// Chunked-encoded exchanges are still captured: the tee reads the
		// decoded body stream, and oversized bodies are rejected later by
		// the capture-budget check on reqCapture/respCapture.Truncated().
		captureEligible := captureEnabled && (!pm.sampling || acquiredLock)

		// Re-check memory pressure right before attaching capture buffers.
		if captureEligible && isIngressRecordingPaused() {
			pressureCloseMode = true
			captureEligible = false
			captureEnabled = false
			releaseLock()
			req.Close = true
			req.Header.Set("Connection", "close")
		}

		reqCapture := newCaptureBuffer(maxHTTPBodyCaptureBytes)
		if captureEligible && req.Body != nil && req.Body != http.NoBody {
			req.Body = newTeeReadCloser(req.Body, reqCapture)
		}

		if err := req.Write(upConn); err != nil {
			if pressureCloseMode && isIngressExpectedCloseErr(err) {
				logger.Debug("HTTP/1 ingress request write ended during close-under-pressure path", zap.Error(err))
				req.Body.Close()
				return
			}
			logger.Error("Failed to forward request to upstream. Verify the upstream application is running and reachable at the resolved address.",
				zap.Error(err),
				zap.Int64("request_bytes_seen", reqCapture.Total()),
				zap.Bool("request_capture_truncated", reqCapture.Truncated()),
			)
			req.Body.Close()
			return
		}
		req.Body.Close() // Close explicitly to avoid defer leak in loop.

		// Re-check memory pressure after forwarding the request.
		if !pressureCloseMode && isIngressRecordingPaused() {
			pressureCloseMode = true
			captureEnabled = false
			releaseLock()
		}

		resp, err := http.ReadResponse(upstreamReader, req)
		if err != nil {
			if pressureCloseMode && isIngressExpectedCloseErr(err) {
				logger.Debug("HTTP/1 ingress upstream closed while finishing close-under-pressure path", zap.Error(err))
				return
			}
			logger.Error("Failed to read upstream response. Check upstream application health and network connectivity.",
				zap.Error(err),
				zap.Duration("time_since_request_received", time.Since(reqTimestamp)),
				zap.Int64("request_bytes_seen", reqCapture.Total()),
			)
			return
		}

		// Response modifications for sync/sampling modes.
		if forceCloseMode {
			if resp.ContentLength == -1 || isChunked(resp.TransferEncoding) {
				// Release the sync/sampling lock early on streaming
				// responses so a slow upstream doesn't wedge the slot,
				// but keep capture enabled — the tee reads decoded
				// body bytes and the capture-budget check handles
				// genuinely oversized streams.
				releaseLock()
				streamingExchange = true
			}

			resp.Close = true
			resp.Header.Set("Connection", "close")
		}
		respTimestamp := time.Now()

		// Re-evaluate capture eligibility after response headers.
		// Also re-check memory pressure in case it started mid-exchange.
		captureEligible = captureEnabled && (!pm.sampling || acquiredLock)
		if captureEligible && isIngressRecordingPaused() {
			pressureCloseMode = true
			captureEligible = false
			captureEnabled = false
			releaseLock()
			resp.Close = true
			resp.Header.Set("Connection", "close")
		}

		respCapture := newCaptureBuffer(maxHTTPBodyCaptureBytes)
		if captureEligible && resp.Body != nil && resp.Body != http.NoBody {
			resp.Body = newTeeReadCloser(resp.Body, respCapture)
		}

		if err := resp.Write(clientConn); err != nil {
			if pressureCloseMode && isIngressExpectedCloseErr(err) {
				logger.Debug("HTTP/1 ingress client connection closed while finishing close-under-pressure path", zap.Error(err))
				resp.Body.Close()
				return
			}
			logger.Error("Failed to forward response to client. The client may have closed the connection before the response was fully written.",
				zap.Error(err),
				zap.Int64("response_bytes_seen", respCapture.Total()),
				zap.Bool("response_capture_truncated", respCapture.Truncated()),
				zap.Duration("exchange_duration", time.Since(reqTimestamp)),
			)
			resp.Body.Close()
			return
		}
		resp.Body.Close() // Close explicitly.

		// Final memory pressure check before capture evaluation.
		if !pressureCloseMode && isIngressRecordingPaused() {
			pressureCloseMode = true
			captureEnabled = false
			releaseLock()
		}

		shouldCapture := captureEnabled
		if forceCloseMode {
			// Previously: chunked exchanges were dropped here outright.
			// That caused legitimate chunked-encoded REST responses
			// (very common for JSON APIs that omit Content-Length) to
			// be silently skipped at record time, leading to the
			// "1-of-25 captures" symptom on stress runs. Chunked
			// exchanges are now captured normally; the downstream
			// capture-budget check (reqCapture/respCapture.Truncated())
			// rejects genuinely oversized streams.
			if pm.sampling && !acquiredLock {
				shouldCapture = false
			}
		}
		// Skip capture if memory pressure kicked in during the exchange
		if shouldCapture && isIngressRecordingPaused() {
			shouldCapture = false
		}

		if shouldCapture && (reqCapture.Truncated() || respCapture.Truncated()) {
			// chunked_transfer is the narrower bit (actual Transfer-Encoding:
			// chunked on either side). streaming_exchange is the superset
			// that also captures unknown-length streams (Content-Length == -1
			// without chunked TE). Reporting both lets operators tell
			// "chunked JSON API response" apart from "unknown-length
			// upload" when triaging capture-budget trips.
			//
			// The message deliberately does NOT say "while streaming" — a
			// large fixed Content-Length body can also trip this branch
			// (streaming_exchange=false, chunked_transfer=false). The two
			// booleans report how the body was framed; the message just
			// reports the budget trip.
			chunkedTransfer := isChunked(req.TransferEncoding) || isChunked(resp.TransferEncoding)
			logger.Debug("Skipping HTTP capture because body exceeded capture budget",
				zap.Int("capture_budget_bytes", maxHTTPBodyCaptureBytes),
				zap.Int64("request_bytes_seen", reqCapture.Total()),
				zap.Int64("response_bytes_seen", respCapture.Total()),
				zap.Bool("streaming_exchange", streamingExchange),
				zap.Bool("chunked_transfer", chunkedTransfer),
				zap.String("url", req.URL.String()),
				zap.String("method", req.Method),
				zap.Int("status_code", resp.StatusCode),
				zap.String("response_content_type", resp.Header.Get("Content-Type")),
			)
			shouldCapture = false
		}

		if !shouldCapture {
			if forceCloseMode || pressureCloseMode {
				return
			}
			continue
		}

		// Move ALL capture parsing to a background goroutine so the
		// sampling semaphore is released immediately (via defer releaseLock
		// when this function returns). For large payloads the dump+parse
		// cycle allocates 8MB+ of intermediate copies; doing that while
		// holding the semaphore starves other connections of capture slots
		// and drives up p95 latency under load.
		//
		// Snapshot all values the goroutine needs — avoid closing over
		// loop variables even though the loop exits after one iteration
		// in sync/sampling mode (defensive against future changes).
		reqBodyBytes := reqCapture.Bytes()
		respBodyBytes := respCapture.Bytes()
		reqBytesTotal := reqCapture.Total()
		respBytesTotal := respCapture.Total()
		capturedReq := req
		capturedResp := resp
		capturedReqTS := reqTimestamp
		capturedRespTS := respTimestamp

		go func() {
			exchangeCaptureSize, err := capturedExchangeSize(capturedReq, capturedResp, reqBodyBytes, respBodyBytes)
			if err != nil {
				logger.Error("Failed to estimate combined captured exchange size. This indicates an internal capture error; report it if it persists.",
					zap.Error(err),
					zap.Int64("request_bytes_seen", reqBytesTotal),
					zap.Int64("response_bytes_seen", respBytesTotal),
				)
				return
			}
			if exchangeCaptureSize > maxHTTPCombinedCaptureBytes {
				logger.Debug("Skipping HTTP capture because combined request and response exceeded capture budget",
					zap.Int("capture_budget_bytes", maxHTTPCombinedCaptureBytes),
					zap.Int("captured_exchange_bytes", exchangeCaptureSize),
					zap.Int64("request_bytes_seen", reqBytesTotal),
					zap.Int64("response_bytes_seen", respBytesTotal),
					zap.String("url", capturedReq.URL.String()),
					zap.String("method", capturedReq.Method),
					zap.Int("status_code", capturedResp.StatusCode),
				)
				return
			}

			// Capture parsing is best-effort: the exchange has already been
			// proxied successfully, so parse failures just skip the test case.
			reqData, err := dumpCapturedRequest(capturedReq, reqBodyBytes)
			if err != nil {
				logger.Error("Failed to dump captured request. This indicates an internal capture error; report it if it persists.",
					zap.Error(err),
					zap.Int64("request_bytes_seen", reqBytesTotal),
					zap.Int("captured_request_bytes", len(reqBodyBytes)),
				)
				return
			}

			parsedHTTPReq, err := pkg.ParseHTTPRequest(reqData)
			if err != nil {
				logger.Error("Failed to parse captured request for testcase. Verify the client is sending valid HTTP if this persists.",
					zap.Error(err),
					zap.Int("captured_request_dump_bytes", len(reqData)),
					zap.Int64("request_bytes_seen", reqBytesTotal),
				)
				return
			}

			respData, err := dumpCapturedResponse(capturedResp, parsedHTTPReq, respBodyBytes)
			if err != nil {
				logger.Error("Failed to dump captured response. This indicates an internal capture error; report it if it persists.",
					zap.Error(err),
					zap.Int("status_code", capturedResp.StatusCode),
					zap.Int64("response_bytes_seen", respBytesTotal),
					zap.Int("captured_response_bytes", len(respBodyBytes)),
				)
				return
			}
			parsedHTTPRes, err := pkg.ParseHTTPResponse(respData, parsedHTTPReq)
			if err != nil {
				logger.Error("Failed to parse captured response for testcase. Verify the upstream application is returning valid HTTP if this persists.",
					zap.Error(err),
					zap.Int("captured_response_dump_bytes", len(respData)),
					zap.Int64("response_bytes_seen", respBytesTotal),
					zap.Int("status_code", capturedResp.StatusCode),
				)
				return
			}

			defer parsedHTTPReq.Body.Close()
			defer parsedHTTPRes.Body.Close()
			hooksUtils.CaptureHook(ctx, logger, t, parsedHTTPReq, parsedHTTPRes, capturedReqTS, capturedRespTS, pm.incomingOpts, pm.synchronous, pm.mapping, actualPort)
		}()

		// Exit the loop in sync/sampling mode or when memory pressure requires closing.
		if pressureCloseMode {
			return
		}
	}
}

// handleHttp1ZeroCopy handles HTTP/1.x connections in normal (non-sync,
// non-sampling) mode. It forwards raw TCP bytes bidirectionally between client
// and upstream with zero HTTP parsing overhead on the critical path.
//
// Capture is fully decoupled from forwarding: byte streams are piped through
// non-blocking asyncPipeFeeders to a streaming parser (parseStreamingHTTP)
// that emits test cases as soon as each HTTP exchange completes — without
// waiting for the connection to close. When memory pressure is detected or
// the capture size limit is reached, the feeders silently stop capturing
// while forwarding continues unimpacted.
func (pm *IngressProxyManager) handleHttp1ZeroCopy(ctx context.Context, clientConn net.Conn, upConn net.Conn, logger *zap.Logger, t chan *models.TestCase, appPort uint16) {
	logger.Debug("Using zero-copy TCP passthrough with streaming capture")

	captureEnabled := !isIngressRecordingPaused()
	var reqFeeder, respFeeder *asyncPipeFeeder
	if captureEnabled {
		// maxSize=0 disables the per-feeder total-bytes limit. With
		// streaming capture the parser consumes data incrementally, so
		// in-flight memory is bounded by the channel buffer plus one
		// request/response body in the parser. The per-test-case size
		// limit is enforced downstream in CaptureHook.
		reqFeeder = newAsyncPipeFeeder(0, logger)
		respFeeder = newAsyncPipeFeeder(0, logger)
		go pm.parseStreamingHTTP(ctx, logger, reqFeeder, respFeeder, t, appPort)
	}

	// Close connections on context cancellation (shutdown) to unblock
	// the io.Copy goroutines. Without this, the function hangs until
	// the remote side (e.g., upstream app's keep-alive) closes naturally.
	// Same pattern used by the gRPC handler.
	go func() {
		<-ctx.Done()
		clientConn.Close()
		upConn.Close()
	}()

	done := make(chan struct{}, 2)

	// client → upstream (with optional non-blocking side-copy for capture)
	go func() {
		var dst io.Writer = upConn
		if reqFeeder != nil {
			dst = io.MultiWriter(upConn, reqFeeder)
		}
		_, _ = io.Copy(dst, clientConn)
		if reqFeeder != nil {
			reqFeeder.Close()
		}
		if tc, ok := upConn.(*net.TCPConn); ok {
			_ = tc.CloseWrite()
		}
		done <- struct{}{}
	}()

	// upstream → client (with optional non-blocking side-copy for capture)
	go func() {
		var dst io.Writer = clientConn
		if respFeeder != nil {
			dst = io.MultiWriter(clientConn, respFeeder)
		}
		_, _ = io.Copy(dst, upConn)
		if respFeeder != nil {
			respFeeder.Close()
		}
		if tc, ok := clientConn.(*net.TCPConn); ok {
			_ = tc.CloseWrite()
		}
		done <- struct{}{}
	}()

	<-done
	<-done
	// No post-hoc parsing needed — test cases were emitted incrementally
	// by parseStreamingHTTP as each HTTP exchange completed.
}

// parseStreamingHTTP reads from request and response feeders concurrently with
// live forwarding, emitting test cases as soon as each HTTP exchange completes.
// This avoids waiting for the connection to close before capturing test cases.
//
// http.ReadRequest / http.ReadResponse act as natural delimiters — HTTP/1.1 is
// self-framing (headers end with \r\n\r\n, body length from Content-Length or
// chunked encoding), so no custom delimiter detection is needed.
//
// Timestamps are sourced from the feeders' LastReadTime, which is stamped
// inside the feeder's Read at io.Copy's read of the source socket. Under
// concurrent client load the parser can run arbitrarily behind the
// forwarder; using parser-iteration time would push the recorded HTTP
// timestamps after the corresponding DB-side mock timestamps, breaking
// the per-test window-attribution invariant relied on by the postgres v3
// / mongo v2 dispatchers.
//
// Request vs response timestamp semantics differ:
//
//   - reqTimestamp captures FIRST-byte arrival. Snapshot AFTER ReadRequest
//     parses headers but BEFORE io.ReadAll consumes the body, so any
//     outbound mocks the app issues while reading the request body
//     (streaming uploads with side-effects) fall AFTER reqTimestamp and
//     are correctly inside the per-test window. Capturing post-body would
//     give last-byte semantic and silently drop those mocks.
//
//   - respTimestamp captures LAST-byte arrival. Snapshot AFTER io.ReadAll
//     consumes the response body — that is when the application has
//     finished processing the test case and any outbound mocks issued
//     during processing have already been recorded with timestamps before
//     this point.
func (pm *IngressProxyManager) parseStreamingHTTP(ctx context.Context, logger *zap.Logger,
	reqFeeder, respFeeder *asyncPipeFeeder, t chan *models.TestCase, appPort uint16) {

	// Unblock shutdown's drain goroutine on every exit path. Without this
	// the drain would race with our Read() for channel data and could steal
	// chunks before we consume them, silently dropping captures.
	defer reqFeeder.signalParserExit()
	defer respFeeder.signalParserExit()

	reqReader := bufio.NewReader(reqFeeder)
	respReader := bufio.NewReader(respFeeder)

	for {
		if isIngressRecordingPaused() {
			return
		}

		req, err := http.ReadRequest(reqReader)
		if err != nil {
			return
		}

		// Snapshot reqTimestamp here, AFTER ReadRequest has consumed
		// the request line + headers but BEFORE io.ReadAll consumes
		// the body. lastReadNano now reflects the chunk that delivered
		// the most recent bufio fill — for tightly packed connections
		// this is the chunk holding the request's first byte; for the
		// rare case of headers split across two chunks it is the chunk
		// holding the last header byte (a sub-µs overshoot).
		//
		// This is the FIRST-byte semantic the per-test window-
		// attribution path expects: any DB queries the app issues
		// during request handling fall AFTER reqTimestamp and BEFORE
		// respTimestamp, putting them inside the test's window.
		reqTimestamp := reqFeeder.LastReadTime()
		if reqTimestamp.IsZero() {
			reqTimestamp = time.Now()
		}

		// Set Host header to match pkg.ParseHTTPRequest behavior
		req.Header.Set("Host", req.Host)

		// Read request body with a size cap to avoid unbounded allocations.
		// We must consume the full body to advance the reader past it
		// (HTTP framing), but only keep up to MaxTestCaseSize in memory.
		reqBody, err := io.ReadAll(io.LimitReader(req.Body, int64(hooksUtils.MaxTestCaseSize)+1))
		// Drain any remainder to keep the stream aligned.
		_, _ = io.Copy(io.Discard, req.Body)
		req.Body.Close()
		if err != nil {
			return
		}

		if len(reqBody) > hooksUtils.MaxTestCaseSize {
			// Body exceeds capture budget — skip this exchange.
			// Read and discard the response to keep the stream aligned.
			resp, rerr := http.ReadResponse(respReader, req)
			if rerr != nil {
				return
			}
			_, _ = io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			continue
		}
		req.Body = io.NopCloser(bytes.NewReader(reqBody))

		resp, err := http.ReadResponse(respReader, req)
		if err != nil {
			return
		}

		// Same bounded read for the response body.
		respBody, err := io.ReadAll(io.LimitReader(resp.Body, int64(hooksUtils.MaxTestCaseSize)+1))
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if err != nil {
			return
		}
		if len(respBody) > hooksUtils.MaxTestCaseSize {
			continue // Skip oversized response
		}
		resp.Body = io.NopCloser(bytes.NewReader(respBody))

		// Last-byte semantic for the response: lastReadNano now reflects
		// the chunk that delivered the final byte of the response body,
		// so the test window's upper bound encloses any outbound mocks
		// issued during request handling.
		respTimestamp := respFeeder.LastReadTime()
		if respTimestamp.IsZero() {
			respTimestamp = time.Now()
		}
		if respTimestamp.Before(reqTimestamp) {
			respTimestamp = reqTimestamp
		}

		// Emit in a goroutine so the parser loop is never blocked by a
		// slow CaptureHook (e.g. if the unbuffered tcChan or downstream
		// disk write stalls). Acquire from captureHookSem before forking
		// to bound peak goroutine count: under high concurrency we'd
		// otherwise launch one goroutine per HTTP exchange and each holds
		// references to req/resp body buffers (up to ~10MB combined,
		// since CaptureHook itself runs io.ReadAll on the NopCloser-
		// wrapped bodies the parser already buffered). Letting them
		// stack unbounded blew through the 250 MiB go-memory-load CI
		// guard. The acquire blocks the parser if the semaphore is
		// saturated — that backpressure is what prevents the next
		// in-flight allocation, so it must be on the parser goroutine,
		// not inside the launched goroutine. Race the acquire with
		// ctx.Done() so a connection close during agent shutdown unblocks
		// the parser instead of pinning it to a saturated semaphore.
		select {
		case captureHookSem <- struct{}{}:
		case <-ctx.Done():
			return
		}
		go func(req *http.Request, resp *http.Response, reqTs, respTs time.Time) {
			defer func() { <-captureHookSem }()
			hooksUtils.CaptureHook(ctx, logger, t, req, resp, reqTs, respTs, pm.incomingOpts, pm.synchronous, pm.mapping, appPort)
		}(req, resp, reqTimestamp, respTimestamp)
	}
}

// forwardRawTCP does bidirectional TCP forwarding between two connections
// with zero HTTP parsing overhead. Used for sampling-bypass connections
// where capture is not needed — bytes flow straight through via io.Copy.
// Keep-alive is preserved (no Connection: close injected), so the client
// can reuse the connection for multiple requests.
func forwardRawTCP(ctx context.Context, clientConn, upConn net.Conn) {
	// Close connections on context cancellation (shutdown).
	go func() {
		<-ctx.Done()
		clientConn.Close()
		upConn.Close()
	}()

	done := make(chan struct{}, 2)
	go func() {
		_, _ = io.Copy(upConn, clientConn)
		if tc, ok := upConn.(*net.TCPConn); ok {
			_ = tc.CloseWrite()
		}
		done <- struct{}{}
	}()
	go func() {
		_, _ = io.Copy(clientConn, upConn)
		if tc, ok := clientConn.(*net.TCPConn); ok {
			_ = tc.CloseWrite()
		}
		done <- struct{}{}
	}()
	<-done
	<-done
}

func isChunked(te []string) bool {
	for _, s := range te {
		if strings.EqualFold(s, "chunked") {
			return true
		}
	}
	return false
}

func isIngressExpectedCloseErr(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.EOF) {
		return true
	}
	errStr := err.Error()
	return strings.Contains(errStr, "unexpected EOF") ||
		strings.Contains(errStr, "broken pipe") ||
		strings.Contains(errStr, "connection reset by peer") ||
		strings.Contains(errStr, "use of closed network connection") ||
		strings.Contains(errStr, "wsarecv") ||
		strings.Contains(errStr, "wsasend") ||
		strings.Contains(errStr, "forcibly closed by the remote host")
}
