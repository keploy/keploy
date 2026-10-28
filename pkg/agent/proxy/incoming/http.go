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

	proxyutil "go.keploy.io/server/v3/pkg/agent/proxy/util"

	"go.keploy.io/server/v3/pkg"
	hooksUtils "go.keploy.io/server/v3/pkg/agent/hooks/conn"
	"go.keploy.io/server/v3/pkg/agent/memoryguard"
	"go.keploy.io/server/v3/pkg/agent/proxy/fakeconn"
	syncMock "go.keploy.io/server/v3/pkg/agent/proxy/syncMock"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/neterr"
	"go.uber.org/zap"
)

var isIngressRecordingPaused = memoryguard.IsRecordingPaused

// noteIngressRequest counts each request the ingress has read, before it reads
// the pause for it: the debug knob that forces a pause by request count
// (memoryguard.ForcePauseEnv) starts and ends its pause here. A no-op unless
// the knob is set.
var noteIngressRequest = memoryguard.IngressRequest

// wireTimeConn wraps a net.Conn and stamps the time of the most recent
// non-empty Read on an atomic field. The zero-copy loop also wraps the app's
// side in one, for when the bytes that end a response arrived: the end of
// its exchange's window.
//
// Both HTTP/1 capture loops use it to sample the wire-arrival time of an
// HTTP request: when http.ReadRequest had to drive at least one new socket
// Read during this iteration, the latest stamp is the tightest available upper
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
		c.lastReadNano.Store(clockNow().UnixNano())
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

// captureHookConcurrency caps the number of CaptureHook goroutines
// running at the same time across every handleHttp1ZeroCopy
// invocation: each takes its slot with takeCaptureSlot.
// Each goroutine takes a reference to the parsed *http.Request /
// *http.Response (each carrying up to MaxTestCaseSize=5MB of body) and
// runs io.ReadAll a second time inside Capture to materialise its own
// reqBody/respBody copy, so peak transient memory per in-flight
// goroutine is ~10 MB — a bound that holds only because Capture caps
// decompression at MaxTestCaseSize per body (pkg.Decompress).
// Without a cap the parser launches one goroutine
// per HTTP exchange unconditionally; the go-memory-load workload
// (k6 firing 42 concurrent VUs against /large_payload endpoints)
// piles hundreds of these goroutines up faster than the unbuffered
// tcChan can drain them, taking the agent past the 250 MiB CI guard.
// 16 in-flight × ~10 MB = ~160 MB worst-case, leaving headroom inside
// the 200 MB Docker memory limit for the rest of agent state.
const captureHookConcurrency = 16

var captureHookSem = make(chan struct{}, captureHookConcurrency)

// takeCaptureSlot waits for a slot in captureHookSem, or for ctx to end (ok
// false). release gives the slot back to the semaphore it was taken from:
// captureHookSem is read once, here. Read again at release, it would free a
// slot of whatever semaphore it holds by then (tests swap it): the slot taken
// would stay held for good, and one this capture never took would be freed.
func takeCaptureSlot(ctx context.Context) (release func(), ok bool) {
	sem := captureHookSem
	select {
	case sem <- struct{}{}:
		return func() { <-sem }, true
	case <-ctx.Done():
		return nil, false
	}
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

	// newAppAddr holds the port Keploy moved the application to, not the port
	// it advertises, so the test case records the caller-supplied appPort
	// (the original) instead.
	actualPort := appPort

	// Dial Upstream
	upConn, err := pm.dialApp(ctx, finalAppAddr)
	if err != nil {
		pm.logAppDialFailure(logger, "Failed to connect to upstream application. Verify the application is listening on the resolved address.",
			err, zap.String("final_app_addr", finalAppAddr))
		return
	}
	defer func() {
		_ = upConn.Close()
	}()

	// win is the open window of the exchange in flight (syncMock.OpenWindow),
	// from its request's first byte until its capture has decided it: the mock
	// manager holds every mock it may own until then, however long it streams.
	// A capture takes it over; any other way out ends it here.
	var win *syncMock.Window
	defer func() { win.Close() }()

	// forceCloseMode: only sync mode needs the traditional HTTP parsing loop
	// (strict one-at-a-time ordering with forced close). Sampling mode now
	// uses the zero-copy path for both tracked and bypass connections.
	forceCloseMode := pm.synchronous
	// connName names the connection in the count of the requests a pause of
	// the memory guard costs (syncMock NoteNotRecordedUnderPressure), here and
	// in the capture hook (hooksUtils.SkipUnderPressure).
	connName := ingressConnName(clientConn, finalAppAddr)

	if !forceCloseMode {
		// Sampling bypass: no capture, but route through the same
		// request-framed path so we still get MSG_PEEK pool-liveness
		// protection. Passing nil for the testcase channel suppresses
		// capture inside handleHttp1ZeroCopy; forceClose=false leaves
		// keep-alive intact so bypass connections are pure passthrough.
		if pm.sampling && !acquiredLock {
			pm.handleHttp1ZeroCopy(ctx, clientConn, upConn, logger, nil, actualPort, false, nil)
			return
		}
		// Normal mode OR sampling-tracked: zero-copy TCP passthrough with
		// streaming capture: the body tees (captureBuffer) copy what is
		// forwarded, and each capture is built off the forwarding path.
		//
		// Sampling-tracked: pass forceClose=true so each captured
		// exchange terminates the connection (Connection: close on req
		// and resp), releasing the sampling slot promptly. Without
		// this, a Go upstream's keep-alive response pins the slot for
		// the connection's idle lifetime, starving subsequent
		// connections of capture slots — the symptom is "only N of
		// many requests captured" on sequential-call workloads. Pass
		// releaseLock so demote-to-bypass (chunked / memory pressure)
		// can free the slot mid-connection.
		//
		// Normal mode: forceClose=false (keep-alive preserved), and we
		// release any (unused) lock here since normal mode doesn't
		// gate on the sampling semaphore.
		if pm.sampling && acquiredLock {
			pm.handleHttp1ZeroCopy(ctx, clientConn, upConn, logger, t, actualPort, true, releaseLock)
			return
		}
		if !pm.sampling {
			releaseLock() // Normal mode has no sampling lock to hold
		}
		pm.handleHttp1ZeroCopy(ctx, clientConn, upConn, logger, t, actualPort, false, nil)
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
	// The app's side is read ahead of the client, and each piece stamped as
	// it arrives: the arrival of a response's last byte (or of the close that
	// ends its body) is where its exchange's window ends (respTimestamp
	// below), however slow its client.
	upAhead := newAheadReader(upConn)
	defer upAhead.Close() // before upConn's close, which ends its read
	upstreamReader := bufio.NewReader(upAhead)

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
		iterStart := clockNow()
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
		noteIngressRequest()
		pausedAtRequest := isIngressRecordingPaused()
		pressureCloseMode := forceCloseMode || pausedAtRequest
		captureEnabled := !pausedAtRequest
		// lostToPause: a pause of the memory guard decided this exchange is
		// not recorded, at any of the checks below; counted once, as it ends.
		lostToPause := !captureEnabled
		// pausedNow is every check below of a pause that began mid-exchange:
		// when the memory guard has paused recording, the exchange is lost
		// to the pause (lostToPause), so no check can decide it is not
		// recorded without its count.
		pausedNow := func() bool {
			if !isIngressRecordingPaused() {
				return false
			}
			lostToPause = true
			return true
		}
		// The exchange may be captured: open its window before the request
		// reaches the app, so no egress call it makes is ever older than it.
		// One that cannot be captured opens none: nothing would decide it.
		if captureEnabled {
			win = syncMock.FromContextOrGlobal(ctx).OpenWindow(reqTimestamp, nil)
		}

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
		// path returns early via handleHttp1ZeroCopy/forwardRawTCP). So is
		// pressureCloseMode, and the re-checks that ran only while it was
		// false (after the request is forwarded, after the response) are
		// gone too: they could never run.

		// Determine whether capture is eligible for this exchange.
		// Chunked-encoded exchanges are still captured: the tee reads the
		// decoded body stream, and oversized bodies are rejected later by
		// the capture-budget check on reqCapture/respCapture.Truncated().
		captureEligible := captureEnabled && (!pm.sampling || acquiredLock)

		// Re-check memory pressure right before attaching capture buffers.
		if captureEligible && pausedNow() {
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

		// A request whose client waits for a 100 (Continue) before it sends
		// the body is written beside the read of its response, which forwards
		// the 100 (requestRelay).
		var relay *requestRelay
		if expectsContinue(req) {
			relay = relayRequest(req, upConn, clientConn)
		} else {
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
		}

		resp, _, err := readFinalResponse(upstreamReader, req, clientConn)
		if err != nil {
			relay.abandon()
			var gone *clientWriteError
			if errors.As(err, &gone) {
				logger.Debug("HTTP/1 ingress client connection closed while an interim response was forwarded to it", zap.Error(err))
				return
			}
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

		// The response's headers are read. Stamped BEFORE the lock is given
		// back below, so a response whose time this stays (one read whole with
		// its headers) ends before the next request is read: see where the
		// body's last byte moves it on, after resp.Write.
		respTimestamp := clockNow()

		// The app has answered. The rest of a body it answered before it came
		// is forwarded, within bounds, before the answer (requestRelay.finish).
		// A request that still did not reach the app whole (an upload its
		// client never sent the body of) is not recorded: neither what the
		// client meant nor what the app got.
		requestCut := false
		if err := relay.finish(); err != nil {
			requestCut = true
			if captureEligible && captureEnabled {
				warnNotRecorded(logger, req, resp, err)
			}
			captureEnabled = false
		}

		// Response modifications for sync/sampling modes. appResp is the
		// response as the app sent it, which is what is recorded.
		appResp := resp
		if forceCloseMode {
			if resp.ContentLength == -1 || isChunked(resp.TransferEncoding) {
				// Release the sync/sampling lock early on streaming
				// responses so a slow upstream doesn't wedge the slot,
				// but keep capture enabled — the tee reads decoded
				// body bytes and the capture-budget check handles
				// genuinely oversized streams.
				streamingExchange = true
			}
			if streamingExchange {
				// The lock is given back here, or was at a request body of
				// unknown length, and the next request runs beside the rest of
				// this one. The window yields at the headers: it still runs to
				// the response's last byte (respTimestamp, below), but past
				// the headers it leaves a mock another request claims to that
				// request (from its start to its own yield: its headers if it
				// gave the lock back early, else its last byte from the app),
				// and one a window decided before it spans too to that window
				// (syncMock.Window.Yield). Up to the headers it is as it was.
				win.Yield(respTimestamp)
				// Nothing that follows the yield runs before the clock reads
				// past it (waitPastYield): not the next request, which takes
				// the lock given back here, nor what the client does on the
				// headers forwarded below. On a coarse clock (Windows') a
				// call either makes could read the very instant the window
				// yielded at, and the window, decided first, took it. One
				// that is not recorded (memory pressure) claims nothing.
				if captureEnabled {
					waitPastYield(win, respTimestamp)
				}
				releaseLock()
			}

			appResp = closeAfter(resp)
		}

		// Re-evaluate capture eligibility after response headers.
		// Also re-check memory pressure in case it started mid-exchange.
		captureEligible = captureEnabled && (!pm.sampling || acquiredLock)
		if captureEligible && pausedNow() {
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

		// The client sees the response's end (its last byte, or the headers
		// of one with no body) only once the clock reads past it
		// (waitPastYield): the window claims up to its end, and what the
		// client does on it, a next request or a call a stream still in
		// flight makes on it, would read the very instant the window ends at.
		// endAt is that end, the response's time below. It is past it before
		// the lock is given back too: one that kept the lock gives it back
		// once resp.Write has returned, and the next request is read, and
		// calls out, from then on. Only for an exchange that may be recorded:
		// one that is not claims nothing.
		headersAt := respTimestamp
		endAt := func() time.Time {
			if lastByte := upAhead.LastReadTime(); lastByte.After(headersAt) {
				return lastByte
			}
			return headersAt
		}
		waitPastEnd := func() {
			if captureEligible && !reqCapture.Truncated() && !respCapture.Truncated() {
				waitPastYield(win, endAt())
			}
		}
		if resp.Body == nil || resp.Body == http.NoBody {
			waitPastEnd()
		} else {
			resp.Body = &endWaitBody{ReadCloser: resp.Body, left: resp.ContentLength, atEnd: waitPastEnd}
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

		// The response's time is its LAST byte's: when that byte arrived from
		// the app (for a body that ends when the app closes the connection,
		// when the close did), not when its headers were read. It is the end
		// of the exchange's window (reqTimestamp <= an egress call's time <=
		// respTimestamp), for the resolve and for the recorded test case, so
		// an egress call the app makes while it sends the body (a stream that
		// fetches as it goes, a handler that flushes its headers first) is
		// inside it. Left at the headers, the resolve stopped short of those
		// calls, and the exchange was recorded without their mocks.
		//
		// The app's side is read ahead of the client (aheadReader), so a slow
		// client does not stretch the window past when the app finished, up
		// to the read-ahead's bounds; nor is it the moment the body was
		// forwarded. A body read whole with its headers keeps the headers'
		// time, taken before the lock is given back. A window that goes on
		// past its headers overlaps the requests run beside the rest of it,
		// and the yield at the headers (above) keeps what they claim theirs.
		respTimestamp = endAt()
		// The exchange is over: from its last byte on its request is in flight
		// no more, and claims nothing over the requests that run beside a
		// stream still being sent (a no-op for one that yielded at its
		// headers already).
		win.Yield(respTimestamp)

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
		if shouldCapture && pausedNow() {
			shouldCapture = false
		}

		// A pause that cut a body's capture and ended before the checks
		// above read it: the exchange is lost to the pause, and counted.
		if shouldCapture && (reqCapture.LostToPause() || respCapture.LostToPause()) {
			lostToPause = true
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
			if lostToPause {
				syncMock.FromContextOrGlobal(ctx).NoteNotRecordedUnderPressure(connName)
			}
			win.Close()
			win = nil
			if forceCloseMode || pressureCloseMode {
				if requestCut {
					// Its client may still be sending the body cut off.
					releaseLock()
					lingerClose(clientConn)
				}
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
		capturedResp := appResp
		capturedReqTS := reqTimestamp
		capturedRespTS := respTimestamp
		capturedWin := win
		win = nil

		// Being built until it is in the recorder's queue (or given up): a
		// recording's stop waits for it (syncMock BacklogBefore). Noted
		// here, as the response completes, not when the goroutine runs.
		built := syncMock.FromContextOrGlobal(ctx).BuildingTestCase(capturedRespTS)
		pm.captures.Add(1)
		go func() {
			defer pm.captures.Done()
			defer built()
			defer capturedWin.Close() // once the capture has decided it, or skipped it
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
			hooksUtils.CaptureHook(proxyWindowClock(syncMock.WithWindow(syncMock.WithIngressConn(ctx, connName), capturedWin)), logger, t, parsedHTTPReq, parsedHTTPRes, capturedReqTS, capturedRespTS, pm.loadIncomingOpts(), pm.synchronous, pm.mapping, actualPort)
		}()

		// Exit the loop in sync/sampling mode or when memory pressure requires closing.
		if pressureCloseMode {
			return
		}
	}
}

func isHTTPUpgrade(req *http.Request) bool {
	if req.Method == http.MethodConnect {
		return true
	}
	if req.Header.Get("Upgrade") != "" {
		return true
	}
	for _, v := range strings.Split(req.Header.Get("Connection"), ",") {
		if strings.EqualFold(strings.TrimSpace(v), "upgrade") {
			return true
		}
	}
	return false
}

// isIdempotentMethod returns whether RFC 9110 §9.2.2 permits automatic
// retry of the request after a stale-connection failure.
func isIdempotentMethod(m string) bool {
	switch m {
	case http.MethodGet, http.MethodHead, http.MethodPut,
		http.MethodDelete, http.MethodOptions, http.MethodTrace:
		return true
	}
	return false
}

// isStaleConnError reports whether err looks like an upstream
// connection-drop (the case replay-on-stale was designed for) as
// opposed to a deterministic protocol/parse failure (where replay
// would just re-trigger the same bad response). EOF / unexpected EOF
// / connection reset / broken pipe / "use of closed network
// connection" / connection aborted all map to "the conn died, try a
// fresh one"; everything else — including timeouts (Timeout()=true on
// the wrapping net.OpError) and protocol-parse errors — is not
// retried, since those typically indicate a deterministic upstream
// failure that replay would just re-trigger and double-charge.
func isStaleConnError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}
	// Specific syscall errors that propagate via net.OpError's Unwrap
	// chain (errors.Is recurses through it). Timeouts deliberately
	// not included — a slow upstream isn't a stale-pool case and
	// replay would just re-pay the timeout while double-charging the
	// upstream for the original.
	//
	// Classified through pkg/neterr rather than against syscall.E*
	// directly: on Windows those constants are APPLICATION_ERROR-space
	// values Winsock never returns, so the direct comparison is inert
	// and every pooled-connection reset became a hard 502 instead of a
	// transparent redial.
	if neterr.IsConnReset(err) ||
		neterr.IsBrokenPipe(err) ||
		neterr.IsConnAborted(err) {
		return true
	}
	if errors.Is(err, net.ErrClosed) {
		return true
	}
	return false
}

func writeBadGateway(c net.Conn) {
	_, _ = c.Write([]byte("HTTP/1.1 502 Bad Gateway\r\nContent-Length: 0\r\nConnection: close\r\n\r\n"))
}

func (pm *IngressProxyManager) appGone(c net.Conn) {
	if !pm.mockMode {
		writeBadGateway(c)
	}
}

// writeBadRequest is the client-side counterpart to writeBadGateway:
// 400-class so the failure isn't misattributed to the upstream by
// downstream proxies / monitoring. Used when we detect a malformed
// or truncated request from the downstream client (e.g.,
// Content-Length mismatch during replay-safety pre-buffering).
func writeBadRequest(c net.Conn) {
	_, _ = c.Write([]byte("HTTP/1.1 400 Bad Request\r\nContent-Length: 0\r\nConnection: close\r\n\r\n"))
}

// handleHttp1ZeroCopy handles HTTP/1.x connections in normal (non-sync) and
// sampling modes.
//
// Architecture (Option 3 + MSG_PEEK + replay-on-stale): the function frames
// HTTP/1.1 requests on the downstream so it can manage upstream connection
// lifetime per request. Before reusing a pooled upstream conn it does one
// non-blocking MSG_PEEK to detect already-delivered FIN/RST; on stale
// detection it dials a fresh upstream. If the actual write to upstream
// fails (residual race between peek and write), idempotent requests are
// transparently replayed on a fresh conn — the canonical pattern used by
// envoy (retry_on: reset-before-request), nginx (proxy_next_upstream), and
// Go's net/http Transport (errServerClosedIdle).
//
// forceClose / releaseSlot govern sampling-tracked behavior:
//
//   - forceClose=true: this connection holds a sampling slot. Each captured
//     non-streaming exchange has Connection: close injected on both req
//     and resp so the connection terminates after one exchange and the
//     slot is freed. Without this, a Go upstream's keep-alive response
//     pins the slot for the connection's idle lifetime, starving
//     subsequent connections of capture slots ("only N of many requests
//     captured" symptom).
//
//   - releaseSlot (only set when forceClose=true): invoked to free the
//     sampling slot early when the exchange is demoted to bypass
//     (chunked / unknown-length body, or memory pressure). After demotion
//     the connection continues as pure passthrough with no header
//     mutation, matching the "untracked + unchanged" rule for ineligible
//     exchanges.
//
// Downstream (envoy↔keploy) keep-alive is preserved across the entire
// connection lifetime for normal mode and sampling-bypass; only upstream
// conns get redialed on stale detect.
func (pm *IngressProxyManager) handleHttp1ZeroCopy(ctx context.Context, clientConn net.Conn, upConn net.Conn, logger *zap.Logger, t chan *models.TestCase, appPort uint16, forceClose bool, releaseSlot func()) {
	logger.Debug("Using request-framed HTTP/1.1 proxy with MSG_PEEK pool liveness")

	upstreamAddr := upConn.RemoteAddr().String()
	// Capture the network family of the original conn so redial uses
	// the same one — hard-coding "tcp4" would break against an IPv6
	// upstream and could mismatch a "tcp" dual-stack listener.
	upstreamNetwork := upConn.RemoteAddr().Network()

	// upConnHolder lets the ctx-cancel + function-exit goroutines safely
	// close whatever the CURRENT upstream conn is, without racing with
	// redial()'s reassignment. The main loop still uses the local upConn
	// variable for fast access (no atomic load on the hot path); redial
	// is responsible for keeping the holder in sync.
	type connHolder struct{ c net.Conn }
	var upConnHolder atomic.Pointer[connHolder]
	upConnHolder.Store(&connHolder{c: upConn})

	// On agent shutdown, close BOTH conns: closing the downstream
	// unblocks http.ReadRequest in the loop's read phase; closing the
	// upstream unblocks http.ReadResponse / req.Write if the loop is
	// stuck in upstream I/O on a hung backend. Race-safe via the
	// atomic — we always close whatever connHolder pointer is current,
	// even if redial swapped it concurrently.
	go func() {
		<-ctx.Done()
		_ = clientConn.Close()
		if h := upConnHolder.Load(); h != nil {
			_ = h.c.Close()
		}
	}()

	wireConn := &wireTimeConn{Conn: clientConn}
	clientReader := bufio.NewReader(wireConn)
	// The app's side is stamped as it is read: when the bytes that end a
	// response arrived from the app is where its exchange's window ends.
	upWire := &wireTimeConn{Conn: upConn}
	upstreamReader := bufio.NewReader(upWire)
	// t == nil → caller (sampling-bypass path) wants MSG_PEEK protection
	// without capture. Suppress capture for this connection's lifetime.
	// connCapture is the connection's capture; whether one exchange of it is
	// captured is decided per exchange (captureEnabled, in the loop). A pause
	// of the memory guard is the exchange's, never the connection's: latched
	// for the connection's life, one pause stopped the capture of every
	// keep-alive connection open through it (a load generator's, a pooled
	// client's) for the rest of the recording, and nothing said so.
	connCapture := t != nil
	// connName names the connection in the count of the requests a pause
	// costs (syncMock NoteNotRecordedUnderPressure), here and in the capture
	// hook (hooksUtils.SkipUnderPressure).
	var connName string
	if connCapture {
		connName = ingressConnName(clientConn, upstreamAddr)
	}
	notRecordedUnderPressure := func() {
		syncMock.FromContextOrGlobal(ctx).NoteNotRecordedUnderPressure(connName)
	}

	// forceCloseActive mirrors the caller's forceClose intent but can be
	// flipped to false mid-connection by demoteToBypass when the exchange
	// turns out to be ineligible for the sampling budget (chunked /
	// unknown-length body, or memory pressure as its request came). Once
	// demoted, the connection becomes pure passthrough: no Connection: close
	// injection on subsequent iterations, the sampling slot is released,
	// and capture is disabled — matching the "untracked + unchanged" rule
	// for non-capturable exchanges. The flag never re-enables itself: a
	// sampled connection serves one exchange, and one demoted is out of the
	// sample, not lost to it.
	forceCloseActive := forceClose
	demoteToBypass := func() {
		if !forceCloseActive {
			return
		}
		forceCloseActive = false
		connCapture = false
		if releaseSlot != nil {
			releaseSlot()
		}
	}

	// Close the most recent upstream conn on function return. The caller
	// (handleHttp1Connection) defers Close on its own copy of the original
	// upConn, but redial() replaces this function's local upConn — so
	// without this defer, every successful redial leaks the freshly dialed
	// socket until ctx is cancelled. Reads from the holder so it always
	// targets the latest conn (the local upConn could be stale if redial
	// happened mid-iteration).
	defer func() {
		if h := upConnHolder.Load(); h != nil {
			_ = h.c.Close()
		}
	}()

	// First request rides the pre-dialed upConn — we trust it because
	// handleHttp1Connection just opened it. Subsequent iterations peek.
	upConnFresh := true

	redial := func() error {
		// Honor parent ctx — once cancellation has fired, the shutdown
		// goroutine has closed the conns and any in-flight req.Write /
		// ReadResponse failed with net.ErrClosed (which isStaleConnError
		// classifies as stale). Without this short-circuit we'd then
		// dial a fresh upstream during shutdown, generate one more
		// pointless round-trip, and delay the connection teardown.
		if err := ctx.Err(); err != nil {
			return err
		}
		// Close the previous conn via the holder so a concurrent
		// shutdown sees a consistent view (either the old conn or the
		// new one — never a torn pointer).
		if h := upConnHolder.Load(); h != nil {
			_ = h.c.Close()
		}
		// DialContext (instead of DialTimeout) so an in-flight dial
		// also gets cancelled when the parent ctx fires. The derived
		// 3s deadline preserves the original per-dial timeout.
		dialCtx, cancelDial := context.WithTimeout(ctx, 3*time.Second)
		defer cancelDial()
		var d net.Dialer
		newConn, err := d.DialContext(dialCtx, upstreamNetwork, upstreamAddr)
		if err != nil {
			return err
		}
		upConn = newConn
		upWire = &wireTimeConn{Conn: upConn}
		upstreamReader = bufio.NewReader(upWire)
		upConnHolder.Store(&connHolder{c: newConn})
		return nil
	}

	// win is the open window of the exchange in flight (syncMock.OpenWindow),
	// from its request's first byte until its capture has decided it: the mock
	// manager holds every mock it may own until then, however long it streams.
	// A capture takes it over; any other way out ends it here.
	var win *syncMock.Window
	defer func() { win.Close() }()

	for {
		iterStart := clockNow()
		req, err := http.ReadRequest(clientReader)
		if err != nil {
			if !errors.Is(err, io.EOF) {
				// Malformed / truncated downstream request — surface a
				// structured 400 so clients/proxies don't see a bare
				// reset and misattribute it to upstream. EOF is the
				// normal end-of-keep-alive close path; no response
				// needed there.
				logger.Debug("Failed to parse downstream HTTP request", zap.Error(err))
				writeBadRequest(clientConn)
			}
			return
		}
		reqTimestamp := wireConn.LastReadTime()
		if !reqTimestamp.After(iterStart) {
			reqTimestamp = iterStart
		}

		// Sampling-tracked demote checks. The user-stated invariant is:
		// the sampling slot budget applies only to non-streaming
		// request-response pairs that we actually capture. Anything
		// else (HTTP Upgrade, chunked / unknown-length body, mid-stream
		// memory pressure) should be "untracked + unchanged" — i.e.
		// release the slot and forward without injecting headers.
		//
		// Demote MUST happen before any header mutation below so we
		// never half-modify an exchange we then bail out on.
		if forceCloseActive && isHTTPUpgrade(req) {
			demoteToBypass()
		}
		if forceCloseActive && (req.ContentLength == -1 || isChunked(req.TransferEncoding)) {
			demoteToBypass()
		}
		// The pause is read as the request has come: what it costs is this
		// exchange, which is counted, and the connection's next one is
		// captured if the pause is over by then. An Upgrade is a tunnel no
		// pause costs anything.
		noteIngressRequest()
		paused := isIngressRecordingPaused()
		if paused && connCapture && !isHTTPUpgrade(req) {
			notRecordedUnderPressure()
		}
		if forceCloseActive && paused {
			demoteToBypass()
		}
		captureEnabled := connCapture && !paused

		// HTTP Upgrade / CONNECT — bail out to raw byte pump after
		// forwarding the parsed handshake. Subsequent frames are not
		// HTTP/1.1, so per-request framing breaks down.
		if isHTTPUpgrade(req) {
			// Pool-liveness probe applies here too: an Upgrade on a
			// reused-but-stale conn would silently swallow the
			// handshake and leave the client hanging.
			if !upConnFresh && !peekUpstreamLive(upConn) {
				logger.Debug("Stale upstream pool entry detected before upgrade; redialing",
					zap.String("upstream", upstreamAddr))
				if rerr := redial(); rerr != nil {
					pm.logAppDialFailure(logger, "Upstream redial before upgrade failed. Verify the application is still listening on the resolved address.",
						rerr, zap.String("upstream", upstreamAddr))
					pm.appGone(clientConn)
					return
				}
			}
			upConnFresh = false
			if err := req.Write(upConn); err != nil {
				logger.Error("Failed to forward upgrade request to upstream. Verify upstream supports the requested upgrade and is listening on the resolved address.",
					zap.String("method", req.Method),
					zap.String("upgrade", req.Header.Get("Upgrade")),
					zap.String("upstream", upstreamAddr),
					zap.Error(err))
				pm.appGone(clientConn)
				return
			}
			// Drain bytes already buffered in our bufio readers before
			// switching to the raw byte pump. http.ReadRequest may
			// have buffered post-headers bytes from the same TCP
			// segment (e.g., a pipelined first WebSocket frame); on
			// the upstream side, partial response bytes from a
			// previous keep-alive iteration are theoretically possible
			// too. forwardRawTCP reads directly from the underlying
			// net.Conn and would silently drop anything sitting in the
			// bufio buffers, corrupting upgraded connections.
			//
			// Use io.CopyN — it advances the bufio cursor as it reads
			// (no separate Discard), and guarantees ALL N bytes are
			// transferred before returning (handles short writes
			// internally, unlike a single net.Conn.Write).
			if n := clientReader.Buffered(); n > 0 {
				if _, werr := io.CopyN(upConn, clientReader, int64(n)); werr != nil {
					logger.Debug("Failed to flush bufio-buffered upgrade bytes upstream",
						zap.String("upstream", upstreamAddr), zap.Error(werr))
					return
				}
			}
			if n := upstreamReader.Buffered(); n > 0 {
				if _, werr := io.CopyN(clientConn, upstreamReader, int64(n)); werr != nil {
					logger.Debug("Failed to flush bufio-buffered upgrade bytes downstream", zap.Error(werr))
					return
				}
			}
			forwardRawTCP(ctx, clientConn, upConn)
			return
		}

		// An exchange that may be captured opens its window here, after every
		// way it can turn out not to be (a bypass, an Upgrade tunnel, paused
		// recording) and before the request reaches the app, so no egress call
		// it makes is ever older than it. One that cannot be captured opens
		// none: nothing would decide it, and a tunnel can last for hours.
		if captureEnabled {
			win = syncMock.FromContextOrGlobal(ctx).OpenWindow(reqTimestamp, nil)
		}

		// MSG_PEEK before reusing a pooled upstream conn.
		if !upConnFresh && !peekUpstreamLive(upConn) {
			logger.Debug("Stale upstream pool entry detected (FIN in queue); redialing",
				zap.String("upstream", upstreamAddr))
			if err := redial(); err != nil {
				if pm.mockMode {
					logger.Debug("the app is not listening after a restart; closing the call as the app would", zap.String("upstream", upstreamAddr), zap.Error(err))
				} else {
					pm.logAppDialFailure(logger, "Upstream redial after stale-detect failed. Verify the application is still listening on the resolved address and that the network path between the agent and upstream is healthy.",
						err, zap.String("upstream", upstreamAddr))
				}
				pm.appGone(clientConn)
				if req.Body != nil {
					_ = req.Body.Close()
				}
				return
			}
		}
		upConnFresh = false

		// Replay-safety pre-buffering. Replaying an idempotent request
		// is only safe when we can resend the EXACT bytes that were
		// (or would have been) on the wire — gating on
		// reqCapture.Truncated() is insufficient because (a) the tee
		// is not attached on the sampling-bypass path (t == nil), so
		// reqCapture would be empty and we'd resend an empty body; and
		// (b) a mid-body write failure would leave reqCapture holding
		// only a prefix, so replay would resend a partial body.
		//
		// For idempotent methods with a known-bounded body, eagerly
		// drain it into a buffer up-front. The buffer becomes the
		// single source of truth for both the initial write and any
		// replay. For unknown-length (chunked) or oversized bodies we
		// can't safely buffer, so replay is disabled and forwarding
		// stays streaming.
		//
		// The body of a request whose client waits for a 100 (Continue) is
		// not buffered: its client sends it only once the app's 100 reaches
		// it, and the request is written beside the read of its response,
		// which forwards the 100. Replay is disabled, as for a stream.
		var preBufferedReqBody []byte
		canReplay := false
		continues := expectsContinue(req)
		if isIdempotentMethod(req.Method) && !continues {
			switch {
			case req.Body == nil || req.Body == http.NoBody || req.ContentLength == 0:
				// No body — replay is trivially safe.
				canReplay = true
			case req.ContentLength > 0 && req.ContentLength <= int64(maxHTTPBodyCaptureBytes):
				// Known-small body — buffer fully. Use ContentLength+1
				// as the read cap so an upstream lying about
				// Content-Length doesn't OOM us.
				b, rerr := io.ReadAll(io.LimitReader(req.Body, req.ContentLength+1))
				_ = req.Body.Close()
				if rerr != nil {
					// Failure reading the DOWNSTREAM client body —
					// usually a client disconnect mid-body or a
					// truncated request. This is not an upstream
					// problem, so 400 (writeBadRequest) is the right
					// status; 502 would misattribute it to the app.
					logger.Error("Failed to read request body for replay-safe buffering. Possible client disconnect mid-body.",
						zap.String("method", req.Method),
						zap.Int64("content_length", req.ContentLength),
						zap.Error(rerr))
					writeBadRequest(clientConn)
					return
				}
				if int64(len(b)) != req.ContentLength {
					// Client lied about Content-Length or disconnected
					// mid-body. This is a downstream-client protocol
					// issue, not an upstream failure — return 400 Bad
					// Request rather than 502 so downstream
					// proxies/monitoring don't misattribute the failure
					// to the app.
					logger.Error("Request body length did not match declared Content-Length; treating as malformed client request. If this fires in production, check the downstream client/proxy for truncated uploads, mid-body disconnects, or an incorrectly computed Content-Length header on the request side.",
						zap.String("method", req.Method),
						zap.Int64("content_length", req.ContentLength),
						zap.Int("bytes_read", len(b)))
					writeBadRequest(clientConn)
					return
				}
				preBufferedReqBody = b
				req.Body = io.NopCloser(bytes.NewReader(b))
				canReplay = true
			default:
				// Unknown-length (chunked) or known-too-large body.
				// Stream-forward but disable replay — partial-body
				// replay would be worse than a clean 502.
				canReplay = false
			}
		}

		// Tee body for capture (only when we did NOT pre-buffer; if
		// we did, the buffer IS the captured body bytes). Forwarding
		// still works if the buffer truncates (captureBuffer.Write
		// always reports len(p) bytes consumed regardless of internal
		// limit). Allocate the buffer lazily — if capture is disabled
		// or the body is already pre-buffered or absent, we never
		// touch reqCapture, so don't pay the per-request alloc.
		captureEligible := captureEnabled
		var reqCapture *captureBuffer
		if captureEligible && preBufferedReqBody == nil && req.Body != nil && req.Body != http.NoBody {
			reqCapture = newCaptureBuffer(maxHTTPBodyCaptureBytes)
			req.Body = newTeeReadCloser(req.Body, reqCapture)
		}

		// Sampling-tracked force-close: tell upstream this is a
		// one-shot exchange so it tears the connection down after the
		// response. The sampling slot held by this connection is
		// released when handleHttp1Connection's defer fires — which
		// happens when this loop returns via the req.Close || resp.Close
		// branch at the bottom of the iteration. Without this, a Go
		// upstream's default keep-alive response leaves the slot held
		// for the idle keep-alive window and starves subsequent
		// connections of capture slots.
		if forceCloseActive {
			req.Close = true
			req.Header.Set("Connection", "close")
		}

		// Forward request. On write failure, replay on a fresh upstream
		// for idempotent methods only. The peek narrows the race; the
		// replay catches the residual (FIN arrived between peek and
		// write). A request whose client waits for a 100 (Continue) is
		// written beside the read of its response, which forwards the 100.
		var relay *requestRelay
		if continues {
			relay = relayRequest(req, upConn, clientConn)
		} else if err := req.Write(upConn); err != nil {
			if req.Body != nil {
				_ = req.Body.Close()
			}
			if canReplay {
				logger.Debug("Idempotent request write failed; redial+replay",
					zap.String("method", req.Method), zap.Error(err))
				if rerr := redial(); rerr != nil {
					pm.logAppDialFailure(logger, "Replay redial after request-write failure failed. Verify the upstream application is still listening on the resolved address.",
						rerr, zap.String("upstream", upstreamAddr))
					pm.appGone(clientConn)
					return
				}
				// Replay from the pre-buffered body (or no body if
				// none was sent). Using preBufferedReqBody is what
				// makes this safe — reqCapture may be partial or
				// empty depending on capture state and write timing.
				if preBufferedReqBody != nil {
					req.Body = io.NopCloser(bytes.NewReader(preBufferedReqBody))
				} else {
					req.Body = http.NoBody
				}
				if rerr := req.Write(upConn); rerr != nil {
					logger.Error("Replay of idempotent request failed after redial; upstream may be unhealthy or rejecting writes. Check application status and recent restarts on the resolved address.",
						zap.String("method", req.Method), zap.String("upstream", upstreamAddr), zap.Error(rerr))
					pm.appGone(clientConn)
					return
				}
				if req.Body != nil {
					_ = req.Body.Close()
				}
			} else {
				// Non-idempotent, unknown-length body, or oversized
				// body: cannot safely replay per RFC 9110 §9.2.2.
				// Surface a 502 to the client and tear down so envoy
				// doesn't reuse this conn.
				logger.Error("Forward of request to upstream failed and request is not safely replayable (non-idempotent method or body cannot be re-sent). Verify upstream health on the resolved address.",
					zap.String("method", req.Method),
					zap.String("upstream", upstreamAddr),
					zap.Bool("idempotent", isIdempotentMethod(req.Method)),
					zap.Int64("content_length", req.ContentLength),
					zap.Error(err))
				pm.appGone(clientConn)
				return
			}
		} else if req.Body != nil {
			_ = req.Body.Close()
		}

		// Read response. Empty-response on idempotent methods is also
		// classified as stale-conn; replay once. canReplay is the
		// authoritative gate (see preBufferedReqBody comment above) —
		// don't fall back to reqCapture.Truncated() here because it
		// can be empty even in safe-to-replay cases (capture disabled
		// on the sampling-bypass path).
		//
		// Replay only when the error LOOKS like a connection drop
		// (isStaleConnError). A protocol/parse error from
		// http.ReadResponse (malformed status line, bad headers) is
		// deterministic — replaying just produces the same bad
		// response and double-charges the upstream for an idempotent
		// request that's not even getting through. Nor when the app had
		// begun to answer (an interim response came): the request reached
		// it, and sent again it would run twice.
		//
		// Interim responses (1xx) are forwarded to the client as they come,
		// and the final one is the exchange's (readFinalResponse).
		resp, interims, err := readFinalResponse(upstreamReader, req, clientConn)
		if err != nil {
			relay.abandon()
			var gone *clientWriteError
			if errors.As(err, &gone) {
				logger.Debug("Failed to forward an interim response to client", zap.Error(err))
				return
			}
			if canReplay && interims == 0 && isStaleConnError(err) {
				logger.Debug("Empty response from upstream; redial+replay",
					zap.String("method", req.Method), zap.Error(err))
				if rerr := redial(); rerr != nil {
					pm.logAppDialFailure(logger, "Replay redial after empty-response failed. Verify the upstream application is still listening on the resolved address.",
						rerr, zap.String("upstream", upstreamAddr))
					pm.appGone(clientConn)
					return
				}
				if preBufferedReqBody != nil {
					req.Body = io.NopCloser(bytes.NewReader(preBufferedReqBody))
				} else {
					req.Body = http.NoBody
				}
				if rerr := req.Write(upConn); rerr != nil {
					logger.Error("Replay request write failed after redial; upstream may be unhealthy. Check application status on the resolved address.",
						zap.String("method", req.Method), zap.String("upstream", upstreamAddr), zap.Error(rerr))
					pm.appGone(clientConn)
					return
				}
				if req.Body != nil {
					_ = req.Body.Close()
				}
				resp, _, err = readFinalResponse(upstreamReader, req, clientConn)
				if errors.As(err, &gone) {
					logger.Debug("Failed to forward an interim response to client", zap.Error(err))
					return
				}
				if err != nil {
					logger.Error("Replay response read failed; upstream returned no response on the redialed connection either. Check application logs for crashes/restarts.",
						zap.String("upstream", upstreamAddr), zap.Error(err))
					pm.appGone(clientConn)
					return
				}
			} else {
				// Non-idempotent, unknown-length, or oversized body:
				// surface 502 so the downstream client doesn't see a
				// silent connection close (which presents as a
				// confusing reset/hang rather than a structured 502).
				logger.Error("Failed to read upstream response and request is not safely replayable. Verify upstream application health on the resolved address.",
					zap.String("method", req.Method),
					zap.String("upstream", upstreamAddr),
					zap.Bool("idempotent", isIdempotentMethod(req.Method)),
					zap.Int64("content_length", req.ContentLength),
					zap.Duration("time_since_request", time.Since(reqTimestamp)),
					zap.Error(err))
				pm.appGone(clientConn)
				return
			}
		}

		// The response's headers are read: the earliest its exchange can end.
		headersAt := clockNow()

		// The app has answered. The rest of a body it answered before it came
		// is forwarded, within bounds, before the answer (requestRelay.finish).
		// A request that still did not reach the app whole (an upload its
		// client never sent the body of) is not recorded, neither what the
		// client meant nor what the app got, and the connection ends with its
		// response: where the client's next request would start is not known.
		requestWhole := true
		if rerr := relay.finish(); rerr != nil {
			requestWhole = false
			if captureEnabled {
				warnNotRecorded(logger, req, resp, rerr)
			}
		}

		captureEligible = captureEnabled && requestWhole
		var respCapture *captureBuffer
		if captureEligible && resp.Body != nil && resp.Body != http.NoBody {
			respCapture = newCaptureBuffer(maxHTTPBodyCaptureBytes)
			resp.Body = newTeeReadCloser(resp.Body, respCapture)
		}

		// Sampling-tracked force-close: also mark the client-facing
		// response as close so the downstream client tears down its
		// end after consuming the response. We injected close on the
		// request above, so a well-behaved upstream will already
		// respond with Connection: close — this is the belt-and-
		// suspenders override in case the upstream ignores the request
		// hint and still hands back a keep-alive response. The loop
		// exits at the req.Close || resp.Close check at the bottom of
		// this iteration, which triggers the deferred releaseLock in
		// the caller and frees the sampling slot. appResp is the response
		// as the app sent it, which is what is recorded.
		appResp := resp
		if forceCloseActive || !requestWhole {
			appResp = closeAfter(resp)
		}

		// The response's time, the end of its exchange's window, is taken as
		// its end is found, BEFORE what ends it is forwarded: when the bytes
		// that end it arrived from the app (its headers', for a response with
		// no body), or, for a body that ends when the app closes the
		// connection, when that close was read. The client sees the end only
		// once the clock reads past it (waitPastYield), so the window holds
		// every call the app made for the exchange and nothing its client does
		// on the answer: a sequential client's next request, on this
		// connection or on a new one, and the calls that request makes.
		// Stamped after the write instead, the window ran on for as long as
		// this goroutine waited to run again once its client had the answer,
		// and took the next request's calls: on a loaded host the next test
		// was recorded without them and this one with calls it never made.
		// On Linux the wait returns at once; on a clock as coarse as Windows'
		// it holds the end up to one tick, as the synchronous loop does.
		endAt, ended := headersAt, false
		hasBody := resp.Body != nil && resp.Body != http.NoBody
		closeDelimited := hasBody && resp.ContentLength < 0 && !isChunked(resp.TransferEncoding)
		markEnd := func() {
			ended = true
			if closeDelimited {
				endAt = clockNow()
			} else if last := upWire.LastReadTime(); last.After(endAt) {
				endAt = last
			}
			// An exchange that is not recorded claims nothing: no wait.
			if captureEligible && (reqCapture == nil || !reqCapture.Truncated()) && (respCapture == nil || !respCapture.Truncated()) {
				waitPastYield(win, endAt)
			}
		}
		if hasBody {
			resp.Body = &endWaitBody{ReadCloser: resp.Body, left: resp.ContentLength, atEnd: markEnd}
		} else {
			markEnd()
		}

		if err := resp.Write(clientConn); err != nil {
			if resp.Body != nil {
				_ = resp.Body.Close()
			}
			logger.Debug("Failed to forward response to client", zap.Error(err))
			return
		}
		if resp.Body != nil {
			_ = resp.Body.Close()
		}

		// The window invariant (reqTimestamp <= an egress call's time <=
		// respTimestamp) holds with the end found above. A write that
		// returned without its body's end being read (none does today) ends
		// it now, as before.
		respTimestamp := endAt
		if !ended {
			respTimestamp = clockNow()
		}

		// Async capture (matches synchronous-mode pattern at the call
		// site below — keeps the proxy hot-loop free of dump+parse
		// allocations under load). Body source preference:
		// preBufferedReqBody (eager-buffered for replay safety; always
		// the exact bytes the upstream saw) → reqCapture (tee on the
		// streaming path) → nil (no body / capture disabled).
		// reqCapture / respCapture may be nil when we skipped the
		// per-request alloc (capture disabled, body absent, or body
		// pre-buffered).
		reqTeeTruncated := reqCapture != nil && reqCapture.Truncated()
		respTeeTruncated := respCapture != nil && respCapture.Truncated()
		// The pause is read once, as the request comes: one that starts
		// later in the exchange cuts a body's capture instead, and the
		// exchange it costs is counted here. Not twice: a body is captured
		// only for an exchange the pause had not cost as its request came.
		if reqCapture.LostToPause() || respCapture.LostToPause() {
			notRecordedUnderPressure()
		}
		if captureEligible && !reqTeeTruncated && !respTeeTruncated {
			releaseCaptureSlot, ok := takeCaptureSlot(ctx)
			if !ok {
				return
			}

			var reqBodyBytes []byte
			switch {
			case preBufferedReqBody != nil:
				reqBodyBytes = preBufferedReqBody
			case reqCapture != nil:
				reqBodyBytes = reqCapture.Bytes()
			}
			var respBodyBytes []byte
			if respCapture != nil {
				respBodyBytes = respCapture.Bytes()
			}
			capturedReq := req
			capturedResp := appResp
			capturedReqTS := reqTimestamp
			capturedRespTS := respTimestamp
			actualPort := appPort
			capturedWin := win
			win = nil

			// Being built until it is in the recorder's queue (or given
			// up): a recording's stop waits for it (syncMock
			// BacklogBefore).
			built := syncMock.FromContextOrGlobal(ctx).BuildingTestCase(capturedRespTS)
			pm.captures.Add(1)
			go func() {
				defer pm.captures.Done()
				defer built()
				defer releaseCaptureSlot()
				defer capturedWin.Close() // once the capture has decided it, or skipped it

				exchSize, err := capturedExchangeSize(capturedReq, capturedResp, reqBodyBytes, respBodyBytes)
				if err != nil || exchSize > maxHTTPCombinedCaptureBytes {
					return
				}

				reqData, err := dumpCapturedRequest(capturedReq, reqBodyBytes)
				if err != nil {
					return
				}
				parsedReq, err := pkg.ParseHTTPRequest(reqData)
				if err != nil {
					return
				}
				respData, err := dumpCapturedResponse(capturedResp, parsedReq, respBodyBytes)
				if err != nil {
					return
				}
				parsedResp, err := pkg.ParseHTTPResponse(respData, parsedReq)
				if err != nil {
					return
				}
				defer parsedReq.Body.Close()
				defer parsedResp.Body.Close()
				hooksUtils.CaptureHook(proxyWindowClock(syncMock.WithWindow(syncMock.WithIngressConn(ctx, connName), capturedWin)), logger, t, parsedReq, parsedResp, capturedReqTS, capturedRespTS, pm.loadIncomingOpts(), pm.synchronous, pm.mapping, actualPort)
			}()
		}
		// Not captured: nothing will decide it.
		win.Close()
		win = nil

		// Honor close signals from either side.
		if req.Close || resp.Close {
			if !requestWhole {
				// Its client may still be sending the body cut off.
				if releaseSlot != nil {
					releaseSlot()
				}
				lingerClose(clientConn)
			}
			return
		}
	}
}

// ingressConnName names an ingress connection, its client's address and the
// app's, for the count of the requests a pause of the memory guard costs
// (syncMock NoteNotRecordedUnderPressure).
func ingressConnName(clientConn net.Conn, app string) string {
	client := "unknown client"
	if a := clientConn.RemoteAddr(); a != nil {
		client = a.String()
	}
	return client + " -> " + app
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
	// The FIN forwarding below used to assert *net.TCPConn concretely,
	// which silently skipped every *tls.Conn and every wrapped conn —
	// the capability is what matters here, not the concrete type.
	//
	// UNCONDITIONAL, unlike the record paths in integrations/. Those gate
	// on a clean copy so a truncated request is never reported to the
	// peer as complete — a record-FIDELITY argument, and this pump
	// captures nothing. What it does instead is join on both copies, so
	// the FIN is what unblocks the peer io.Copy: signalling only on a
	// clean exit leaves a client-side error with nothing to wake the
	// upstream side, and two goroutines plus two sockets sit there until
	// the upstream closes on its own. On an idle WebSocket that is
	// minutes.
	go func() {
		_, _ = io.Copy(upConn, clientConn)
		_ = proxyutil.CloseWriteIfPossible(upConn)
		done <- struct{}{}
	}()
	go func() {
		_, _ = io.Copy(clientConn, upConn)
		_ = proxyutil.CloseWriteIfPossible(clientConn)
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

// proxyWindowClock is ctx carrying the clock an exchange's window start is on
// (hooksUtils.WithWindowClock): the proxy's. Both HTTP loops
// stamp a request with the proxy's time.Now as its bytes arrived, and open its
// window at that stamp before they forward it to the app (OpenWindow), so an
// egress call the app makes for it is stamped no earlier on that same clock.
func proxyWindowClock(ctx context.Context) context.Context {
	return hooksUtils.WithWindowClock(ctx, fakeconn.NameProxyClock())
}
