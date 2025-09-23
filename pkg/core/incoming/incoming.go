package IncomingTestCase

import (
	"context"
	"fmt"
	"time"

	"go.keploy.io/server/v2/pkg"
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

	parsedHTTPReq, err := pkg.ParseHTTPRequest(reqData)

	if err != nil {
		return fmt.Errorf("failed to parse raw http request: %w", err)
	}

	parsedHTTPRes, err := pkg.ParseHTTPResponse(respData, parsedHTTPReq)
	if err != nil {
		return fmt.Errorf("failed to parse raw http response: %w", err)
	}

	defer parsedHTTPReq.Body.Close()
	defer parsedHTTPRes.Body.Close()

	utils.Capture(ctx, d.logger, t, parsedHTTPReq, parsedHTTPRes, reqTime, respTime, opts)

	return nil
}
