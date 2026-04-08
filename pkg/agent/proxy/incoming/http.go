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
		b.buf.Reset()
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

type teeReadCloser struct {
	io.Reader
	io.Closer
}

func wrapBodyForCapture(body io.ReadCloser, capture io.Writer) io.ReadCloser {
	if body == nil || capture == nil {
		return body
	}
	return &teeReadCloser{
		Reader: io.TeeReader(body, capture),
		Closer: body,
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
			logger.Debug("Acquired 1 of 5 sampling slots for capture")
		case <-ctx.Done():
			return
		default:
			// Non-blocking bypass: The 5 slots are FULL.
			// We do not block. We set acquiredLock to false.
			// This connection will be proxied normally and marked closed, but WILL NOT be captured.
			acquiredLock = false
			logger.Debug("Sampling limit reached (5/5). Ignoring request for capture.")
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
				logger.Debug("Lock released")
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
		logger.Warn("Failed to dial upstream app port", zap.String("Final_App_Port", finalAppAddr), zap.Error(err))
		return
	}
	defer upConn.Close()

	clientReader := bufio.NewReader(clientConn)
	upstreamReader := bufio.NewReader(upConn)

	// forceCloseMode is true if we are running in Sync or Sampling mode.
	// In these modes, we strictly disable Keep-Alive and drop the loop after one request.
	forceCloseMode := pm.synchronous || pm.sampling

	for {
		req, err := http.ReadRequest(clientReader)
		if err != nil {
			if errors.Is(err, io.EOF) {
				logger.Debug("Client closed the keep-alive connection.", zap.String("client", clientConn.RemoteAddr().String()))
			} else {
				logger.Warn("Failed to read client request", zap.Error(err))
			}
			return
		}
		reqTimestamp := time.Now()
		var chunked bool = false
		pressureCloseMode := forceCloseMode || isIngressRecordingPaused()
		captureEnabled := !isIngressRecordingPaused()
		var captureState *httpCaptureState
		var reqBodyCapture httpBodyCaptureBuffer
		var respBodyCapture httpBodyCaptureBuffer

		// Request modifications for Sync/Sampling modes
		if forceCloseMode {
			if req.ContentLength == -1 || isChunked(req.TransferEncoding) {
				logger.Debug("Detected chunked request. Releasing lock early.")
				releaseLock()
				chunked = true
			} else if captureEnabled && pm.synchronous && acquiredLock {
				mgr := syncMock.Get()
				if !mgr.GetFirstReqSeen() {
					mgr.SetFirstRequestSignaled()
				}
			}

			// Requirement: "mark the connection as closed for keep alive"
			// This applies to ALL requests in these modes, even chunked or ignored (6th+) requests.
			req.Close = true
			req.Header.Set("Connection", "close")
		}
		if pressureCloseMode && !forceCloseMode {
			releaseLock()
			req.Close = true
			req.Header.Set("Connection", "close")
			captureEnabled = false
		}

		var reqData []byte
		if captureEnabled {
			if isIngressRecordingPaused() {
				pressureCloseMode = true
				captureEnabled = false
				releaseLock()
				req.Close = true
				req.Header.Set("Connection", "close")
			} else {
				captureState = newHTTPCaptureState(hooksUtils.MaxTestCaseSize)
				reqBodyCapture.state = captureState
				req.Body = wrapBodyForCapture(req.Body, &reqBodyCapture)
			}
		}

		if err := req.Write(upConn); err != nil {
			if pressureCloseMode && isIngressExpectedCloseErr(err) {
				logger.Debug("HTTP/1 ingress request write ended during close-under-pressure path", zap.Error(err))
				req.Body.Close()
				return
			}
			if isIngressRecordingPaused() {
				logger.Debug("HTTP/1 ingress request write failed under memory pressure", zap.Error(err))
				req.Body.Close()
				return
			}
			logger.Error("Failed to forward request to upstream", zap.Error(err))
			req.Body.Close()
			return
		}
		req.Body.Close()

		if captureEnabled && captureState != nil {
			if captureState.isAborted() {
				captureEnabled = false
				reqBodyCapture.Reset()
			} else {
				reqData, err = serializeCapturedRequest(req, reqBodyCapture.Bytes())
				reqBodyCapture.Reset()
				if err != nil {
					captureEnabled = false
					logger.Warn("Failed to serialize forwarded request for capturing", zap.Error(err))
				}
			}
		}

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
			if isIngressRecordingPaused() {
				logger.Debug("HTTP/1 ingress upstream read failed under memory pressure", zap.Error(err))
				return
			}
			logger.Error("Failed to read upstream response", zap.Error(err))
			return
		}

		// Response modifications for Sync/Sampling modes
		if forceCloseMode {
			if resp.ContentLength == -1 || isChunked(resp.TransferEncoding) {
				logger.Debug("Detected chunked response. Releasing lock early.")
				releaseLock()
				chunked = true
			}

			// Close the connection on the response side as well
			resp.Close = true
			resp.Header.Set("Connection", "close")
		}
		if pressureCloseMode && !forceCloseMode {
			resp.Close = true
			resp.Header.Set("Connection", "close")
			captureEnabled = false
		}

		respTimestamp := time.Now()
		var respData []byte
		if captureEnabled {
			if isIngressRecordingPaused() {
				pressureCloseMode = true
				captureEnabled = false
				releaseLock()
				resp.Close = true
				resp.Header.Set("Connection", "close")
			} else {
				respBodyCapture.state = captureState
				resp.Body = wrapBodyForCapture(resp.Body, &respBodyCapture)
			}
		}

		if !pressureCloseMode && isIngressRecordingPaused() {
			pressureCloseMode = true
			captureEnabled = false
			releaseLock()
			resp.Close = true
			resp.Header.Set("Connection", "close")
		}

		if err := resp.Write(clientConn); err != nil {
			if pressureCloseMode && isIngressExpectedCloseErr(err) {
				logger.Debug("HTTP/1 ingress client connection closed while finishing close-under-pressure path", zap.Error(err))
				resp.Body.Close()
				return
			}
			if isIngressRecordingPaused() {
				logger.Debug("HTTP/1 ingress client write failed under memory pressure", zap.Error(err))
				resp.Body.Close()
				return
			}
			logger.Error("Failed to forward response to client", zap.Error(err))
			resp.Body.Close()
			return
		}
		resp.Body.Close()

		if captureEnabled && captureState != nil {
			if captureState.isAborted() {
				captureEnabled = false
				reqBodyCapture.Reset()
				respBodyCapture.Reset()
			} else {
				respData, err = serializeCapturedResponse(resp, respBodyCapture.Bytes())
				respBodyCapture.Reset()
				if err != nil {
					captureEnabled = false
					logger.Warn("Failed to serialize forwarded response for capturing", zap.Error(err))
				}
			}
		}

		if !pressureCloseMode && isIngressRecordingPaused() {
			pressureCloseMode = true
			captureEnabled = false
			releaseLock()
		}

		// Capture Evaluation
		shouldCapture := captureEnabled
		if forceCloseMode {
			if chunked {
				shouldCapture = false
			} else if pm.sampling && !acquiredLock {
				shouldCapture = false
			}
		}

		// Only parse and invoke the hook if it's eligible for capture
		if shouldCapture && !isIngressRecordingPaused() {
			parsedHTTPReq, err := pkg.ParseHTTPRequest(reqData)
			if err == nil {
				parsedHTTPRes, err := pkg.ParseHTTPResponse(respData, parsedHTTPReq)
				if err == nil {
					go func() {
						defer parsedHTTPReq.Body.Close()
						defer parsedHTTPRes.Body.Close()
						hooksUtils.CaptureHook(ctx, logger, t, parsedHTTPReq, parsedHTTPRes, reqTimestamp, respTimestamp, pm.incomingOpts, pm.synchronous, actualPort)
					}()
				}
			}
		}

		// Break the keep-alive loop and exit if we are in sync/sampling mode
		if pressureCloseMode {
			return
		}
	}
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
