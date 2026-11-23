package proxy

import (
	"bufio"
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	hooksUtils "go.keploy.io/server/v3/pkg/agent/hooks/conn"
	syncMock "go.keploy.io/server/v3/pkg/agent/proxy/syncMock"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// Exercise both HTTP/1.1 forwarder loops (synchronous and zero-copy) with
// IgnoreStaticAssets: skipped web static assets short-circuit at response
// headers (never invoking CaptureHook), while the client still receives the
// exact response bytes; default-off, binary/PDF responses, and explicit Include
// filters continue to capture normally.
func TestStaticAssetsForwardedAndHeaderShortCircuit(t *testing.T) {
	for _, synchronous := range []bool{false, true} {
		mode := map[bool]string{false: "zero-copy", true: "synchronous"}[synchronous]
		for _, tc := range []struct {
			name        string
			path        string
			contentType string
			body        string
			opts        models.IncomingOptions
			wantCapture bool
		}{
			{
				name:        "default off captures image",
				path:        "/logo.png",
				contentType: "image/png",
				body:        "\x89PNG\x00\xff",
				opts:        models.IncomingOptions{},
				wantCapture: true,
			},
			{
				name:        "ignore static assets skips image before CaptureHook",
				path:        "/logo.png",
				contentType: "image/png",
				body:        "\x89PNG\x00\xff",
				opts:        models.IncomingOptions{IgnoreStaticAssets: true},
				wantCapture: false,
			},
			{
				name:        "ignore static assets preserves pdf download",
				path:        "/api/invoice.pdf",
				contentType: "application/pdf",
				body:        "%PDF-1.7\x00binary",
				opts:        models.IncomingOptions{IgnoreStaticAssets: true},
				wantCapture: true,
			},
			{
				name:        "ignore static assets preserves octet-stream",
				path:        "/api/export",
				contentType: "application/octet-stream",
				body:        "\x01\x02\x03\x04",
				opts:        models.IncomingOptions{IgnoreStaticAssets: true},
				wantCapture: true,
			},
			{
				name:        "explicit Include filter wins over ignore static assets",
				path:        "/api/avatars/1.png",
				contentType: "image/png",
				body:        "\x89PNG\x00\xff",
				opts: models.IncomingOptions{
					IgnoreStaticAssets: true,
					Filters: []models.Filter{
						{BypassRule: models.BypassRule{Path: "/api/avatars/.*"}, FilterPolicy: models.Include},
					},
				},
				wantCapture: true,
			},
		} {
			t.Run(mode+"/"+tc.name, func(t *testing.T) {
				stubIngressPaused(t, func() bool { return false })
				mgr := syncMock.New(zap.NewNop())

				upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					w.Header().Set("Content-Type", tc.contentType)
					_, _ = io.WriteString(w, tc.body)
				}))
				t.Cleanup(upstream.Close)

				pm := &IngressProxyManager{
					logger:      zap.NewNop(),
					tcChan:      make(chan *models.TestCase, 2),
					synchronous: synchronous,
					samplingSem: make(chan struct{}, 1),
				}
				opts := tc.opts
				pm.incomingOpts.Store(&opts)

				var hookRuns atomic.Int32
				stubCaptureHook(t, func(ctx context.Context, logger *zap.Logger, cases chan *models.TestCase,
					req *http.Request, resp *http.Response, reqTS, respTS time.Time,
					opts models.IncomingOptions, syncMode, mapping bool, port uint16) {
					hookRuns.Add(1)
					hooksUtils.Capture(ctx, logger, cases, req, resp, reqTS, respTS, opts, syncMode, mapping, port)
				})

				client, proxy := net.Pipe()
				t.Cleanup(func() {
					_ = client.Close()
					_ = proxy.Close()
				})
				require.NoError(t, client.SetDeadline(time.Now().Add(5*time.Second)))

				ctx, cancel := context.WithTimeout(syncMock.NewContext(context.Background(), mgr), 5*time.Second)
				defer cancel()

				done := make(chan struct{})
				go func() {
					defer close(done)
					pm.handleHttp1Connection(ctx, proxy, strings.TrimPrefix(upstream.URL, "http://"),
						pm.logger, pm.tcChan, make(chan struct{}, 1), 8080)
				}()
				// Register after stubCaptureHook so LIFO cleanup waits for handler +
				// capture goroutines before stubCaptureHook restores the global hook.
				t.Cleanup(func() {
					_ = client.Close()
					<-done
					pm.captures.Wait()
				})

				req := httptest.NewRequest(http.MethodGet, "http://example.com"+tc.path, nil)
				req.Close = true
				require.NoError(t, req.Write(client))

				resp, err := http.ReadResponse(bufio.NewReader(client), req)
				require.NoError(t, err)
				got, err := io.ReadAll(resp.Body)
				require.NoError(t, err)
				require.NoError(t, resp.Body.Close())
				assert.Equal(t, http.StatusOK, resp.StatusCode)
				assert.Equal(t, tc.body, string(got))

				<-done
				pm.captures.Wait()

				if tc.wantCapture {
					assert.Equal(t, int32(1), hookRuns.Load())
					require.Len(t, pm.tcChan, 1)
					assert.Equal(t, tc.body, (<-pm.tcChan).HTTPResp.Body)
				} else {
					assert.Equal(t, int32(0), hookRuns.Load(), "skipped static asset should short-circuit before CaptureHook")
					assert.Empty(t, pm.tcChan)
				}
			})
		}
	}
}
