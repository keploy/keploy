package proxy

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/internal/leakcheck"
	syncMock "go.keploy.io/server/v3/pkg/agent/proxy/syncMock"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// The ingress lives as long as the recording, a long native or docker one's
// for days, and serves every connection the app is sent in that time. A
// connection it is done with leaves nothing behind it: no goroutine of its
// handler parked on the ingress's context, and no registration on that
// context. One goroutine was left, per connection, by the zero-copy HTTP/1.1
// loop and the raw passthrough alike, parked on the context and holding the
// closed connection's objects until the agent stopped: a soak's 10
// connections a second left 600 goroutines, about 3 MB, a minute. The close
// on shutdown each handler now registers instead must be stopped as the
// handler returns, or it leaves the same growth, a registration and a
// closure holding the connection per connection, with no goroutine to show.
func TestIngressHandlersLeaveNothingBehindAClosedConnection(t *testing.T) {
	stubCaptureHook(t, func(ctx context.Context, logger *zap.Logger, tc chan *models.TestCase,
		req *http.Request, resp *http.Response, reqTS, respTS time.Time,
		opts models.IncomingOptions, sync bool, mapping bool, appPort uint16) {
	})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))
	t.Cleanup(upstream.Close)
	pm := &IngressProxyManager{logger: zap.NewNop(), tcChan: make(chan *models.TestCase, 64)}
	// The ingress's context: live for the whole test, as an agent's is for
	// every connection it serves.
	ctx := leakcheck.NewContext(syncMock.NewContext(context.Background(), syncMock.New(zap.NewNop())))
	defer ctx.Cancel()

	const conns = 50
	for i := 0; i < conns; i++ {
		upConn, err := net.Dial("tcp4", strings.TrimPrefix(upstream.URL, "http://"))
		if err != nil {
			t.Fatal(err)
		}
		server, client := tcpPair(t)
		done := make(chan struct{})
		go func() {
			defer close(done)
			pm.handleHttp1ZeroCopy(ctx, server, upConn, pm.logger, pm.tcChan, 8080, i%2 == 0, func() {})
		}()
		if _, err := fmt.Fprintf(client, "GET /c%d HTTP/1.1\r\nHost: app\r\n\r\n", i); err != nil {
			t.Fatal(err)
		}
		resp, err := http.ReadResponse(bufio.NewReader(client), nil)
		if err != nil {
			t.Fatalf("connection %d: %v", i, err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		_ = client.Close() // the client hangs up, as most do
		<-done
		// What the caller (handleConnection) closes as it returns.
		_ = server.Close()
		_ = upConn.Close()
	}
	httpLive := ctx.Live()
	for i := 0; i < conns; i++ {
		appSide, clientSide := tcpPair(t)
		upSide, appPeer := tcpPair(t)
		done := make(chan struct{})
		go func() {
			defer close(done)
			forwardRawTCP(ctx, appSide, upSide)
		}()
		_ = clientSide.Close()
		_ = appPeer.Close()
		<-done
	}
	pm.captures.Wait()

	if n, g := leakcheck.Parked("incoming.(*IngressProxyManager).handleHttp1ZeroCopy", "incoming.forwardRawTCP"); n != 0 {
		t.Errorf("%d goroutines of the ingress's handlers are parked after the %d connections they served all ended; the first:\n%s", n, 2*conns, g)
	}
	if live := ctx.Live(); live != 0 {
		t.Errorf("%d registrations on the ingress's context outlive the %d connections (%d after the HTTP/1.1 half)", live, 2*conns, httpLive)
	}
}
