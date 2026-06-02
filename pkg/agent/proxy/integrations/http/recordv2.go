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

		gotLastWritten, err := h.readResponseV2(ctx, sess.DestStream, &finalResp)
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
		for !bytes.HasSuffix(*finalReq, chunkedTerminator) {
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
		return nil
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
//   - If Transfer-Encoding chunked, read until the last-chunk marker
//     "0\r\n\r\n" appears as a suffix (the fix in SAP bug #4110).
//
// Returns (lastWrittenAt, err). err may be io.EOF for the legitimate
// "server closed after sending a full response" case; the caller
// decides whether to emit a mock.
func (h *HTTP) readResponseV2(ctx context.Context, stream *fakeconn.FakeConn, finalResp *[]byte) (time.Time, error) {
	var lastWr time.Time

	// 1. Complete headers.
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

	// 2. Parse headers for body framing.
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
		// Chunked: read until we see the last-chunk terminator as a
		// suffix of finalResp. pUtil terminator detection (see chunk.go
		// chunkedTerminator) uses HasSuffix so a body chunk that shares
		// a TLS record with the terminator still exits cleanly.
		for !bytes.HasSuffix(*finalResp, chunkedTerminator) {
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
		return lastWr, nil
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
		if len(chunk.Bytes) == 0 {
			return lastWr, io.EOF
		}
		*finalResp = append(*finalResp, chunk.Bytes...)
	}
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
			reqBody, err = pkg.Decompress(h.Logger, req.Header.Get("Content-Encoding"), reqBody)
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
			respBody, err = pkg.Decompress(h.Logger, respParsed.Header.Get("Content-Encoding"), respBody)
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
	}
	return mock, nil
}
