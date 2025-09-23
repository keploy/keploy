package IncomingTestCase

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"net/http"
	"time"

	utils "go.keploy.io/server/v2/pkg/core/hooks/conn"
	"go.keploy.io/server/v2/pkg/models"
	"go.uber.org/zap"
)

type CaptureIncoming struct {
	logger *zap.Logger
}

func NewCaptureIncoming(logger *zap.Logger) *CaptureIncoming {
	return &CaptureIncoming{
		logger: logger,
	}
}

func (d *CaptureIncoming) CreateHTTP(ctx context.Context, t chan *models.TestCase, reqData, respData []byte, reqTime, respTime time.Time, opts models.IncomingOptions) error {

	req, err := http.ReadRequest(bufio.NewReader(bytes.NewReader(reqData)))
	if err != nil {
		return fmt.Errorf("failed to parse raw http request: %w", err)
	}

	resp, err := http.ReadResponse(bufio.NewReader(bytes.NewReader(respData)), req)
	if err != nil {
		return fmt.Errorf("failed to parse raw http response: %w", err)
	}

	defer req.Body.Close()
	defer resp.Body.Close()

	utils.Capture(ctx, d.logger, t, req, resp, reqTime, respTime, opts)

	return nil
}
