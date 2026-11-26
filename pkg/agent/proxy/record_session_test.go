package proxy

import (
	"bufio"
	"context"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"

	"go.keploy.io/server/v3/config"
	syncMock "go.keploy.io/server/v3/pkg/agent/proxy/syncMock"
	"go.keploy.io/server/v3/pkg/models"
)

// A recording keploy's agent makes after another in the same process (a
// long-lived agent, or keploy's own next session) starts as the first did:
// before its own first request, what the application calls is a startup
// call, sent on as it is captured. The process-wide mock manager kept the
// earlier recording's first request seen, so a call before the new
// recording's first request waited, as a test's, for a test case's window
// that never came, and its mock never reached the recording.
func TestARecordingAfterAnotherStartsBeforeItsFirstRequest(t *testing.T) {
	// The earlier recording's state, as its first request left it.
	mgr := syncMock.Get()
	mgr.SetFirstRequestSignaled()
	t.Cleanup(func() { mgr.SetOutputChannel(nil) })

	upstream := keepAliveHTTPUpstream(t)
	cfg := config.New()
	cfg.Agent.Mode = models.MODE_RECORD
	p := New(zap.NewNop(), fakeDestInfo{dest: ipv4Dest(t, upstream)}, cfg)
	if err := p.InitIntegrations(context.Background()); err != nil {
		t.Fatal(err)
	}
	mocks := make(chan *models.Mock, 16)
	if err := p.Record(context.Background(), mocks, models.OutgoingOptions{DisableMysqlAutoDetect: true}); err != nil {
		t.Fatal(err)
	}
	if mgr.GetFirstReqSeen() {
		t.Fatal("a new recording starts with the earlier one's first request seen")
	}

	// A startup call: no request of the recording's yet.
	app, src := acceptedConn(t)
	done := make(chan error, 1)
	go func() { done <- p.handleConnection(context.Background(), src) }()
	if body := awaitEvent(t, get(app, bufio.NewReader(app), "/startup", true), "the startup call"); body != "/startup" {
		t.Fatalf("the startup call got %q", body)
	}
	_ = app.Close()
	recorded := make(chan string, 1)
	go func() {
		for m := range mocks {
			if m.Spec.HTTPReq != nil {
				recorded <- m.Spec.HTTPReq.URL
				return
			}
		}
	}()
	if url := awaitEvent(t, recorded, "the startup call's mock"); !strings.HasSuffix(url, "/startup") {
		t.Fatalf("the mock recorded is for %q, want the startup call's", url)
	}
	awaitEvent(t, done, "handleConnection returning")
}

// A request of the application's in flight across a recording's stop (an
// attached app goes on as a recording stops) is no request of the recording
// the agent makes next. The stop's graceful shutdown notes it, and the proxy's
// Record gives it up as the next recording starts, if nothing ended the last
// one before. Still open, it held every leftover of the next recording's
// requests, and kept in it, it was recorded there with them.
func TestARequestInFlightAcrossAStopIsGivenUpAsTheNextRecordingStarts(t *testing.T) {
	mgr := syncMock.Get()
	mgr.StartRecordSession() // what an earlier test's recording left
	t.Cleanup(func() {
		// A new recording's holds session, then none: the stop's ask below
		// is not left stopping the next test's holds.
		mgr.SetOutputChannel(make(chan *models.Mock))
		mgr.SetOutputChannel(nil)
	})
	inFlight := mgr.OpenWindow(time.Now().Add(-time.Second), nil)
	t.Cleanup(inFlight.Close)

	cfg := config.New()
	cfg.Agent.Mode = models.MODE_RECORD
	p := New(zap.NewNop(), fakeDestInfo{dest: ipv4Dest(t, keepAliveHTTPUpstream(t))}, cfg)
	if err := p.InitIntegrations(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := p.SetGracefulShutdown(context.Background()); err != nil { // the recording's stop
		t.Fatal(err)
	}
	if err := p.Record(context.Background(), make(chan *models.Mock, 16), models.OutgoingOptions{DisableMysqlAutoDetect: true}); err != nil {
		t.Fatal(err)
	}
	if inFlight.Keep() {
		t.Fatal("the next recording kept a request in flight across the last recording's stop")
	}
}

// When a recording client reconnects to a still-running agent (for example,
// k8s-proxy restarting mid-recording on a sidecar pod), Proxy.Record starts a
// new recording session with a fresh mock output channel. An application
// connection that stayed open across the reconnect never re-enters
// handleConnection, so its parser keeps emitting through syncMock.Get();
// Proxy.Record must rewire syncMock.Get()'s output channel to the new stream
// so post-reconnect mocks reach the new recording instead of leaking onto the
// previous session's dead channel.
func TestARecordingAfterReconnectDeliversMocksFromAnExistingConnection(t *testing.T) {
	mgr := syncMock.Get()
	t.Cleanup(func() { mgr.SetOutputChannel(nil) })

	upstream := keepAliveHTTPUpstream(t)
	cfg := config.New()
	cfg.Agent.Mode = models.MODE_RECORD
	p := New(zap.NewNop(), fakeDestInfo{dest: ipv4Dest(t, upstream)}, cfg)
	if err := p.InitIntegrations(context.Background()); err != nil {
		t.Fatal(err)
	}

	mocks1 := make(chan *models.Mock, 16)
	if err := p.Record(context.Background(), mocks1, models.OutgoingOptions{DisableMysqlAutoDetect: true}); err != nil {
		t.Fatal(err)
	}

	// One persistent keep-alive connection stays open across the reconnect.
	app, src := acceptedConn(t)
	br := bufio.NewReader(app)
	done := make(chan error, 1)
	go func() { done <- p.handleConnection(context.Background(), src) }()

	if body := awaitEvent(t, get(app, br, "/before-reconnect", false), "before-reconnect call"); body != "/before-reconnect" {
		t.Fatalf("before-reconnect call got %q", body)
	}
	select {
	case m := <-mocks1:
		if m.Spec.HTTPReq == nil || !strings.HasSuffix(m.Spec.HTTPReq.URL, "/before-reconnect") {
			t.Fatalf("first recording got unexpected mock %+v", m.Spec.HTTPReq)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for /before-reconnect mock on first recording")
	}

	// Simulate the recording client stopping and a replacement opening a new
	// /agent/outgoing stream with a fresh channel while the app's connection
	// stays open.
	if err := p.SetGracefulShutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	mocks2 := make(chan *models.Mock, 16)
	if err := p.Record(context.Background(), mocks2, models.OutgoingOptions{DisableMysqlAutoDetect: true}); err != nil {
		t.Fatal(err)
	}

	if body := awaitEvent(t, get(app, br, "/after-reconnect", true), "after-reconnect call"); body != "/after-reconnect" {
		t.Fatalf("after-reconnect call got %q", body)
	}
	_ = app.Close()
	_ = awaitEvent(t, done, "handleConnection returning")

	select {
	case m := <-mocks2:
		if m.Spec.HTTPReq == nil || !strings.HasSuffix(m.Spec.HTTPReq.URL, "/after-reconnect") {
			t.Fatalf("reconnected recording got unexpected mock %+v", m.Spec.HTTPReq)
		}
	case leaked := <-mocks1:
		url := "<non-http>"
		if leaked != nil && leaked.Spec.HTTPReq != nil {
			url = leaked.Spec.HTTPReq.URL
		}
		t.Fatalf("post-reconnect mock %q leaked onto the previous recording's output channel instead of the reconnected stream", url)
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for /after-reconnect mock on reconnected recording")
	}
}
