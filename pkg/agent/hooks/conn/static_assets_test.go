package conn

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

func TestCaptureStaticAssets(t *testing.T) {
	for _, tc := range []struct {
		name        string
		contentType string
		method      string
		status      int
		wantCapture bool
	}{
		{name: "css", contentType: "text/css; charset=utf-8"},
		{name: "javascript", contentType: "text/javascript"},
		{name: "legacy javascript", contentType: "application/javascript"},
		{name: "png", contentType: "image/png"},
		{name: "svg", contentType: "image/svg+xml"},
		{name: "font", contentType: "font/woff2"},
		{name: "legacy font", contentType: "application/font-woff"},
		{name: "audio", contentType: "audio/mpeg"},
		{name: "video", contentType: "video/mp4"},
		{name: "binary", contentType: "application/octet-stream"},
		{name: "pdf", contentType: "application/pdf"},
		{name: "zip", contentType: "application/zip"},
		{name: "wasm", contentType: "application/wasm"},
		{name: "head", method: http.MethodHead, contentType: "image/png"},
		{name: "not modified", status: http.StatusNotModified, contentType: "text/css"},
		{name: "case insensitive", contentType: "IMAGE/PNG; charset=UTF-8"},
		{name: "json", contentType: "application/json", wantCapture: true},
		{name: "vendor json", contentType: "application/vnd.example+json", wantCapture: true},
		{name: "html", contentType: "text/html", wantCapture: true},
		{name: "xml", contentType: "application/xml", wantCapture: true},
		{name: "text", contentType: "text/plain", wantCapture: true},
		{name: "stream", contentType: "text/event-stream", wantCapture: true},
		{name: "unknown", contentType: "application/x-custom", wantCapture: true},
		{name: "missing type", wantCapture: true},
		{name: "malformed type", contentType: "image/png; invalid", wantCapture: true},
		{name: "failed asset", status: http.StatusNotFound, contentType: "image/png", wantCapture: true},
		{name: "mutating request", method: http.MethodPost, contentType: "image/png", wantCapture: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			method := tc.method
			if method == "" {
				method = http.MethodGet
			}
			status := tc.status
			if status == 0 {
				status = http.StatusOK
			}
			// An asset-looking path must not suppress a JSON API response.
			req := httptest.NewRequest(method, "http://example.com/assets/file.png", nil)
			body := &trackCloser{Reader: strings.NewReader("response body")}
			resp := &http.Response{StatusCode: status, Header: make(http.Header), Body: body}
			resp.Header.Set("Content-Type", tc.contentType)
			cases := make(chan *models.TestCase, 1)
			Capture(context.Background(), zap.NewNop(), cases, req, resp,
				time.Now(), time.Now(), models.IncomingOptions{}, false, false, 8080)
			assert.True(t, body.closed)
			if tc.wantCapture {
				require.Len(t, cases, 1)
				assert.Equal(t, "response body", (<-cases).HTTPResp.Body)
			} else {
				assert.Empty(t, cases)
			}
		})
	}
}

type unreadableAssetBody struct {
	read   bool
	closed bool
}

func (b *unreadableAssetBody) Read([]byte) (int, error) {
	b.read = true
	return 0, io.ErrUnexpectedEOF
}

func (b *unreadableAssetBody) Close() error {
	b.closed = true
	return nil
}

func TestCaptureStaticAssetDoesNotReadBodies(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "http://example.com/image", nil)
	reqBody, respBody := &unreadableAssetBody{}, &unreadableAssetBody{}
	req.Body = reqBody
	resp := &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"image/png"}}, Body: respBody}
	cases := make(chan *models.TestCase, 1)
	Capture(context.Background(), zap.NewNop(), cases, req, resp,
		time.Now(), time.Now(), models.IncomingOptions{}, false, false, 8080)
	assert.Empty(t, cases)
	assert.False(t, reqBody.read)
	assert.False(t, respBody.read)
	assert.True(t, reqBody.closed)
	assert.True(t, respBody.closed)
}

func TestCaptureIncludesStaticAssetsWhenRequested(t *testing.T) {
	for _, explicitFilter := range []bool{false, true} {
		t.Run(fmt.Sprintf("explicit filter=%t", explicitFilter), func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "http://example.com/image", nil)
			body := &trackCloser{Reader: strings.NewReader("image bytes")}
			resp := &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"image/png"}}, Body: body}
			opts := models.IncomingOptions{IncludeStaticAssets: true}
			if explicitFilter {
				opts.Filters = []models.Filter{{URLMethods: []string{http.MethodGet}, FilterPolicy: models.Exclude}}
			}
			cases := make(chan *models.TestCase, 1)
			Capture(context.Background(), zap.NewNop(), cases, req, resp,
				time.Now(), time.Now(), opts, false, false, 8080)
			assert.True(t, body.closed)
			if explicitFilter {
				assert.Empty(t, cases)
			} else {
				require.Len(t, cases, 1)
				assert.Equal(t, "image bytes", (<-cases).HTTPResp.Body)
			}
		})
	}
}
