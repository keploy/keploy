//go:build linux || darwin

package proxy

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"testing"

	"go.uber.org/zap"

	"go.keploy.io/server/v3/config"
	syncMock "go.keploy.io/server/v3/pkg/agent/proxy/syncMock"
)

// A request the app served and the ingress was recording either reaches the
// recording's queue as a test case or is counted as one that did not
// (syncMock NoteNotHandedOn): never a silent loss. The context a capture runs
// on ends as its ingress stops (a forwarder's StopIngress, the agent's stop),
// and the capture's waits for a slot and for the queue gave up on it without
// a count: a recording that lost test cases so ended short with nothing said
// (tcs_sent_to_cli and tcs_suppressed_total both 0 in a run that lost every
// one). Each round serves one Connection: close exchange and ends the
// context the moment the exchange is served, as an ingress stopped then
// would; the capture is then racing that end.
func TestEveryRecordedRequestIsHandedOnOrCountedAsItsIngressStops(t *testing.T) {
	appLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	serveApp(t, appLn, func(w http.ResponseWriter, _ *http.Request) { _, _ = fmt.Fprint(w, "pong") })
	cfg := &config.Config{}
	cfg.Agent.MockMode = true
	cfg.Agent.RecordRequests = true
	pm := New(zap.NewNop(), nil, cfg)
	mgr := syncMock.New(zap.NewNop())
	const rounds = 80 // under the channel's 100, so no round waits on a reader
	for i := 0; i < rounds; i++ {
		client, proxied := tcpPairForWatch(t)
		ctx, cancel := context.WithCancel(syncMock.NewContext(context.Background(), mgr))
		done := make(chan struct{})
		go func() {
			defer close(done)
			pm.ServeLive(ctx, proxied, appLn.Addr().String(), 8083)
			cancel() // the ingress stops as soon as the exchange is served
			_ = proxied.Close()
		}()
		if _, err := fmt.Fprintf(client, "GET /ping/%d HTTP/1.1\r\nHost: app\r\nConnection: close\r\n\r\n", i); err != nil {
			t.Fatal(err)
		}
		resp, err := http.ReadResponse(bufio.NewReader(client), nil)
		if err != nil {
			t.Fatal(err)
		}
		// The whole answer is read before the client goes: one that goes
		// first is told as not recorded for that (tellClientLeft).
		if _, err := io.ReadAll(resp.Body); err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		_ = client.Close()
		<-done
	}
	pm.captures.Wait()
	delivered, counted := uint64(len(pm.tcChan)), mgr.NotHandedOn()
	if delivered+counted != rounds {
		t.Fatalf("of %d served requests %d reached the test-case channel and %d were counted as not handed on: %d were lost without a count",
			rounds, delivered, counted, rounds-delivered-counted)
	}
	t.Logf("%d handed on, %d counted as not handed on", delivered, counted)
}
