package proxy_test

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"net/http"
	"time"

	utils "go.keploy.io/server/v2/pkg/core/hooks/conn"
	"go.keploy.io/server/v2/pkg/models" // Assuming this is the models package
	"go.uber.org/zap"
)

// MyDecoder implements the proxy.TestCaseCreator interface.
// It uses standard library functions to parse raw HTTP traffic and
// transforms it into the specific TestCase structure you provided.
type MyDecoder struct{}

func NewMyDecoder() *MyDecoder {
	return &MyDecoder{}
}

func (d *MyDecoder) Create(ctx context.Context, logger *zap.Logger, t chan *models.TestCase, reqData, respData []byte, reqTime, respTime time.Time, opts models.IncomingOptions) error {

	// 1. Parse the raw request data.
	req, err := http.ReadRequest(bufio.NewReader(bytes.NewReader(reqData)))
	if err != nil {
		return fmt.Errorf("failed to parse raw http request: %w", err)
	}

	// 2. Parse the raw response data.
	resp, err := http.ReadResponse(bufio.NewReader(bytes.NewReader(respData)), req)
	if err != nil {
		return fmt.Errorf("failed to parse raw http response: %w", err)
	}

	// 3. Defer closing the bodies to prevent resource leaks.
	defer req.Body.Close()
	defer resp.Body.Close()

	// 4. Call your original, unmodified Capture function to do the heavy lifting.
	utils.Capture(ctx, logger, t, req, resp, reqTime, respTime, opts)

	return nil
}
