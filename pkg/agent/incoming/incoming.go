package incoming


import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"net/http"
	"time"

	"go.keploy.io/server/v2/pkg"
	utils "go.keploy.io/server/v2/pkg/agent/hooks/conn"
	"go.keploy.io/server/v2/pkg/models"
	"go.uber.org/zap"
)

type TestcaseCapture struct{}

func NewTestcaseCapture() *TestcaseCapture {
	return &TestcaseCapture{}
}

func (d *TestcaseCapture) CreateHTTP(ctx context.Context, logger *zap.Logger, t chan *models.TestCase, reqData, respData []byte, reqTime, respTime time.Time, opts models.IncomingOptions) error {

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

	utils.Capture(ctx, logger, t, req, resp, reqTime, respTime, opts)

	return nil
}

func (d *TestcaseCapture) CreateGRPC(ctx context.Context, logger *zap.Logger, t chan *models.TestCase, stream *pkg.HTTP2Stream) error {

	utils.CaptureGRPC(ctx, logger, t, stream)
	return nil
}
