package proxy

import (
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/relay"
	syncMock "go.keploy.io/server/v3/pkg/agent/proxy/syncMock"
	"go.keploy.io/server/v3/pkg/models"
)

// Proxy mode's watermark counts the manager handing mocks on as progress of
// what holds a test case while the manager holds a mock of its window: that
// mock, queued behind others for the stream, moves as the queue does. A
// hand-on while it holds none is not what holds the test case, and does not
// move its count.
func TestProxyWatermarkCountsTheManagersHandOnAsHeldProgress(t *testing.T) {
	mgr := syncMock.New(nil)
	out := make(chan *models.Mock, 4)
	mgr.SetOutputChannel(out)
	mgr.SetFirstRequestSignaled() // mocks are buffered for their window
	w := proxyWatermark{Watermark: relay.NewWatermark(), mgr: mgr}
	t0 := time.Unix(2000, 0)
	held := &models.Mock{Name: "held"}
	held.Spec.ReqTimestampMock = t0
	mgr.AddMock(held)
	start, end := t0.Add(-time.Second), t0.Add(time.Second)
	otherStart, otherEnd := t0.Add(time.Hour), t0.Add(2*time.Hour)
	if !mgr.PendingIn(start, end) || mgr.PendingIn(otherStart, otherEnd) {
		t.Fatal("setup: want a mock pending in the window only")
	}
	before, beforeOther := w.HeldProgress("", "", start, end), w.HeldProgress("", "", otherStart, otherEnd)
	mgr.SendConfigMock(&models.Mock{Name: "dns"})
	if got := w.HeldProgress("", "", start, end); got == before {
		t.Fatal("the manager handed a mock on while it held one of the window, and the held progress did not move")
	}
	if got := w.HeldProgress("", "", otherStart, otherEnd); got != beforeOther {
		t.Fatalf("the manager handed a mock on while it held none of the window, and its held progress moved from %d to %d", beforeOther, got)
	}
}
