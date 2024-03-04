package http

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"strconv"
	"time"

	"go.keploy.io/server/v2/pkg/core/proxy/integrations"
	"go.keploy.io/server/v2/pkg/core/proxy/util"

	"go.keploy.io/server/v2/pkg"
	"go.keploy.io/server/v2/pkg/models"
	"go.uber.org/zap"
)

func init() {
	integrations.Register("http", NewHttp)
}

type Http struct {
	logger *zap.Logger
	//opts  globalOptions //other global options set by the proxy
}

func NewHttp(logger *zap.Logger) integrations.Integrations {
	return &Http{
		logger: logger,
	}
}

type finalHttp struct {
	req              []byte
	resp             []byte
	reqTimestampMock time.Time
	resTimestampMock time.Time
}

// MatchType function determines if the outgoing network call is HTTP by comparing the
// message format with that of an HTTP text message.
func (h *Http) MatchType(ctx context.Context, buf []byte) bool {
	return bytes.HasPrefix(buf[:], []byte("HTTP/")) ||
		bytes.HasPrefix(buf[:], []byte("GET ")) ||
		bytes.HasPrefix(buf[:], []byte("POST ")) ||
		bytes.HasPrefix(buf[:], []byte("PUT ")) ||
		bytes.HasPrefix(buf[:], []byte("PATCH ")) ||
		bytes.HasPrefix(buf[:], []byte("DELETE ")) ||
		bytes.HasPrefix(buf[:], []byte("OPTIONS ")) ||
		bytes.HasPrefix(buf[:], []byte("HEAD "))
}

func (h *Http) RecordOutgoing(ctx context.Context, src net.Conn, dst net.Conn, mocks chan<- *models.Mock, opts models.OutgoingOptions) error {
	logger := h.logger.With(zap.Any("Client IP Address", src.RemoteAddr().String()), zap.Any("Client ConnectionID", util.GetNextID()), zap.Any("Destination ConnectionID", util.GetNextID()))

	reqBuf, err := util.ReadInitialBuf(ctx, logger, src)
	if err != nil {
		logger.Error("failed to read the initial http message", zap.Error(err))
		return errors.New("failed to record the outgoing http call")
	}

	err = encodeHttp(ctx, logger, reqBuf, src, dst, mocks, opts)
	if err != nil {
		logger.Error("failed to encode the http message into the yaml", zap.Error(err))
		return errors.New("failed to record the outgoing http call")
	}
	return nil
}

func (h *Http) MockOutgoing(ctx context.Context, src net.Conn, dstCfg *integrations.ConditionalDstCfg, mockDb integrations.MockMemDb, opts models.OutgoingOptions) error {
	logger := h.logger.With(zap.Any("Client IP Address", src.RemoteAddr().String()), zap.Any("Client ConnectionID", util.GetNextID()), zap.Any("Destination ConnectionID", util.GetNextID()))

	reqBuf, err := util.ReadInitialBuf(ctx, logger, src)
	if err != nil {
		logger.Error("failed to read the initial http message", zap.Error(err))
		return errors.New("failed to mock the outgoing http call")
	}

	err = decodeHttp(ctx, logger, reqBuf, src, dstCfg, mockDb, opts)
	if err != nil {
		logger.Error("failed to decode the http message from the yaml", zap.Error(err))
		return errors.New("failed to mock the outgoing http call")
	}
	return nil
}

// ParseFinalHttp is used to parse the final http request and response and save it in a yaml file
func ParseFinalHttp(ctx context.Context, logger *zap.Logger, mock *finalHttp, destPort uint, mocks chan<- *models.Mock, opts models.OutgoingOptions) error {
	var req *http.Request
	// converts the request message buffer to http request
	req, err := http.ReadRequest(bufio.NewReader(bytes.NewReader(mock.req)))
	if err != nil {
		logger.Error("failed to parse the http request message", zap.Error(err))
		return err
	}

	var reqBody []byte
	if req.Body != nil { // Read
		var err error
		reqBody, err = io.ReadAll(req.Body)
		if err != nil {
			// TODO right way to log errors
			logger.Error("failed to read the http request body", zap.Any("metadata", getReqMeta(req)), zap.Error(err))
			return err
		}
	}

	// converts the response message buffer to http response
	respParsed, err := http.ReadResponse(bufio.NewReader(bytes.NewReader(mock.resp)), req)
	if err != nil {
		logger.Error("failed to parse the http response message", zap.Any("metadata", getReqMeta(req)), zap.Error(err))
		return err
	}

	//Add the content length to the headers.
	var respBody []byte
	//Checking if the body of the response is empty or does not exist.
	if respParsed.Body != nil { // Read
		if respParsed.Header.Get("Content-Encoding") == "gzip" {
			check := respParsed.Body
			ok, reader := isGZipped(check, logger)
			logger.Debug("The body is gzip? " + strconv.FormatBool(ok))
			logger.Debug("", zap.Any("isGzipped", ok))
			if ok {
				gzipReader, err := gzip.NewReader(reader)
				if err != nil {
					logger.Error("failed to create a gzip reader", zap.Any("metadata", getReqMeta(req)), zap.Error(err))
					return err
				}
				respParsed.Body = gzipReader
			}
		}
		respBody, err = io.ReadAll(respParsed.Body)
		if err != nil {
			logger.Error("failed to read the the http response body", zap.Any("metadata", getReqMeta(req)), zap.Error(err))
			return err
		}
		logger.Debug("This is the response body: " + string(respBody))
		//Set the content length to the headers.
		respParsed.Header.Set("Content-Length", strconv.Itoa(len(respBody)))
	}

	// store the request and responses as mocks
	meta := map[string]string{
		"name":      "Http",
		"type":      models.HttpClient,
		"operation": req.Method,
	}

	// Check if the request is a passThrough request
	if isPassThrough(logger, req, destPort, opts) {
		logger.Debug("The request is a passThrough request", zap.Any("metadata", getReqMeta(req)))
		return nil
	}

	mocks <- &models.Mock{
		Version: models.GetVersion(),
		Name:    "mocks",
		Kind:    models.HTTP,
		Spec: models.MockSpec{
			Metadata: meta,
			HttpReq: &models.HttpReq{
				Method:     models.Method(req.Method),
				ProtoMajor: req.ProtoMajor,
				ProtoMinor: req.ProtoMinor,
				URL:        req.URL.String(),
				Header:     pkg.ToYamlHttpHeader(req.Header),
				Body:       string(reqBody),
				URLParams:  pkg.UrlParams(req),
				Host:       req.Host,
			},
			HttpResp: &models.HttpResp{
				StatusCode: respParsed.StatusCode,
				Header:     pkg.ToYamlHttpHeader(respParsed.Header),
				Body:       string(respBody),
			},
			Created:          time.Now().Unix(),
			ReqTimestampMock: mock.resTimestampMock,
			ResTimestampMock: mock.resTimestampMock,
		},
	}
	return nil
}
