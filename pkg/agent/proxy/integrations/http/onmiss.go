package http

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"io"
	"net"
	"net/http"
	"sync"
	"time"

	"go.keploy.io/server/v3/pkg"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// On-miss policy for `keploy mock replay` (models.OutgoingOptions.OnMiss):
//   - fail (default): the miss is a hard 502; nothing reaches the real upstream.
//   - passthrough: dial the real dependency, relay the exchange, don't persist.
//   - record: passthrough AND capture the exchange so the CLI appends it to the
//     mock set at end-of-run (VCR "new_episodes" — incremental refresh).
//
// The capture buffer is a process-global drained once per run by the CLI via
// GET /agent/mock/captured, so record-on-miss needs no live streaming and no
// change to the proxy's mock-serving session mode.

var (
	capturedMu    sync.Mutex
	capturedMocks []*models.Mock
)

// ResetCaptured clears the on-miss capture buffer at the start of a mock-serving
// session so a reused agent process does not leak captures across runs.
func ResetCaptured() {
	capturedMu.Lock()
	capturedMocks = nil
	capturedMu.Unlock()
}

// DrainCaptured returns and clears the mocks captured on miss this run. The CLI
// appends them to the active set.
func DrainCaptured() []*models.Mock {
	capturedMu.Lock()
	out := capturedMocks
	capturedMocks = nil
	capturedMu.Unlock()
	return out
}

func appendCaptured(m *models.Mock) {
	capturedMu.Lock()
	capturedMocks = append(capturedMocks, m)
	capturedMu.Unlock()
}

// serveOnMiss handles an HTTP mock miss under a passthrough/record policy: it
// dials the real upstream, relays the request and response, writes the response
// back to the client, and (for record) captures a mock. It returns true when it
// handled the exchange (the caller then continues the keep-alive loop), or false
// when the policy is fail (the caller sends the 502). An error means the
// upstream dial/relay failed and the caller should fall back to the 502.
func (h *HTTP) serveOnMiss(ctx context.Context, clientConn net.Conn, reqBuf []byte, request *http.Request, reqBody []byte, dstCfg *models.ConditionalDstCfg, opts models.OutgoingOptions) (bool, error) {
	if !opts.OnMiss.PassesThroughOnMiss() {
		return false, nil
	}
	if dstCfg == nil || dstCfg.Addr == "" {
		h.Logger.Debug("on-miss passthrough: no upstream address known; falling back to hard miss")
		return false, nil
	}

	reqTs := time.Now()
	dstConn, err := h.dialUpstream(ctx, dstCfg)
	if err != nil {
		return false, err
	}
	defer func() {
		if cerr := dstConn.Close(); cerr != nil {
			h.Logger.Debug("on-miss passthrough: failed to close upstream conn", zap.Error(cerr))
		}
	}()

	// Forward the exact request bytes upstream.
	if _, err := dstConn.Write(reqBuf); err != nil {
		return false, err
	}

	// Read the upstream response.
	respReader := bufio.NewReader(dstConn)
	respParsed, err := http.ReadResponse(respReader, request)
	if err != nil {
		return false, err
	}
	respBody, err := io.ReadAll(respParsed.Body)
	_ = respParsed.Body.Close()
	if err != nil {
		return false, err
	}
	resTs := time.Now()

	// Relay the response to the client verbatim (re-serialize with a correct
	// Content-Length; the app reads a normal HTTP response).
	respParsed.Body = io.NopCloser(bytes.NewReader(respBody))
	respParsed.ContentLength = int64(len(respBody))
	var outBuf bytes.Buffer
	if err := respParsed.Write(&outBuf); err != nil {
		return false, err
	}
	if _, err := clientConn.Write(outBuf.Bytes()); err != nil {
		return false, err
	}

	h.Logger.Info("on-miss: served an unrecorded call from the real dependency",
		zap.String("method", request.Method),
		zap.String("url", request.URL.String()),
		zap.String("policy", string(opts.OnMiss)))

	// record policy: capture the exchange for the CLI to persist.
	if opts.OnMiss.RecordsOnMiss() {
		appendCaptured(&models.Mock{
			Version: models.GetVersion(),
			Name:    "mocks",
			Kind:    models.HTTP,
			Spec: models.MockSpec{
				Metadata: map[string]string{"type": "HTTP_CLIENT", "operation": request.Method},
				HTTPReq: &models.HTTPReq{
					Method:     models.Method(request.Method),
					ProtoMajor: request.ProtoMajor,
					ProtoMinor: request.ProtoMinor,
					URL:        request.URL.String(),
					Header:     pkg.ToYamlHTTPHeader(request.Header),
					Body:       string(reqBody),
					URLParams:  pkg.URLParams(request),
				},
				HTTPResp: &models.HTTPResp{
					StatusCode: respParsed.StatusCode,
					Header:     pkg.ToYamlHTTPHeader(respParsed.Header),
					Body:       string(respBody),
				},
				Created:          time.Now().Unix(),
				ReqTimestampMock: reqTs,
				ResTimestampMock: resTs,
			},
		})
	}
	return true, nil
}

// dialUpstream opens a connection to the real dependency, wrapping it in TLS
// when the original client connection was TLS (dstCfg.TLSCfg set). It mirrors
// keploy's record-time posture of not being stricter than the app it stands in
// for, so the upstream cert is not verified here.
func (h *HTTP) dialUpstream(ctx context.Context, dstCfg *models.ConditionalDstCfg) (net.Conn, error) {
	d := &net.Dialer{Timeout: 30 * time.Second}
	raw, err := d.DialContext(ctx, "tcp", dstCfg.Addr)
	if err != nil {
		return nil, err
	}
	if dstCfg.TLSCfg == nil {
		return raw, nil
	}
	cfg := dstCfg.TLSCfg.Clone()
	cfg.InsecureSkipVerify = true
	tlsConn := tls.Client(raw, cfg)
	if err := tlsConn.HandshakeContext(ctx); err != nil {
		_ = raw.Close()
		return nil, err
	}
	return tlsConn, nil
}
