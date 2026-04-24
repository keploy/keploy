package http

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"strconv"
	"time"

	"go.keploy.io/server/v3/pkg"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	syncMock "go.keploy.io/server/v3/pkg/agent/proxy/syncMock"
	"go.keploy.io/server/v3/pkg/agent/proxy/util"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
)

// httpMethodPrefixes are pre-computed to avoid per-call []byte allocations.
var (
	httpResponsePrefix = []byte("HTTP/")
	httpMethodGET      = []byte("GET ")
	httpMethodPOST     = []byte("POST ")
	httpMethodPUT      = []byte("PUT ")
	httpMethodPATCH    = []byte("PATCH ")
	httpMethodDELETE   = []byte("DELETE ")
	httpMethodOPTIONS  = []byte("OPTIONS ")
	httpMethodHEAD     = []byte("HEAD ")
	httpMethodCONNECT  = []byte("CONNECT ")
	httpVersionMarker  = []byte(" HTTP/")
)

// maxRequestLineScan caps how far into the first line we scan for " HTTP/".
const maxRequestLineScan = 8192

func init() {
	integrations.Register(integrations.HTTP, &integrations.Parsers{
		Initializer: New, Priority: 100,
	})
}

type HTTP struct {
	Logger *zap.Logger
	//opts  globalOptions //other global options set by the proxy
}

func New(logger *zap.Logger) integrations.Integrations {
	return &HTTP{
		Logger: logger,
	}
}

type FinalHTTP struct {
	Req              []byte
	Resp             []byte
	ReqTimestampMock time.Time
	ResTimestampMock time.Time
}

// MatchType determines if the outgoing network call is HTTP by checking for
// a well-formed HTTP request line (METHOD path HTTP/version) or a response
// status prefix (HTTP/version). For requests, it verifies " HTTP/" appears in
// the first line to prevent false positives from binary protocols that start
// with method-like ASCII bytes. Response detection only checks the prefix.
func (h *HTTP) MatchType(_ context.Context, buf []byte) bool {
	isResponse := bytes.HasPrefix(buf, httpResponsePrefix)
	isRequest := bytes.HasPrefix(buf, httpMethodGET) ||
		bytes.HasPrefix(buf, httpMethodPOST) ||
		bytes.HasPrefix(buf, httpMethodPUT) ||
		bytes.HasPrefix(buf, httpMethodPATCH) ||
		bytes.HasPrefix(buf, httpMethodDELETE) ||
		bytes.HasPrefix(buf, httpMethodOPTIONS) ||
		bytes.HasPrefix(buf, httpMethodHEAD) ||
		bytes.HasPrefix(buf, httpMethodCONNECT)

	if !isRequest && !isResponse {
		h.Logger.Debug("determined the protocol is not HTTP", zap.Bool("isHTTP", false))
		return false
	}

	// For requests, verify the first line contains " HTTP/" to confirm it's a
	// valid HTTP request line and not a binary protocol that coincidentally
	// starts with method-like ASCII bytes.
	if isRequest {
		// Cap the search range first to bound the scan cost on large non-HTTP payloads.
		scanBuf := buf
		maxScan := maxRequestLineScan + len(httpVersionMarker)
		if len(scanBuf) > maxScan {
			scanBuf = scanBuf[:maxScan]
		}
		end := bytes.IndexByte(scanBuf, '\n')
		if end == -1 {
			end = len(scanBuf)
		}
		if !bytes.Contains(scanBuf[:end], httpVersionMarker) {
			h.Logger.Debug("HTTP method prefix found but no HTTP version in request line", zap.Bool("isHTTP", false))
			return false
		}
	}

	h.Logger.Debug("determined whether the protocol is HTTP", zap.Bool("isHTTP", true))
	return true
}

// IsV2 declares that the HTTP parser is migrated to the supervisor + relay
// + FakeConn architecture (pkg/agent/proxy/proxy_v2.go). The dispatcher
// type-asserts the matchedParser against integrations.IntegrationsV2 and
// routes to recordViaSupervisor only when IsV2() returns true.
//
// Returning true is unconditional — the HTTP parser has no per-instance
// configuration that could force it back to the legacy path. The rollback
// knob is the env KEPLOY_NEW_RELAY=off handled in proxy_v2.go.
func (h *HTTP) IsV2() bool { return true }

// RecordOutgoing dispatches to the V2 path when the supervisor has
// attached a session via RecordSession.V2, and to the legacy path
// otherwise. Keeping both paths live lets the dispatcher / rollback
// knob (KEPLOY_NEW_RELAY) swap between them without code changes.
func (h *HTTP) RecordOutgoing(ctx context.Context, session *integrations.RecordSession) error {
	if session != nil && session.V2 != nil {
		return h.recordV2(ctx, session.V2)
	}
	return h.recordLegacy(ctx, session)
}

// recordLegacy is the original RecordOutgoing body preserved unchanged.
// It consumes the legacy RecordSession fields (Ingress / Egress / Mocks)
// and forwards bytes between the real sockets itself. The V2 path in
// recordV2 relies on the supervisor's relay to do the forwarding and
// only observes teed chunks via FakeConn streams.
func (h *HTTP) recordLegacy(ctx context.Context, session *integrations.RecordSession) error {
	logger := session.Logger

	h.Logger.Debug("Recording the outgoing http call in record mode")

	reqBuf, err := util.ReadInitialBuf(ctx, logger, session.Ingress)
	if err != nil {
		utils.LogError(logger, err, "failed to read the initial http message")
		return err
	}
	err = h.encodeHTTP(ctx, reqBuf, session.Ingress, session.Egress, session.Mocks, session.Opts, session.OnMockRecorded)
	if err != nil {
		utils.LogError(logger, err, "failed to encode the http message into the yaml")
		return err
	}
	return nil
}

func (h *HTTP) MockOutgoing(ctx context.Context, src net.Conn, dstCfg *models.ConditionalDstCfg, mockDb integrations.MockMemDb, opts models.OutgoingOptions) error {
	// h.Logger = h.Logger.With(zap.Any("Client ConnectionID", ctx.Value(models.ClientConnectionIDKey).(string)), zap.Any("Destination ConnectionID", ctx.Value(models.DestConnectionIDKey).(string)), zap.Any("Client IP Address", src.RemoteAddr().String()))
	h.Logger.Debug("Mocking the outgoing http call in test mode")

	reqBuf, err := util.ReadInitialBuf(ctx, h.Logger, src)
	if err != nil {
		utils.LogError(h.Logger, err, "failed to read the initial http message")
		return err
	}

	err = h.decodeHTTP(ctx, reqBuf, src, dstCfg, mockDb, opts)
	if err != nil {
		if errors.Is(err, ErrMockNotMatched) {
			h.Logger.Debug("mock miss — 502 already sent to client", zap.Error(err))
			return err
		}
		utils.LogError(h.Logger, err, "failed to decode the http message from the yaml")
		return err
	}
	return nil
}

// ParseFinalHTTP is used to parse the final http request and response and save it in a yaml file
func (h *HTTP) parseFinalHTTP(ctx context.Context, mock *FinalHTTP, destPort uint, mocks chan<- *models.Mock, opts models.OutgoingOptions, onMockRecorded integrations.PostRecordHook) error {
	var req *http.Request
	// converts the request message buffer to http request
	req, err := http.ReadRequest(bufio.NewReader(bytes.NewReader(mock.Req)))
	if err != nil {
		utils.LogError(h.Logger, err, "failed to parse the http request message")
		return err
	}

	// Set the host header explicitly because the `http.ReadRequest`` trim the host header
	// func ReadRequest(b *bufio.Reader) (*Request, error) {
	// 	req, err := readRequest(b)
	// 	if err != nil {
	// 		return nil, err
	// 	}

	// 	delete(req.Header, "Host")
	// 	return req, err
	// }
	req.Header.Set("Host", req.Host)

	var reqBody []byte
	if req.Body != nil { // Read
		var err error
		reqBody, err = io.ReadAll(req.Body)
		if err != nil {
			// TODO right way to log errors
			utils.LogError(h.Logger, err, "failed to read the http request body", zap.Any("metadata", utils.GetReqMeta(req)))
			return err
		}

		if req.Header.Get("Content-Encoding") != "" {
			reqBody, err = pkg.Decompress(h.Logger, req.Header.Get("Content-Encoding"), reqBody)
			if err != nil {
				utils.LogError(h.Logger, err, "failed to decode the http request body", zap.Any("metadata", utils.GetReqMeta(req)))
				return err
			}
		}
	}

	// converts the response message buffer to http response
	respParsed, err := http.ReadResponse(bufio.NewReader(bytes.NewReader(mock.Resp)), req)
	if err != nil {
		utils.LogError(h.Logger, err, "failed to parse the http response message", zap.Any("metadata", utils.GetReqMeta(req)))
		return err
	}

	//Add the content length to the headers.
	var respBody []byte
	//Checking if the body of the response is empty or does not exist.
	if respParsed.Body != nil { // Read
		respBody, err = io.ReadAll(respParsed.Body)
		if err != nil {
			utils.LogError(h.Logger, err, "failed to read the the http response body", zap.Any("metadata", utils.GetReqMeta(req)))
			return err
		}

		if respParsed.Header.Get("Content-Encoding") != "" {
			respBody, err = pkg.Decompress(h.Logger, respParsed.Header.Get("Content-Encoding"), respBody)
			if err != nil {
				utils.LogError(h.Logger, err, "failed to decode the http response body", zap.Any("metadata", utils.GetReqMeta(req)))
				return err
			}
		}

		h.Logger.Debug("This is the response body: " + string(respBody))
		//Set the content length to the headers.
		respParsed.Header.Set("Content-Length", strconv.Itoa(len(respBody)))
	}

	// store the request and responses as mocks
	meta := map[string]string{
		"name":      "Http",
		"type":      models.HTTPClient,
		"operation": req.Method,
		"connID":    ctx.Value(models.ClientConnectionIDKey).(string),
	}

	// Check if the request is a passThrough request
	if utils.IsPassThrough(h.Logger, req, destPort, opts) {
		h.Logger.Debug("The request is a passThrough request", zap.Any("metadata", utils.GetReqMeta(req)))
		return nil
	}

	newMock := &models.Mock{
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
			Created: time.Now().Unix(),

			ReqTimestampMock: mock.ReqTimestampMock,
			ResTimestampMock: mock.ResTimestampMock,
		},
		// HTTP is a request-response protocol — every exchange is per-test
		// by construction. No handshake/session tier exists at the HTTP
		// layer; outbound calls from an application under test are always
		// bound to the current test window. Stamp LifetimePerTest + mark
		// LifetimeDerived so DeriveLifetime's ingest-time classifier is a
		// no-op (the recorder is authoritative at emit time). Leaves
		// Metadata["type"] unchanged (legacy readers still see HTTPClient).
		TestModeInfo: models.TestModeInfo{
			Lifetime:        models.LifetimePerTest,
			LifetimeDerived: true,
		},
	}

	if onMockRecorded != nil {
		onMockRecorded(newMock)
	}

	if mgr := syncMock.Get(); mgr != nil {
		// Route HTTP mocks through the sync manager. The manager uses its
		// internal first-request state to decide whether to buffer or forward
		// mocks for correct time-window based association.
		mgr.AddMock(newMock)
		return nil
	}

	// Fallback: syncMock manager unavailable, send to mocks channel directly.
	// Use select with ctx so we don't block forever during shutdown.
	select {
	case <-ctx.Done():
		select {
		case mocks <- newMock:
		default:
		}
	case mocks <- newMock:
	}
	return nil
}
