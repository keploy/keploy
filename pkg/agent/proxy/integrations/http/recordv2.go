package http

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/andybalholm/brotli"
	"go.keploy.io/server/v3/pkg"
	"go.keploy.io/server/v3/pkg/agent/proxy/fakeconn"
	"go.keploy.io/server/v3/pkg/agent/proxy/supervisor"
	pUtil "go.keploy.io/server/v3/pkg/agent/proxy/util"
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
// pipelining. Each request starts where Session.NextRequest finds it, so its
// response is what the server sent after the request was captured: server
// bytes captured before it (the answer to a request in flight when the capture
// began or the agent restarted, a 408 the server sent on its own) are never
// paired with it, and NextRequest says which of them it reports: it ends
// with Session.EndExchanges, however it ends, so they are reported though it
// never reads the server's stream past them. It exits
// cleanly on either stream reaching EOF or Close,
// on ctx cancellation, or on a malformed-HTTP decode error (the error is
// returned, and the supervisor falls through to passthrough). An exchange
// it stops on and cannot record (a request or response that does not
// decode, a mock that cannot be built) is reported first, as every parser
// reports the exchange it stops on (Session.ReportStoppedOn). A request the
// server's stream ends without answering is not: with no response there is
// no mock to lose.
func (h *HTTP) recordV2(ctx context.Context, sess *supervisor.Session) error {
	if sess == nil {
		return errors.New("recordV2: nil supervisor session")
	}
	defer sess.EndExchanges()
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
		firstChunk, err := nextRequestV2(sess)
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
			sess.ReportStoppedOn(models.HTTP, reqTs, "http decode error: request read failed: "+err.Error())
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
				// The stream ended with nothing of a response, so there is
				// no mock to lose and nothing is reported. The server
				// closed without answering: the keep-alive idle-close race,
				// in which the app's HTTP client retries the request on a
				// new connection, where it is recorded. Or, in an
				// observe-only capture, the client gave up and closed. Or
				// the session closed the stream: the supervisor's abort,
				// whose fallthrough leaves out the rest of the connection,
				// or the recording's stop.
				//
				// A mark on the incomplete-mock flag does not make it a loss
				// either. Response bytes the capture lost stopped this
				// connection's capture, as this parser cannot re-align
				// after a hole, and the capture leaves out the test cases
				// the connection carries from the loss on itself (the
				// relay's OnCaptureDesync). A write the relay could not
				// make (write_error) is a request the server never got:
				// the same race, when the app writes its request in more
				// than one piece.
				logger.Debug("V2 HTTP record: dest stream ended before response", zap.Error(err))
				return nil
			}
			if ctx.Err() != nil {
				return ctx.Err()
			}
			sess.ReportStoppedOn(models.HTTP, reqTs, "http decode error: initial response read failed: "+err.Error())
			utils.LogError(logger, err, "V2 HTTP record: initial response read failed")
			return err
		}
		finalResp := append([]byte(nil), firstRespChunk.Bytes...)
		resTs := firstRespChunk.WrittenAt

		gotLastWritten, err := h.readResponseV2(ctx, sess.DestStream, &finalResp, requestMethod(finalReq))
		// The stream ended before readResponseV2 found the response's end.
		// Where keploy relays the connection it has read all the server sent,
		// so that is the server's doing. An observe-only capture carries only
		// what the client read, so when its stream ended with the connection
		// (EndedWithConnection) it is the client that stopped reading and
		// closed: the response is recorded as the client read it
		// (FinalHTTP.RespReadInPart).
		streamEnded := false
		if err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, fakeconn.ErrClosed) {
				// Legacy encodeHTTP treats EOF after some bytes as
				// end-of-response. Respect that shape: emit what we have.
				// Only a stream that ran out (io.EOF) ended where its capture
				// did: ErrClosed is its reader being closed, possibly with
				// chunks still on their way.
				streamEnded = errors.Is(err, io.EOF)
				if !gotLastWritten.IsZero() {
					resTs = gotLastWritten
				}
			} else {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				sess.ReportStoppedOn(models.HTTP, reqTs, "http decode error: response read failed: "+err.Error())
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
			RespReadInPart:   streamEnded && sess.Opts.SkipTLSMITM && sess.EndedWithConnection(fakeconn.FromDest),
		}, destPort, sess.ClientConnID, sess.Opts)
		if err != nil {
			sess.ReportStoppedOn(models.HTTP, reqTs, "http decode error: "+err.Error())
			utils.LogError(logger, err, "V2 HTTP record: failed to build mock")
			return err
		}
		if mock != nil {
			// EmitMock takes the incomplete-mock flag itself. Clearing it
			// again here would wipe a mark set while EmitMock delivered
			// (AddMock can block for its send budget): that mark is the
			// next exchange's, whose mock would then be recorded with
			// nothing reporting it.
			if emitErr := sess.EmitMock(mock); emitErr != nil {
				return emitErr
			}
		} else {
			// Nothing recorded for this exchange (a passthrough), so a mark
			// set during it must not leave out the next exchange's mock.
			sess.MarkMockComplete()
		}
	}
}

// nextRequestV2 starts the next exchange (Session.NextRequest) and takes its
// request's first chunk. A chunk with no request bytes, nothing but empty
// lines (emptyLinesBefore), is no part of a request, so it is taken first, and
// the request starts at the client's next chunk: its response is what the
// server sent after that one was captured, and it is the connection's first
// request when the capture began after the CRLF of a request it does not have.
// The request's first chunk therefore always holds request bytes.
func nextRequestV2(sess *supervisor.Session) (fakeconn.Chunk, error) {
	for {
		c, err := sess.ClientStream.Peek()
		if err != nil {
			return fakeconn.Chunk{}, err
		}
		if emptyLinesBefore(c.Bytes) < len(c.Bytes) {
			break
		}
		if _, err := sess.ClientStream.ReadChunk(); err != nil {
			return fakeconn.Chunk{}, err
		}
	}
	if _, err := sess.NextRequest(models.HTTP, false); err != nil {
		return fakeconn.Chunk{}, err
	}
	return sess.ClientStream.ReadChunk()
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
	// 1. Complete headers: complete when messageHead finds the header section,
	// not at the first "\r\n\r\n", which can be empty lines in front of the
	// request line. Those are dropped (dropEmptyLinesBeforeRequest).
	head, bodyStart, _, ok := messageHead(*finalReq, false)
	for ; !ok; head, bodyStart, _, ok = messageHead(*finalReq, false) {
		if err := ctx.Err(); err != nil {
			return err
		}
		chunk, err := stream.ReadChunk()
		if err != nil {
			return err
		}
		*finalReq = append(*finalReq, chunk.Bytes...)
	}
	bodyStart -= dropEmptyLinesBeforeRequest(finalReq)

	contentLengthHeader, transferEncodingHeader := parseHeaders(head)

	if contentLengthHeader != "" {
		contentLength, err := strconv.Atoi(contentLengthHeader)
		if err != nil {
			return fmt.Errorf("invalid content-length: %w", err)
		}
		remaining := contentLength - (len(*finalReq) - bodyStart)
		for remaining > 0 {
			if err := ctx.Err(); err != nil {
				return err
			}
			chunk, err := stream.ReadChunk()
			if err != nil {
				return err
			}
			*finalReq = append(*finalReq, chunk.Bytes...)
			remaining -= len(chunk.Bytes)
		}
		return nil
	}

	if transferEncodingHeader != "" &&
		strings.Contains(strings.ToLower(transferEncodingHeader), "chunked") {
		// Frame the chunks (see chunkedBody): a suffix test on the buffer ended
		// a body whose data ends like "0\r\n\r\n" at a chunk boundary, and
		// never ended one that carries trailers.
		var body chunkedBody
		for {
			done, err := body.complete(*finalReq)
			if err != nil {
				return err
			}
			if done {
				return nil
			}
			if err := ctx.Err(); err != nil {
				return err
			}
			chunk, err := stream.ReadChunk()
			if err != nil {
				return err
			}
			*finalReq = append(*finalReq, chunk.Bytes...)
		}
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
//   - If Transfer-Encoding chunked, read until the chunked framing ends
//     (last-chunk, trailer section, final CRLF).
//
// Returns (lastWrittenAt, err). err may be io.EOF for the legitimate
// "server closed after sending a full response" case; the caller
// decides whether to emit a mock.
func (h *HTTP) readResponseV2(ctx context.Context, stream *fakeconn.FakeConn, finalResp *[]byte, reqMethod string) (time.Time, error) {
	var lastWr time.Time

	// 1. Complete headers: the final response's, past any interim 1xx.
	for {
		if _, _, _, ok := messageHead(*finalResp, true); ok {
			break
		}
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
		*finalResp = append(*finalResp, chunk.Bytes...)
	}

	// 2. Parse the final response's headers for body framing. The answer to
	// a HEAD, a 204 and a 304 have no body whatever the headers say.
	head, bodyStart, status, _ := messageHead(*finalResp, true)
	if responseHasNoBody(reqMethod, status) {
		return lastWr, nil
	}
	contentLengthHeader, transferEncodingHeader := parseHeaders(head)

	if contentLengthHeader != "" {
		contentLength, err := strconv.Atoi(contentLengthHeader)
		if err != nil {
			return lastWr, fmt.Errorf("invalid content-length: %w", err)
		}
		remaining := contentLength - (len(*finalResp) - bodyStart)
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
			*finalResp = append(*finalResp, chunk.Bytes...)
			remaining -= len(chunk.Bytes)
		}
		return lastWr, nil
	}

	if transferEncodingHeader != "" &&
		strings.Contains(strings.ToLower(transferEncodingHeader), "chunked") {
		// Chunked: read until the body's chunked framing ends (see
		// chunkedBody), not until finalResp ends in "0\r\n\r\n".
		body := chunkedBody{response: true}
		for {
			done, err := body.complete(*finalResp)
			if err != nil {
				return lastWr, err
			}
			if done {
				return lastWr, nil
			}
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
			*finalResp = append(*finalResp, chunk.Bytes...)
		}
	}

	// Neither Content-Length nor chunked: read until EOF (RFC 7230
	// permits this for HTTP/1.0 and for responses with "Connection:
	// close"). The loop relies on the stream eventually closing.
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
		*finalResp = append(*finalResp, chunk.Bytes...)
	}
}

// parseHeaders extracts the body-framing Content-Length and
// Transfer-Encoding header values (if any) from an HTTP message whose
// headers end with CRLFCRLF. The parse is the same loose style the chunk.go
// helpers use: split on '\n', trim '\r', skip malformed lines.
//
// A chunked Transfer-Encoding overrides a Content-Length sent with it (RFC
// 9112 §6.3), so contentLength is "" then: framing such a message by its
// Content-Length would end it where its peer does not.
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
	if strings.Contains(strings.ToLower(transferEncoding), "chunked") {
		contentLength = ""
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

// parseFinalResponse parses the response m records: the final one, past any
// interim 1xx responses in front of it (net/http does not skip them), which are
// read off the same reader one after another, so their line endings do not
// matter. One net/http will not read that way (an interim with a malformed
// header line) is framed by its blank lines instead (finalResponse), as it was
// before the interims were read. A response its client read only in part
// (m.RespReadInPart) that does not parse whole is parsed as the client had it
// (responseAsRead, from where the final response begins), and cut reports so.
// What parses as it stands is never read that way: a bare-LF header section,
// which responseAsRead cannot frame, still parses whole.
//
// An interim response is never recorded as the answer: one whose final
// response never arrived (the stream ended after it, in its head, or in the
// final one's status line) is an error wrapping io.ErrUnexpectedEOF, as a
// response cut short is.
func parseFinalResponse(m *FinalHTTP, req *http.Request) (resp *http.Response, cut bool, err error) {
	rd := bytes.NewReader(m.Resp)
	br := bufio.NewReader(rd)
	interim, final := 0, 0 // final: where the response after the interims begins
	for {
		resp, err = http.ReadResponse(br, req)
		if err != nil || !pUtil.IsInterimStatus(resp.StatusCode) {
			break
		}
		interim = resp.StatusCode // no body: the next response follows it
		final = len(m.Resp) - rd.Len() - br.Buffered()
	}
	if err == nil {
		return resp, false, nil
	}
	if framed, ferr := http.ReadResponse(bufio.NewReader(bytes.NewReader(finalResponse(m.Resp))), req); ferr == nil && !pUtil.IsInterimStatus(framed.StatusCode) {
		return framed, false, nil
	}
	if m.RespReadInPart {
		asRead, rerr := http.ReadResponse(bufio.NewReader(bytes.NewReader(responseAsRead(m.Resp[final:]))), req)
		if rerr == nil && !pUtil.IsInterimStatus(asRead.StatusCode) {
			return asRead, true, nil
		}
	}
	if interim != 0 && (errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF)) {
		return nil, false, fmt.Errorf("parse response: the final response after a %d never arrived whole: %w",
			interim, io.ErrUnexpectedEOF)
	}
	return nil, false, fmt.Errorf("parse response: %w", err)
}

// decompressReadInPart is pkg.Decompress for a body its client read only in
// part: what decodes before the cut, which is as much as the client could
// decode. A body cut before any of it decodes (in the gzip header, or none of
// it read) decodes to nothing. The cut is expected here, so it is not logged as
// a failure. An encoding it does not decode is kept as it is, as Decompress
// keeps it.
func decompressReadInPart(encoding string, data []byte, limit int64) ([]byte, error) {
	var r io.Reader
	switch strings.ToLower(strings.TrimSpace(encoding)) {
	case "gzip":
		zr, err := gzip.NewReader(bytes.NewReader(data))
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			return []byte{}, nil // cut in its header
		}
		if err != nil {
			return nil, err
		}
		defer zr.Close()
		r = zr
	case "br":
		r = brotli.NewReader(bytes.NewReader(data))
	case "", "identity":
		return data, nil
	default:
		return data, nil
	}
	out, err := io.ReadAll(io.LimitReader(r, limit+1))
	if int64(len(out)) > limit {
		return nil, fmt.Errorf("%w of %d bytes (possible decompression bomb)", pkg.ErrDecompressedTooLarge, limit)
	}
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) {
		return nil, err
	}
	return out, nil
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

	respParsed, cut, err := parseFinalResponse(m, req)
	if err != nil {
		return nil, err
	}

	var respBody []byte
	if respParsed.Body != nil {
		respBody, err = io.ReadAll(respParsed.Body)
		// A body its client read in part is recorded as far as it read it:
		// ReadAll returns what it read before the cut. Replay serves it with
		// its own length (the Content-Length set below).
		if err != nil {
			if !m.RespReadInPart || !errors.Is(err, io.ErrUnexpectedEOF) {
				return nil, fmt.Errorf("read response body: %w", err)
			}
			cut = true
		}
		if enc := respParsed.Header.Get("Content-Encoding"); enc != "" {
			// Read in part, a body with no length of its own (close-delimited)
			// cannot be known to be whole either: it decodes as far as it goes.
			// A whole one decodes the same either way.
			//
			// Not a response with no body by rule (to a HEAD, a 204, a 304):
			// its encoding describes a body it does not carry, and it is
			// recorded, or not, as a whole one is.
			if (cut || m.RespReadInPart) && !responseHasNoBody(req.Method, respParsed.StatusCode) {
				respBody, err = decompressReadInPart(enc, respBody, pkg.MaxDecompressedSize)
			} else {
				respBody, err = pkg.Decompress(h.Logger, enc, respBody, pkg.MaxDecompressedSize)
			}
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

				// Replay serves this response: keep what it takes to serve a
				// repeated header (Set-Cookie) on its own lines.
				HeaderLineLengths: pkg.ToYamlHTTPHeaderLineLengths(respParsed.Header),
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
