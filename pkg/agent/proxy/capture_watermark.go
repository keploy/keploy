package proxy

import (
	"fmt"
	"os"
	"strings"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/relay"
	syncMock "go.keploy.io/server/v3/pkg/agent/proxy/syncMock"
	"go.uber.org/zap"
)

// capture_watermark.go gives proxy mode the capture watermark the rest of the
// recording works from (syncMock settle.go): what the parsers of the
// connections it records have got through (relay.Watermark). With it,
//
//   - the recorder's stream holds each test case until its verdict is final
//     (syncMock.TestCaseHold): a parser behind its connection's traffic can
//     still fail on an exchange inside a test case that has completed, and a
//     test case streamed at once was then saved without its mocks;
//   - a recording's stop asks the agent whether it may still hand over
//     something from before the stop (/agent/record/pending), and drains its
//     streams until it may not, instead of until they fall quiet for a few
//     seconds: a parser behind the traffic can be quiet for longer than that.

// proxyWatermark is proxy mode's syncMock.Watermark, Backlog, Explainer,
// Progressor and PendingExplainer: its relays' capture watermark, and the
// recording's manager holding no mock of the test case's window. The manager
// hands mocks to the CLI's stream, and a mock it drops there is told
// (WasMockDroppedForTC, OnDrop) before it stops being pending (PendingIn): a
// test case is checked only once its mocks are handed on or dropped. The
// enterprise proxyless agent pairs its capture watermark the same way.
type proxyWatermark struct {
	*relay.Watermark
	mgr *syncMock.SyncMockManager
}

var (
	_ syncMock.Watermark        = proxyWatermark{}
	_ syncMock.Backlog          = proxyWatermark{}
	_ syncMock.Explainer        = proxyWatermark{}
	_ syncMock.Progressor       = proxyWatermark{}
	_ syncMock.PendingExplainer = proxyWatermark{}
)

func (w proxyWatermark) Settled(namespace, pod string, start, end time.Time) bool {
	return w.Watermark.Settled(namespace, pod, start, end) && !w.mgr.PendingIn(start, end)
}

// HeldBy is the capture's blocker, then a mock of the window the manager still
// holds.
func (w proxyWatermark) HeldBy(namespace, pod string, start, end time.Time, fence uint64) fmt.Stringer {
	if b := w.Watermark.HeldBy(namespace, pod, start, end, fence); b != nil {
		return b
	}
	if w.mgr.PendingIn(start, end) {
		return heldByManager{}
	}
	return nil
}

type heldByManager struct{}

func (heldByManager) String() string {
	return "the recording's manager still holds a mock of its window"
}

// addCaptureWatermark adds the proxy's capture watermark to the recording's
// manager, beside any an embedder sets (SyncMockManager.AddWatermark), once.
func (p *Proxy) addCaptureWatermark() {
	p.captureWatermarkOnce.Do(func() {
		if p.capture == nil {
			p.capture = relay.NewWatermark()
		}
		mgr := syncMock.Get()
		p.removeCaptureWatermark = mgr.AddWatermark(proxyWatermark{Watermark: p.capture, mgr: mgr})
	})
}

// debugParserLagEnv makes every recorded connection's parser run this far
// behind its traffic (relay.Config.ReaderLag): a Go duration ("5s"). It exists
// for end-to-end tests of what a lagging capture costs a recording; unset, the
// proxy adds no lag.
const debugParserLagEnv = "KEPLOY_DEBUG_PARSER_LAG"

// debugParserLag reads debugParserLagEnv, and says so at WARN when it is set:
// a lag no one asked for must never go unnoticed.
func debugParserLag(logger *zap.Logger) time.Duration {
	v := strings.TrimSpace(os.Getenv(debugParserLagEnv))
	if v == "" {
		return 0
	}
	d, err := time.ParseDuration(v)
	if err != nil || d <= 0 {
		if logger != nil {
			logger.Warn("ignoring "+debugParserLagEnv+": not a positive Go duration",
				zap.String("value", v), zap.Error(err))
		}
		return 0
	}
	if logger != nil {
		logger.Warn(debugParserLagEnv+" is set: every recorded connection's parser runs this far behind its traffic. It is a test hook; unset it for real recordings",
			zap.Duration("lag", d))
	}
	return d
}
