//go:build linux

package http

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"time"

	"go.keploy.io/server/v2/pkg/core/proxy/integrations"
	"go.keploy.io/server/v2/pkg/core/proxy/util"
	"go.keploy.io/server/v2/utils"

	"go.keploy.io/server/v2/pkg"
	"go.keploy.io/server/v2/pkg/models"
	"go.uber.org/zap"
)

func init() {
	integrations.Register(integrations.HTTP, &integrations.Parsers{
		Initializer: New, Priority: 100,
	})
}

type HTTP struct {
	Logger *zap.Logger
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

func (h *HTTP) MatchType(_ context.Context, buf []byte) bool {
	isHTTP := bytes.HasPrefix(buf[:], []byte("HTTP/")) ||
		bytes.HasPrefix(buf[:], []byte("GET ")) ||
		bytes.HasPrefix(buf[:], []byte("POST ")) ||
		bytes.HasPrefix(buf[:], []byte("PUT ")) ||
		bytes.HasPrefix(buf[:], []byte("PATCH ")) ||
		bytes.HasPrefix(buf[:], []byte("DELETE ")) ||
		bytes.HasPrefix(buf[:], []byte("OPTIONS ")) ||
		bytes.HasPrefix(buf[:], []byte("HEAD "))
	h.Logger.Debug(fmt.Sprintf("is Http Protocol?: %v ", isHTTP))
	return isHTTP
}

func (h *HTTP) RecordOutgoing(ctx context.Context, src net.Conn, dst net.Conn, mocks chan<- *models.Mock, opts models.OutgoingOptions) error {
	logger := h.Logger.With(
		zap.Any("Client IP Address", src.RemoteAddr().String()),
		zap.Any("Client ConnectionID", ctx.Value(models.ClientConnectionIDKey).(string)),
		zap.Any("Destination ConnectionID", ctx.Value(models.DestConnectionIDKey).(string)),
	)

	h.Logger.Debug("Recording the outgoing http call in record mode")

	fullReqBuf, err := util.ReadBytes(ctx, logger, src)
	if err != nil && err != io.EOF {
		utils.LogError(logger, err, "failed to read complete HTTP request")
		return err
	}

	logger.Debug("Complete request received", zap.Int("size", len(fullReqBuf)))

	err = h.encodeHTTP(ctx, fullReqBuf, src, dst, mocks, opts)
	if err != nil {
		utils.LogError(logger, err, "failed to encode the http message")
		return err
	}

	return nil
}

func (h *HTTP) MockOutgoing(ctx context.Context, src net.Conn, dstCfg *models.ConditionalDstCfg, mockDb integrations.MockMemDb, opts models.OutgoingOptions) error {
	h.Logger = h.Logger.With(
		zap.Any("Client IP Address", src.RemoteAddr().String()),
		zap.Any("Client ConnectionID", ctx.Value(models.ClientConnectionIDKey).(string)),
		zap.Any("Destination ConnectionID", ctx.Value(models.DestConnectionIDKey).(string)),
	)

	h.Logger.Debug("Mocking the outgoing http call in test mode")

	reqBuf, err := util.ReadInitialBuf(ctx, h.Logger, src)
	if err != nil {
		utils.LogError(h.Logger, err, "failed to read the initial http message")
		return err
	}

	err = h.decodeHTTP(ctx, reqBuf, src, dstCfg, mockDb, opts)
	if err != nil {
		utils.LogError(h.Logger, err, "failed to decode the http message from the yaml")
		return err
	}

	return nil
}

func (h *HTTP) parseFinalHTTP(_ context.Context, mock *FinalHTTP, destPort uint, mocks chan<- *models.Mock, opts models.OutgoingOptions) error {
	req, err := http.ReadRequest(bufio.NewReader(bytes.NewReader(mock.Req)))
	if err != nil {
		utils.LogError(h.Logger, err, "failed to parse the http request message")
		return err
	}

	req.Header.Set("Host", req.Host)

	var reqBody []byte
	if req.Body != nil {
		reqBody, err = io.ReadAll(req.Body)
		if err != nil {
			utils.LogError(h.Logger, err, "failed to read the http request body", zap.Any("metadata", GetReqMeta(req)))
			return err
		}
	}

	respParsed, err := http.ReadResponse(bufio.NewReader(bytes.NewReader(mock.Resp)), req)
	if err != nil {
		utils.LogError(h.Logger, err, "failed to parse the http response message", zap.Any("metadata", GetReqMeta(req)))
		return err
	}

	var respBody []byte
	if respParsed.Body != nil {
		if respParsed.Header.Get("Content-Encoding") == "gzip" {
			ok, reader := isGZipped(respParsed.Body, h.Logger)
			h.Logger.Debug("The body is gzip? " + strconv.FormatBool(ok))
			if ok {
				gzipReader, err := gzip.NewReader(reader)
				if err != nil {
					utils.LogError(h.Logger, err, "failed to create a gzip reader", zap.Any("metadata", GetReqMeta(req)))
					return err
				}
				respParsed.Body = gzipReader
			}
		}

		respBody, err = io.ReadAll(respParsed.Body)
		if err != nil {
			utils.LogError(h.Logger, err, "failed to read the the http response body", zap.Any("metadata", GetReqMeta(req)))
			return err
		}

		h.Logger.Debug("This is the response body: " + string(respBody))
		respParsed.Header.Set("Content-Length", strconv.Itoa(len(respBody)))
	}

	meta := map[string]string{
		"name":      "Http",
		"type":      models.HTTPClient,
		"operation": req.Method,
	}

	if IsPassThrough(h.Logger, req, destPort, opts) {
		h.Logger.Debug("The request is a passThrough request", zap.Any("metadata", GetReqMeta(req)))
		return nil
	}

	mocks <- &models.Mock{
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
			ReqTimestampMock: mock.ReqTimestampMock,
			ResTimestampMock: mock.ResTimestampMock,
		},
	}

	return nil
}
