package conn

import (
	"context"
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

func TestCaptureStaticAssetsDefaultOff(t *testing.T) {
	// Opt-in default (IgnoreStaticAssets: false) must record static assets and binaries.
	for _, ct := range []string{"text/css", "application/javascript", "image/png", "font/woff2", "application/pdf", "application/octet-stream"} {
		t.Run(ct, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "http://example.com/resource", nil)
			body := &trackCloser{Reader: strings.NewReader("payload")}
			resp := &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {ct}}, Body: body}
			cases := make(chan *models.TestCase, 1)
			Capture(context.Background(), zap.NewNop(), cases, req, resp,
				time.Now(), time.Now(), models.IncomingOptions{}, false, false, 8080)
			assert.True(t, body.closed)
			require.Len(t, cases, 1)
			assert.Equal(t, "payload", (<-cases).HTTPResp.Body)
		})
	}
}

func TestCaptureStaticAssetsWhenIgnored(t *testing.T) {
	for _, tc := range []struct {
		name        string
		contentType string
		method      string
		status      int
		wantCapture bool
	}{
		// Web static assets skipped when IgnoreStaticAssets is enabled.
		{name: "css", contentType: "text/css; charset=utf-8"},
		{name: "javascript", contentType: "text/javascript"},
		{name: "legacy javascript", contentType: "application/javascript"},
		{name: "x-javascript", contentType: "application/x-javascript"},
		{name: "png", contentType: "image/png"},
		{name: "svg", contentType: "image/svg+xml"},
		{name: "font", contentType: "font/woff2"},
		{name: "legacy font", contentType: "application/font-woff"},
		{name: "audio", contentType: "audio/mpeg"},
		{name: "video", contentType: "video/mp4"},
		{name: "head", method: http.MethodHead, contentType: "image/png"},
		{name: "not modified", status: http.StatusNotModified, contentType: "text/css"},
		{name: "case insensitive", contentType: "IMAGE/PNG; charset=UTF-8"},

		// Binary / API download responses must ALWAYS be recorded.
		{name: "octet-stream preserved", contentType: "application/octet-stream", wantCapture: true},
		{name: "pdf preserved", contentType: "application/pdf", wantCapture: true},
		{name: "zip preserved", contentType: "application/zip", wantCapture: true},
		{name: "gzip preserved", contentType: "application/gzip", wantCapture: true},
		{name: "wasm preserved", contentType: "application/wasm", wantCapture: true},

		// Standard API / document / error / mutating responses remain recordable.
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
				time.Now(), time.Now(), models.IncomingOptions{IgnoreStaticAssets: true}, false, false, 8080)
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

func TestIsStaticAssetResponseNilSafe(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "http://example.com/logo.png", nil)
	resp := &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"image/png"}}}
	assert.False(t, IsStaticAssetResponse(nil, resp))
	assert.False(t, IsStaticAssetResponse(req, nil))
	assert.False(t, IsStaticAssetResponse(nil, nil))
	assert.False(t, ShouldSkipStaticAsset(zap.NewNop(), nil, resp, models.IncomingOptions{IgnoreStaticAssets: true}))
	assert.False(t, ShouldSkipStaticAsset(zap.NewNop(), req, nil, models.IncomingOptions{IgnoreStaticAssets: true}))
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
		time.Now(), time.Now(), models.IncomingOptions{IgnoreStaticAssets: true}, false, false, 8080)
	assert.Empty(t, cases)
	assert.False(t, reqBody.read)
	assert.False(t, respBody.read)
	assert.True(t, reqBody.closed)
	assert.True(t, respBody.closed)
}

func TestCaptureExplicitIncludeFilterOverridesIgnoreStaticAssets(t *testing.T) {
	for _, tc := range []struct {
		name        string
		path        string
		method      string
		filters     []models.Filter
		wantCapture bool
	}{
		{
			name:   "explicit Include wins over IgnoreStaticAssets",
			path:   "/api/avatars/1.png",
			method: http.MethodGet,
			filters: []models.Filter{
				{BypassRule: models.BypassRule{Path: "/api/avatars/.*"}, FilterPolicy: models.Include},
			},
			wantCapture: true,
		},
		{
			name:   "non-matching Include still filters asset",
			path:   "/static/logo.png",
			method: http.MethodGet,
			filters: []models.Filter{
				{BypassRule: models.BypassRule{Path: "/api/avatars/.*"}, FilterPolicy: models.Include},
			},
			wantCapture: false,
		},
		{
			name:   "explicit Exclude still wins over Include",
			path:   "/api/avatars/1.png",
			method: http.MethodHead,
			filters: []models.Filter{
				{BypassRule: models.BypassRule{Path: "/api/avatars/.*"}, FilterPolicy: models.Include},
				{URLMethods: []string{http.MethodHead}, FilterPolicy: models.Exclude},
			},
			wantCapture: false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(tc.method, "http://example.com"+tc.path, nil)
			body := &trackCloser{Reader: strings.NewReader("image bytes")}
			resp := &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"image/png"}}, Body: body}
			opts := models.IncomingOptions{
				IgnoreStaticAssets: true,
				Filters:            tc.filters,
			}
			cases := make(chan *models.TestCase, 1)
			Capture(context.Background(), zap.NewNop(), cases, req, resp,
				time.Now(), time.Now(), opts, false, false, 8080)
			assert.True(t, body.closed)
			if tc.wantCapture {
				require.Len(t, cases, 1)
				assert.Equal(t, "image bytes", (<-cases).HTTPResp.Body)
			} else {
				assert.Empty(t, cases)
			}
		})
	}
}
