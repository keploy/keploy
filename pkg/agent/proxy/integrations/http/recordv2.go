package http

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"go.keploy.io/server/v3/pkg"
	"go.keploy.io/server/v3/pkg/agent/proxy/fakeconn"
	"go.keploy.io/server/v3/pkg/agent/proxy/supervisor"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
)

// recordV2 is the FakeConn-based record path. The relay owns and writes the
// real sockets; recordV2 only observes the teed chunks via sess.ClientStream
// / sess.DestStream and emits mocks with timestamps derived from the chunks'
// ReadAt / WrittenAt fields (never from time.Now()).
//
// The mock payload (headers, body, metadata, URL, method, status) is
// constructed identically to the legacy path via buildHTTPMock, so
// mock.Spec.HTTPReq / HTTPResp are byte-equivalent between the two paths
// for the same input traffic. Delivery differs: V2 goes through
// sess.EmitMock (which runs the post-record hook chain and respects the
// incomplete-mock gate) rather than the syncMock / mocks channel shim
// the legacy path uses.
//
// recordV2 loops over request/response pairs for HTTP/1.1 keepalive /
// pipelining. It exits cleanly on either stream reaching EOF or Close,
// on ctx cancellation, or on a malformed-HTTP decode error (in which
// case it marks the session's mock incomplete so the supervisor can
// abort and fall through to passthrough).
func (h *HTTP) recordV2(ctx context.Context, sess *supervisor.Session) error {
	if sess == nil {
		return errors.New("recordV2: nil supervisor session")
	}
	logger := sess.Logger
	if logger == nil {
		logger = h.Logger
	}

	destPort := destPortFromAddr(sess.DestStream)

	// watchdogSuspended latches once this connection's watchdog has been
	// disarmed for a poll lane, so we don't re-parse and re-match every
	// subsequent request on a keep-alive poll-poll-poll connection.
	watchdogSuspended := false

	for {
		if err := ctx.Err(); err != nil {
			return err
		}

		// --- Request side ---------------------------------------------
		//
		// First chunk's ReadAt is the request arrival timestamp. We grab
		// it via ReadChunk so the timestamp is carried regardless of
		// what ReadBytes does underneath.
		firstChunk, err := sess.ClientStream.ReadChunk()
		if err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, fakeconn.ErrClosed) {
				logger.Debug("V2 HTTP record: client stream ended at start of request", zap.Error(err))
				return nil
			}
			if ctx.Err() != nil {
				return ctx.Err()
			}
			utils.LogError(logger, err, "V2 HTTP record: initial request read failed")
			return err
		}
		if len(firstChunk.Bytes) == 0 {
			// Empty synthetic chunk (channel close sentinel): treat as EOF.
			return nil
		}
		reqTs := firstChunk.ReadAt
		finalReq := append([]byte(nil), firstChunk.Bytes...)

		// Complete the request: read more chunks until headers + body
		// are on hand. We use chunk-level reads (not HandleChunkedRequests
		// which wraps bufio-like bulk reads over the conn) so we never
		// over-read past the end of one request into the start of the
		// next pipelined request. This is essential for HTTP/1.1
		// keepalive correctness.
		if err := h.readRequestV2(ctx, sess.ClientStream, &finalReq); err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, fakeconn.ErrClosed) {
				logger.Debug("V2 HTTP record: client stream closed mid-request", zap.Error(err))
				return nil
			}
			if ctx.Err() != nil {
				return ctx.Err()
			}
			sess.MarkMockIncomplete("http decode error: request read failed: " + err.Error())
			utils.LogError(logger, err, "V2 HTTP record: failed to read full request")
			return err
		}

		// If this request routes to a long-poll async lane, disarm the
		// supervisor's hang watchdog for this connection before we block on
		// the response: a poll legitimately holds the connection open with no
		// byte progress for far longer than the hang budget, and would
		// otherwise be aborted (falling through to passthrough) before the
		// delivery arrives — so the poll's mock would never be recorded.
		if !watchdogSuspended && sess.SuspendWatchdog != nil && h.isPollLaneRequest(finalReq) {
			logger.Debug("V2 HTTP record: request matched a poll lane; suspending hang watchdog for this connection")
			sess.SuspendWatchdog()
			watchdogSuspended = true
		}

		// --- Response side --------------------------------------------
		firstRespChunk, err := sess.DestStream.ReadChunk()
		if err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, fakeconn.ErrClosed) {
				sess.MarkMockIncomplete("http decode error: server closed before response")
				logger.Debug("V2 HTTP record: dest stream ended before response", zap.Error(err))
				return nil
			}
			if ctx.Err() != nil {
				return ctx.Err()
			}
			sess.MarkMockIncomplete("http decode error: initial response read failed: " + err.Error())
			utils.LogError(logger, err, "V2 HTTP record: initial response read failed")
			return err
		}
		if len(firstRespChunk.Bytes) == 0 {
			sess.MarkMockIncomplete("http decode error: empty initial response chunk")
			return nil
		}
		finalResp := append([]byte(nil), firstRespChunk.Bytes...)
		resTs := firstRespChunk.WrittenAt

		// The request method decides whether the response may carry a body at
		// all (RFC 9112 section 6.3 — a response to HEAD never does), so it has
		// to travel with the response read.
		reqMethod := httpRequestMethod(finalReq)
		gotLastWritten, err := h.readResponseV2(ctx, sess.DestStream, &finalResp, reqMethod)
		if err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, fakeconn.ErrClosed) {
				// Legacy encodeHTTP treats EOF after some bytes as
				// end-of-response. Respect that shape: emit what we have.
				if !gotLastWritten.IsZero() {
					resTs = gotLastWritten
				}
			} else {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				sess.MarkMockIncomplete("http decode error: response read failed: " + err.Error())
				utils.LogError(logger, err, "V2 HTTP record: failed to read full response")
				return err
			}
		} else if !gotLastWritten.IsZero() {
			resTs = gotLastWritten
		}

		// Build and emit the mock. Identical shape to the legacy path.
		mock, err := h.buildHTTPMock(&FinalHTTP{
			Req:              finalReq,
			Resp:             finalResp,
			ReqTimestampMock: reqTs,
			ResTimestampMock: resTs,
		}, destPort, sess.ClientConnID, sess.Opts)
		if err != nil {
			sess.MarkMockIncomplete("http decode error: " + err.Error())
			utils.LogError(logger, err, "V2 HTTP record: failed to build mock")
			return err
		}
		if mock != nil {
			if emitErr := sess.EmitMock(mock); emitErr != nil {
				return emitErr
			}
		}
		sess.MarkMockComplete()
	}
}

// readRequestV2 is the V2 counterpart of HandleChunkedRequests. It
// consumes the remainder of an HTTP/1 request from stream, appending
// bytes to *finalReq. Unlike HandleChunkedRequests it uses ReadChunk
// so we pull exactly one tee'd chunk at a time and never over-read
// past the end of the current request into the start of the next.
//
// Returns io.EOF if stream closes before the body is fully consumed.
// Returns a decode error for malformed Content-Length / body framing.
func (h *HTTP) readRequestV2(ctx context.Context, stream *fakeconn.FakeConn, finalReq *[]byte) error {
	// 1. Complete headers.
	for !hasCompleteHeaders(*finalReq) {
		if err := ctx.Err(); err != nil {
			return err
		}
		chunk, err := stream.ReadChunk()
		if err != nil {
			return err
		}
		if len(chunk.Bytes) == 0 {
			return io.EOF
		}
		*finalReq = append(*finalReq, chunk.Bytes...)
	}

	contentLengthHeader, transferEncodingHeader := parseHeaders(*finalReq)

	if contentLengthHeader != "" {
		contentLength, err := strconv.Atoi(contentLengthHeader)
		if err != nil {
			return fmt.Errorf("invalid content-length: %w", err)
		}
		headerEnd := bytes.Index(*finalReq, []byte("\r\n\r\n"))
		if headerEnd < 0 {
			return fmt.Errorf("header terminator missing")
		}
		bodyLength := len(*finalReq) - headerEnd - 4
		remaining := contentLength - bodyLength
		for remaining > 0 {
			if err := ctx.Err(); err != nil {
				return err
			}
			chunk, err := stream.ReadChunk()
			if err != nil {
				return err
			}
			if len(chunk.Bytes) == 0 {
				return io.EOF
			}
			*finalReq = append(*finalReq, chunk.Bytes...)
			remaining -= len(chunk.Bytes)
		}
		return nil
	}

	if transferEncodingHeader != "" &&
		strings.Contains(strings.ToLower(transferEncodingHeader), "chunked") {
		headerEnd := bytes.Index(*finalReq, []byte("\r\n\r\n"))
		if headerEnd < 0 {
			return fmt.Errorf("header terminator missing")
		}
		return h.consumeChunkedBody(ctx, stream, finalReq, headerEnd+4, nil)
	}

	// No body framing: the request is headers-only (e.g. GET / HTTP/1.1
	// with no body). We're done.
	return nil
}

// readResponseV2 is the V2 counterpart of handleChunkedResponses. It
// consumes the remainder of an HTTP/1 response from stream, appending
// bytes to *finalResp, and returns the WrittenAt timestamp of the last
// chunk it consumed. It does NOT write to any client connection — the
// relay owns forwarding on the V2 path.
//
// The completion logic mirrors handleChunkedResponses:
//   - Pull more chunks until response headers are complete.
//   - Parse Content-Length / Transfer-Encoding from headers.
//   - If Content-Length, read until the declared body length is met.
//   - If Transfer-Encoding chunked, walk the chunk framing to its true end
//     (see consumeChunkedBody / chunkedFramer).
//
// Returns (lastWrittenAt, err). err may be io.EOF for the legitimate
// "server closed after sending a full response" case; the caller
// decides whether to emit a mock.
func (h *HTTP) readResponseV2(ctx context.Context, stream *fakeconn.FakeConn, finalResp *[]byte, reqMethod string) (time.Time, error) {
	var lastWr time.Time

	// 1. Complete headers, discarding any INTERIM 1xx response first.
	//
	// RFC 9110 section 15.2: an interim response (100 Continue, 103 Early Hints)
	// is not the response — the real one follows on the same connection. Framing
	// the interim as if it were the response leaves the real response unread and,
	// via the read-until-EOF fallback below, swallows every later exchange on the
	// connection. 101 is deliberately NOT skipped: it IS the final response for
	// an upgrade, so it is framed and recorded like any other bodyless
	// response. What follows on the wire is no longer HTTP, and the parser
	// blocks there until the hang watchdog retires it -- deliberately, because
	// returning cleanly would end the supervisor run with a status that does
	// not preserve the byte relay, killing the upgraded connection.
	for interim := 0; ; interim++ {
		for !hasCompleteHeaders(*finalResp) {
			if err := ctx.Err(); err != nil {
				return lastWr, err
			}
			chunk, err := stream.ReadChunk()
			if err != nil {
				return lastWr, err
			}
			if !chunk.WrittenAt.IsZero() {
				lastWr = chunk.WrittenAt
			}
			if len(chunk.Bytes) == 0 {
				return lastWr, io.EOF
			}
			*finalResp = append(*finalResp, chunk.Bytes...)
		}
		code := httpStatusCode(*finalResp)
		if code < 100 || code >= 200 || code == 101 {
			break
		}
		if interim >= maxInterimResponses {
			return lastWr, fmt.Errorf("more than %d consecutive interim (1xx) responses", maxInterimResponses)
		}
		end := bytes.Index(*finalResp, []byte("\r\n\r\n"))
		if end < 0 {
			break
		}
		h.Logger.Debug("V2 HTTP record: discarding an interim 1xx response and reading the real one",
			zap.Int("status", code))
		*finalResp = append([]byte(nil), (*finalResp)[end+4:]...)
	}

	// 2. Bodyless by status/method (RFC 9112 section 6.3) — must be checked
	//    BEFORE Content-Length / Transfer-Encoding, which are present on plenty
	//    of 304s and HEAD responses and describe the body the response WOULD
	//    have had. See responseHasNoBody.
	if code := httpStatusCode(*finalResp); responseHasNoBody(code, reqMethod) {
		return lastWr, nil
	}

	// 3. Parse headers for body framing.
	contentLengthHeader, transferEncodingHeader := parseHeaders(*finalResp)

	if contentLengthHeader != "" {
		contentLength, err := strconv.Atoi(contentLengthHeader)
		if err != nil {
			return lastWr, fmt.Errorf("invalid content-length: %w", err)
		}
		headerEnd := bytes.Index(*finalResp, []byte("\r\n\r\n"))
		if headerEnd < 0 {
			return lastWr, fmt.Errorf("header terminator missing")
		}
		bodyLength := len(*finalResp) - headerEnd - 4
		remaining := contentLength - bodyLength
		for remaining > 0 {
			if err := ctx.Err(); err != nil {
				return lastWr, err
			}
			chunk, err := stream.ReadChunk()
			if err != nil {
				return lastWr, err
			}
			if !chunk.WrittenAt.IsZero() {
				lastWr = chunk.WrittenAt
			}
			if len(chunk.Bytes) == 0 {
				return lastWr, io.EOF
			}
			*finalResp = append(*finalResp, chunk.Bytes...)
			remaining -= len(chunk.Bytes)
		}
		return lastWr, nil
	}

	if transferEncodingHeader != "" &&
		strings.Contains(strings.ToLower(transferEncodingHeader), "chunked") {
		headerEnd := bytes.Index(*finalResp, []byte("\r\n\r\n"))
		if headerEnd < 0 {
			return lastWr, fmt.Errorf("header terminator missing")
		}
		err := h.consumeChunkedBody(ctx, stream, finalResp, headerEnd+4, func(c fakeconn.Chunk) {
			if !c.WrittenAt.IsZero() {
				lastWr = c.WrittenAt
			}
		})
		return lastWr, err
	}

	// Neither Content-Length nor chunked: read until EOF (RFC 7230
	// permits this for HTTP/1.0 and for responses with "Connection:
	// close"). The loop relies on the stream eventually closing.
	//
	// Say so. On a keep-alive connection the stream does NOT close after this
	// response, so this loop consumes every later exchange as body and none of
	// them is ever recorded — and because the parser then returns nil, the
	// supervisor records StatusOK and no retire warning is emitted. That is
	// silent recording loss, and it stayed invisible for exactly as long as this
	// branch said nothing.
	h.Logger.Warn("V2 HTTP record: response has neither Content-Length nor chunked framing; reading until the connection closes",
		zap.Int("status", httpStatusCode(*finalResp)),
		zap.String("method", reqMethod),
		zap.String("impact", "if this connection is keep-alive, later requests on it will be consumed as this response's body and not recorded"))
	for {
		if err := ctx.Err(); err != nil {
			return lastWr, err
		}
		chunk, err := stream.ReadChunk()
		if err != nil {
			return lastWr, err
		}
		if !chunk.WrittenAt.IsZero() {
			lastWr = chunk.WrittenAt
		}
		if len(chunk.Bytes) == 0 {
			return lastWr, io.EOF
		}
		*finalResp = append(*finalResp, chunk.Bytes...)
	}
}

// maxInterimResponses bounds the 1xx skip loop so a peer that only ever sends
// interim responses cannot spin here.
const maxInterimResponses = 8

// httpStatusCode returns the status code from a response's start line, or 0 if
// it cannot be parsed. Deliberately a byte-level parse of the first line rather
// than http.ReadResponse: this runs before the body has been framed, which is
// precisely what ReadResponse would need.
func httpStatusCode(resp []byte) int {
	end := bytes.IndexByte(resp, '\n')
	if end < 0 {
		end = len(resp)
	}
	fields := strings.Fields(string(resp[:end]))
	if len(fields) < 2 || !strings.HasPrefix(fields[0], "HTTP/") {
		return 0
	}
	code, err := strconv.Atoi(fields[1])
	if err != nil {
		return 0
	}
	return code
}

// httpRequestMethod returns the method from a request's start line, uppercased,
// or "" if it cannot be parsed. Needed because RFC 9112 section 6.3 makes the
// response to HEAD bodyless regardless of its own headers.
func httpRequestMethod(req []byte) string {
	end := bytes.IndexByte(req, '\n')
	if end < 0 {
		end = len(req)
	}
	fields := strings.Fields(string(req[:end]))
	if len(fields) == 0 {
		return ""
	}
	return strings.ToUpper(fields[0])
}

// responseHasNoBody reports whether RFC 9112 section 6.3 forbids a message body
// on this response, in which case Content-Length / Transfer-Encoding must be
// ignored rather than used for framing.
//
// Getting this wrong is not a cosmetic bug. Framing a bodyless response from its
// headers sends readResponseV2 into either the Content-Length wait or the
// read-until-EOF fallback, and it then consumes every LATER exchange on the same
// keep-alive connection as if it were this response's body. Chromium revalidates
// static assets (304) and preflights CORS (204) constantly, so on a browser
// workload this silently destroys most of a connection's recording.
func responseHasNoBody(code int, reqMethod string) bool {
	switch {
	case code == 204, code == 304:
		return true
	case code >= 100 && code < 200:
		// 101 reaches here; 1xx below it is consumed by the interim skip.
		return true
	case strings.EqualFold(reqMethod, "HEAD"):
		return true
	case code >= 200 && code < 300 && strings.EqualFold(reqMethod, "CONNECT"):
		// RFC 9112 section 6.3: a 2xx to CONNECT turns the connection into a
		// tunnel, so the response itself has no body and everything after it is
		// opaque tunnelled bytes. Like a 101 it carries neither Content-Length
		// nor chunked framing, so without this it lands in the read-until-close
		// branch and swallows the rest of the connection.
		return true
	}
	return false
}

// parseHeaders extracts Content-Length and Transfer-Encoding header
// values (if any) from an HTTP message whose headers end with CRLFCRLF.
// The parse is the same loose style the chunk.go helpers use: split on
// '\n', trim '\r', skip malformed lines.
func parseHeaders(msg []byte) (contentLength string, transferEncoding string) {
	lines := strings.Split(string(msg), "\n")
	for _, line := range lines {
		line = strings.TrimRight(line, "\r")
		if line == "" {
			break // end of headers
		}
		parts := strings.SplitN(line, ":", 2)
		if len(parts) != 2 {
			continue
		}
		key := strings.ToLower(strings.TrimSpace(parts[0]))
		val := strings.TrimSpace(parts[1])
		switch key {
		case "content-length":
			contentLength = val
		case "transfer-encoding":
			transferEncoding = val
		}
	}
	return contentLength, transferEncoding
}

// destPortFromAddr extracts the destination TCP port from a FakeConn's
// RemoteAddr, falling back to 0 if the address is not a TCP address
// (e.g. a net.Pipe under tests). Port is only used for passthrough
// rule matching — 0 reliably matches no rule so tests without real
// sockets still exercise the non-passthrough path.
func destPortFromAddr(conn net.Conn) uint {
	if conn == nil {
		return 0
	}
	addr := conn.RemoteAddr()
	if addr == nil {
		return 0
	}
	if tcp, ok := addr.(*net.TCPAddr); ok {
		return uint(tcp.Port)
	}
	return 0
}

// isPollLaneRequest reports whether the raw HTTP request routes to a
// configured async lane whose type is a poll variant (lane.IsPoll()).
// Matching is by request shape only (host/path/query, via the engine's
// LaneFor), so it is valid at record time before any mock is emitted.
// recordV2 uses it to disarm the supervisor's hang watchdog for long-poll
// connections. Returns false when no async engine is configured or the
// request is malformed.
func (h *HTTP) isPollLaneRequest(rawReq []byte) bool {
	if h.asyncEngine == nil || !h.asyncEngine.HasPollLanes() {
		return false
	}
	parsed, err := http.ReadRequest(bufio.NewReader(bytes.NewReader(rawReq)))
	if err != nil {
		return false
	}
	// Reuse liveReqToMock (the same request→mock shaping the replay LaneFor
	// path uses in decode.go) so record- and replay-time lane matching stay
	// consistent — in particular it leaves URLParams unset so queryOf does the
	// full multi-value query parse rather than a truncated first-value view.
	lane, ok := h.asyncEngine.LaneFor(liveReqToMock(&req{
		method: parsed.Method,
		url:    parsed.URL,
		header: parsed.Header,
	}))
	return ok && lane.IsPoll()
}

// buildHTTPMock constructs a *models.Mock with the same shape the legacy
// parseFinalHTTP produces. Returns (nil, nil) when the request is a
// pass-through (IsPassThrough true) so the caller skips emission.
// Returns a decode error only for malformed HTTP; timestamps are carried
// through unchanged from `m`.
//
// The construction must stay byte-equivalent to parseFinalHTTP. If the
// legacy helper gains a new field, mirror it here. A parity test in
// recordv2_test.go asserts field-by-field equivalence on identical
// input bytes (modulo timestamps, which are allowed to differ because
// legacy uses time.Now() and V2 uses chunk timestamps).
func (h *HTTP) buildHTTPMock(m *FinalHTTP, destPort uint, connID string, opts models.OutgoingOptions) (*models.Mock, error) {
	// Prefer the resolved destination port. The V2 recorder derives destPort from
	// the DestStream FakeConn, which in proxyless (observe-only) capture has no
	// real remote port (0) — breaking port-keyed telemetry-passthrough matching.
	// routeEgressToParser sets DstCfg.Port authoritatively; equal to the FakeConn
	// port on the proxy path, so this is a no-op there.
	if opts.DstCfg != nil && opts.DstCfg.Port != 0 {
		destPort = opts.DstCfg.Port
	}
	req, err := http.ReadRequest(bufio.NewReader(bytes.NewReader(m.Req)))
	if err != nil {
		return nil, fmt.Errorf("parse request: %w", err)
	}
	req.Header.Set("Host", req.Host)

	var reqBody []byte
	if req.Body != nil {
		reqBody, err = io.ReadAll(req.Body)
		if err != nil {
			return nil, fmt.Errorf("read request body: %w", err)
		}
		if req.Header.Get("Content-Encoding") != "" {
			reqBody, err = pkg.Decompress(h.Logger, req.Header.Get("Content-Encoding"), reqBody, pkg.MaxDecompressedSize)
			if err != nil {
				return nil, fmt.Errorf("decompress request body: %w", err)
			}
		}
	}

	respParsed, err := http.ReadResponse(bufio.NewReader(bytes.NewReader(m.Resp)), req)
	if err != nil {
		return nil, fmt.Errorf("parse response: %w", err)
	}

	var respBody []byte
	if respParsed.Body != nil {
		respBody, err = io.ReadAll(respParsed.Body)
		if err != nil {
			return nil, fmt.Errorf("read response body: %w", err)
		}
		if respParsed.Header.Get("Content-Encoding") != "" {
			respBody, err = pkg.Decompress(h.Logger, respParsed.Header.Get("Content-Encoding"), respBody, pkg.MaxDecompressedSize)
			if err != nil {
				return nil, fmt.Errorf("decompress response body: %w", err)
			}
		}
		respParsed.Header.Set("Content-Length", strconv.Itoa(len(respBody)))
	}

	meta := map[string]string{
		"name":      "Http",
		"type":      models.HTTPClient,
		"operation": req.Method,
		"connID":    connID,
	}

	if utils.IsPassThrough(h.Logger, req, destPort, opts) {
		h.Logger.Debug("V2: request is a passThrough, skipping mock emit",
			zap.Any("metadata", utils.GetReqMeta(req)))
		return nil, nil
	}

	// Telemetry / noisy-egress passthrough (config-driven via
	// opts.PassThroughPorts/Hosts, with built-in defaults as "skip"):
	//   - skip:      do not record (live telemetry, not a dependency).
	//   - recordOne: keep ONE representative exchange per (method,host,port,path),
	//                preferring the first 2xx, tagged type:config so it becomes a
	//                session-lifetime mock served body-agnostically on replay.
	var ptRecordOne bool
	var ptKey string
	{
		ptHost := req.Host
		if ptHost == "" && req.URL != nil {
			ptHost = req.URL.Host
		}
		if rule, matched := passThroughRecordDecision(opts, ptHost, uint32(destPort), req.Method, req.URL); matched {
			if rule.Mode == models.PassThroughSkip {
				h.Logger.Debug("egress passthrough: skip mode, not recording", zap.Any("metadata", utils.GetReqMeta(req)))
				return nil, nil
			}
			// recordOne: defer the dedup decision to emit time (below) so a later
			// drop can't burn the one representative and leave zero mocks.
			ptRecordOne = true
			ptKey = ptRecordKey(opts.PassThroughScope, req.Method, ptHost, uint32(destPort), req.URL.Path, rule.QueryKeys, req.URL.Query())
			meta["type"] = "config"
			meta["passthrough"] = string(models.PassThroughRecordOne)
		}
	}

	ptLifetime := models.LifetimePerTest
	if ptRecordOne {
		ptLifetime = models.LifetimeSession
	}
	mock := &models.Mock{
		Version: models.GetVersion(),
		Name:    "mocks",
		Kind:    models.HTTP,
		Spec: models.MockSpec{
			Metadata: meta,
			HTTPReq: &models.HTTPReq{
				Method:     models.Method(req.Method),
				ProtoMajor: req.ProtoMajor,
				ProtoMinor: req.ProtoMinor,
				URL:        req.URL.String(),
				Header:     pkg.ToYamlHTTPHeader(req.Header),
				Body:       string(reqBody),
				URLParams:  pkg.URLParams(req),
			},
			HTTPResp: &models.HTTPResp{
				StatusCode: respParsed.StatusCode,
				Header:     pkg.ToYamlHTTPHeader(respParsed.Header),
				Body:       string(respBody),
			},
			Created:          time.Now().Unix(),
			ReqTimestampMock: m.ReqTimestampMock,
			ResTimestampMock: m.ResTimestampMock,
		},
		TestModeInfo: models.TestModeInfo{
			Lifetime:        ptLifetime,
			LifetimeDerived: true,
		},
	}
	// recordOne dedup, committed only now that a mock is actually being emitted.
	if ptRecordOne && h.ptRecorder != nil && !h.ptRecorder.shouldRecord(ptKey, respParsed.StatusCode) {
		h.Logger.Debug("egress passthrough: recordOne, dropping duplicate telemetry mock", zap.Any("metadata", utils.GetReqMeta(req)))
		return nil, nil
	}
	return mock, nil
}

// consumeChunkedBody reads from stream until the RFC 9112 section 7.1 chunked
// body that begins at bodyStart in *msg is completely framed, appending every
// chunk it reads to *msg. onChunk, when non-nil, is called for each chunk read
// so a caller that anchors timestamps to chunk metadata can record them.
//
// On return *msg ends exactly at the last byte of the chunked body, so the
// caller can hand it to http.ReadResponse / http.ReadRequest unchanged.
func (h *HTTP) consumeChunkedBody(
	ctx context.Context,
	stream *fakeconn.FakeConn,
	msg *[]byte,
	bodyStart int,
	onChunk func(fakeconn.Chunk),
) error {
	f := newChunkedFramer(bodyStart)
	for {
		done, err := f.advance(*msg)
		if err != nil {
			return err
		}
		if done {
			// Overshoot: one tee'd chunk carried the tail of this message
			// and the head of the next one. Framing knows exactly where the
			// split is; the recorder loop does not yet carry those bytes into
			// the next exchange, so say so rather than lose them silently.
			if end := f.End(); end < len(*msg) {
				h.Logger.Warn("V2 HTTP record: bytes arrived past the end of a chunked body",
					zap.Int("overshoot_bytes", len(*msg)-end),
					zap.String("impact", "these bytes are the head of the next exchange on this connection; it will not frame and will not be recorded"))
				*msg = (*msg)[:end]
			}
			return nil
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		chunk, err := stream.ReadChunk()
		if err != nil {
			return err
		}
		if onChunk != nil {
			onChunk(chunk)
		}
		if len(chunk.Bytes) == 0 {
			return io.EOF
		}
		*msg = append(*msg, chunk.Bytes...)
	}
}

// Chunked framing, RFC 9112 section 7.1. The recorder has to know exactly
// where a chunked body ends, because on a keep-alive connection the next byte
// is the first byte of the next exchange.
//
// Searching the accumulated buffer for the last-chunk marker "0\r\n\r\n"
// cannot do that, and is wrong in both directions:
//
//   - It misses a real end. The last chunk may carry a chunk extension
//     ("0;pad=x\r\n\r\n"), and between the last chunk and the final CRLF sits
//     the trailer section — arbitrary header lines ("0\r\nGrpc-Status: 5\r\n\r\n").
//     Neither ends in the marker, so the read loop never stops and consumes
//     every later exchange on the connection as this body.
//   - It finds an end that is not there. A data chunk whose payload ends in
//     "0\r\n" is followed by the framing CRLF, so the wire genuinely reads
//     "...0\r\n\r\n" in mid-body; if a tee'd chunk boundary falls there, the
//     marker IS the buffer's suffix and the body is cut short.
//
// chunkedFramer walks the framing instead: chunk-size line, that many data
// bytes, CRLF, repeat until a zero-size chunk, then trailer lines up to the
// terminating empty line. It keeps its cursor across calls so each byte is
// examined once — rescanning the buffer per arriving chunk would be quadratic
// on a large streamed body.
const (
	// maxChunkLineLen bounds a chunk-size or trailer line so a peer that
	// never sends the LF cannot make the recorder buffer without bound.
	// Same value Go's own chunked reader uses.
	maxChunkLineLen = 4096
	// maxChunkedTrailerBytes bounds the whole trailer section, which is
	// otherwise an unbounded run of header lines.
	maxChunkedTrailerBytes = 16 << 10
)

type chunkedFramerState uint8

const (
	chunkExpectSize chunkedFramerState = iota
	chunkExpectData
	chunkExpectDataCRLF
	chunkExpectTrailer
	chunkComplete
)

type chunkedFramer struct {
	state        chunkedFramerState
	pos          int // cursor into the accumulated message buffer
	remaining    int // data bytes still owed by the chunk being read
	trailerBytes int
}

// newChunkedFramer starts a framer at bodyStart, the offset of the first byte
// after the header section's CRLFCRLF.
func newChunkedFramer(bodyStart int) *chunkedFramer { return &chunkedFramer{pos: bodyStart} }

// advance consumes as much of buf as the framing allows.
//
// done=true means the trailer section's terminating CRLF has been consumed and
// End() is the offset one past the body. done=false with a nil error means the
// framing is well formed so far but incomplete: read more, append to buf, call
// again.
//
// A non-nil error means these bytes are not valid chunked framing. It is
// returned at once rather than read through, so garbled framing costs one
// connection's capture and never a hung parser.
func (f *chunkedFramer) advance(buf []byte) (bool, error) {
	for {
		switch f.state {
		case chunkComplete:
			return true, nil

		case chunkExpectSize:
			line, next, ok, err := chunkLine(buf, f.pos)
			if err != nil || !ok {
				return false, err
			}
			if i := bytes.IndexByte(line, ';'); i >= 0 {
				line = line[:i] // chunk extension: "size;name=value"
			}
			line = bytes.TrimRight(line, " \t")
			if len(line) == 0 {
				return false, errors.New("chunked framing: empty chunk-size line")
			}
			// bitSize 31: a single chunk larger than 2 GiB is not real
			// traffic, and accepting one would overflow int on a 32-bit build.
			size, err := strconv.ParseUint(string(line), 16, 31)
			if err != nil {
				return false, fmt.Errorf("chunked framing: bad chunk size %q: %w", line, err)
			}
			f.pos = next
			if size == 0 {
				f.state = chunkExpectTrailer
				continue
			}
			f.remaining = int(size)
			f.state = chunkExpectData

		case chunkExpectData:
			n := len(buf) - f.pos
			if n > f.remaining {
				n = f.remaining
			}
			f.pos += n
			f.remaining -= n
			if f.remaining > 0 {
				return false, nil
			}
			f.state = chunkExpectDataCRLF

		case chunkExpectDataCRLF:
			if len(buf)-f.pos < 2 {
				return false, nil
			}
			if buf[f.pos] != '\r' || buf[f.pos+1] != '\n' {
				return false, errors.New("chunked framing: chunk data not followed by CRLF")
			}
			f.pos += 2
			f.state = chunkExpectSize

		case chunkExpectTrailer:
			line, next, ok, err := chunkLine(buf, f.pos)
			if err != nil || !ok {
				return false, err
			}
			f.trailerBytes += next - f.pos
			f.pos = next
			if len(line) == 0 {
				f.state = chunkComplete
				return true, nil
			}
			if f.trailerBytes > maxChunkedTrailerBytes {
				return false, fmt.Errorf("chunked framing: trailer section exceeds %d bytes", maxChunkedTrailerBytes)
			}
		}
	}
}

// End is the offset one past the chunked body, valid once advance has reported
// done. Bytes at or after it belong to the next message on the connection.
func (f *chunkedFramer) End() int { return f.pos }

// chunkLine returns the LF-terminated line at buf[pos:] with its trailing CR
// stripped, plus the offset just past the LF. ok=false with a nil error means
// the line has not fully arrived yet.
func chunkLine(buf []byte, pos int) (line []byte, next int, ok bool, err error) {
	i := bytes.IndexByte(buf[pos:], '\n')
	if i < 0 {
		if len(buf)-pos > maxChunkLineLen {
			return nil, 0, false, fmt.Errorf("chunked framing: no LF within %d bytes", maxChunkLineLen)
		}
		return nil, 0, false, nil
	}
	if i > maxChunkLineLen {
		return nil, 0, false, fmt.Errorf("chunked framing: line exceeds %d bytes", maxChunkLineLen)
	}
	return bytes.TrimSuffix(buf[pos:pos+i], []byte("\r")), pos + i + 1, true, nil
}
