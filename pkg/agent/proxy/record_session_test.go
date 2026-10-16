package proxy

import (
	"bufio"
	"context"
	"strings"
	"testing"

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
