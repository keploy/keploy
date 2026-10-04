package proxy

import (
	"bufio"
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	hooksUtils "go.keploy.io/server/v3/pkg/agent/hooks/conn"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// Exercise the real forwarder and Capture together: skipping a recorded asset
// must not change the response delivered to the client, including binary bytes.
func TestStaticAssetsForwardedAndCaptureOptionHonored(t *testing.T) {
	for _, tc := range []struct {
		name        string
		contentType string
		body        string
		include     bool
		wantCapture bool
	}{
		{name: "asset", contentType: "image/png", body: "\x89PNG\x00\xff"},
		{name: "asset opt out", contentType: "image/png", body: "\x89PNG\x00\xff", include: true, wantCapture: true},
		{name: "json api", contentType: "application/json", body: `{"ok":true}`, wantCapture: true},
		{name: "html", contentType: "text/html", body: "<html>hello</html>", wantCapture: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", tc.contentType)
				_, _ = io.WriteString(w, tc.body)
			}))
			defer upstream.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			pm := &IngressProxyManager{logger: zap.NewNop(), tcChan: make(chan *models.TestCase, 1)}
			pm.incomingOpts.Store(&models.IncomingOptions{IncludeStaticAssets: tc.include})
			captured := make(chan struct{})
			stubCaptureHook(t, func(ctx context.Context, logger *zap.Logger, cases chan *models.TestCase,
				req *http.Request, resp *http.Response, reqTS, respTS time.Time,
				opts models.IncomingOptions, synchronous, mapping bool, port uint16) {
				hooksUtils.Capture(ctx, logger, cases, req, resp, reqTS, respTS, opts, synchronous, mapping, port)
				close(captured)
			})
			client, proxy := net.Pipe()
			defer client.Close()
			defer proxy.Close()
			require.NoError(t, client.SetDeadline(time.Now().Add(5*time.Second)))
			done := make(chan struct{})
			go func() {
				defer close(done)
				pm.handleHttp1Connection(ctx, proxy, strings.TrimPrefix(upstream.URL, "http://"),
					pm.logger, pm.tcChan, make(chan struct{}, 1), 8080)
			}()
			req := httptest.NewRequest(http.MethodGet, "http://example.com/resource", nil)
			req.Close = true
			require.NoError(t, req.Write(client))
			resp, err := http.ReadResponse(bufio.NewReader(client), req)
			require.NoError(t, err)
			got, err := io.ReadAll(resp.Body)
			require.NoError(t, err)
			require.NoError(t, resp.Body.Close())
			assert.Equal(t, http.StatusOK, resp.StatusCode)
			assert.Equal(t, tc.body, string(got))
			select {
			case <-captured:
			case <-ctx.Done():
				t.Fatal("capture did not complete")
			}
			select {
			case <-done:
			case <-ctx.Done():
				t.Fatal("forwarder did not finish")
			}
			if tc.wantCapture {
				require.Len(t, pm.tcChan, 1)
				assert.Equal(t, tc.body, (<-pm.tcChan).HTTPResp.Body)
			} else {
				assert.Empty(t, pm.tcChan)
			}
		})
	}
}
