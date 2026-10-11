package proxy

import (
	"context"
	"net"
	"sync"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/connseq"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/agent/proxy/relay"
	syncMock "go.keploy.io/server/v3/pkg/agent/proxy/syncMock"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
	"golang.org/x/sync/errgroup"
)

// heldParser is a V2 parser held behind its connection's traffic: it reads
// nothing until released, then reads every client chunk as it comes.
type heldParser struct {
	stubParser
	release chan struct{}
	mu      sync.Mutex
	read    int
}

func (p *heldParser) IsV2() bool { return true }

func (p *heldParser) RecordOutgoing(ctx context.Context, s *integrations.RecordSession) error {
	select {
	case <-p.release:
	case <-ctx.Done():
		return ctx.Err()
	}
	for {
		if _, err := s.V2.ClientStream.ReadChunk(); err != nil {
			<-ctx.Done()
			return ctx.Err()
		}
		p.mu.Lock()
		p.read++
		p.mu.Unlock()
	}
}

func (p *heldParser) chunksRead() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.read
}

// startHeldConnection runs recordViaSupervisor for a connection whose parser
// is held, on a proxy whose capture watermark is added to mgr, and returns the
// application's end and the parser.
func startHeldConnection(t *testing.T, mgr *syncMock.SyncMockManager) (net.Conn, *heldParser) {
	t.Helper()
	clientApp, srcConn := net.Pipe()
	dstConn, destSvc := net.Pipe()
	// The proxy's watermark on mgr, not on the process's manager.
	w := relay.NewWatermark()
	p := &Proxy{recordBufferQueueSize: 64, capture: w}
	t.Cleanup(mgr.AddWatermark(proxyWatermark{Watermark: w, mgr: mgr}))

	go func() {
		buf := make([]byte, 4096)
		for {
			if _, err := destSvc.Read(buf); err != nil {
				return
			}
		}
	}()
	parser := &heldParser{release: make(chan struct{})}
	ctx, cancel := context.WithCancel(syncMock.NewContext(context.Background(), mgr))
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = p.recordViaSupervisor(ctx, srcConn, connseq.NewUpstream(dstConn), parser, "test",
			make(chan *models.Mock, 8), &errgroup.Group{}, zap.NewNop(), 1, 2, models.OutgoingOptions{})
	}()
	t.Cleanup(func() {
		cancel()
		_ = clientApp.Close()
		_ = destSvc.Close()
		<-done
		_ = srcConn.Close()
		_ = dstConn.Close()
	})
	waitUntilT(t, 30*time.Second, "the proxy to follow the connection", func() bool { return w.Following() == 1 })
	return clientApp, parser
}

// P1 (stress audit rank 2): proxy mode had no capture watermark, so the agent
// could not say whether a parser behind its connection's traffic still held
// something from before a recording's stop (/record/pending answered 501, and
// the stop went by 3 s of quiet), and a test case was streamed as it completed,
// before a parser behind it could fail on traffic inside it. With the
// watermark, a held parser keeps the backlog pending and its test cases held;
// once it has caught up, neither.
func TestCaptureWatermark_AParserBehindItsConnectionHoldsTheRecording(t *testing.T) {
	mgr := syncMock.New(nil)
	app, parser := startHeldConnection(t, mgr)

	start := time.Now()
	if _, err := app.Write([]byte("GET /a HTTP/1.1\r\n\r\n")); err != nil {
		t.Fatal(err)
	}
	stop := time.Now() // the application has been answered by the proxy's forward

	st := mgr.BacklogBefore(stop)
	if !st.Known {
		t.Fatal("the agent cannot tell what it still holds: proxy mode has no capture watermark")
	}
	if !st.Pending {
		t.Fatal("the agent says it holds nothing from before the stop while a parser has not read a request its connection carried")
	}
	if st.WaitingOn == "" {
		t.Fatal("a pending backlog does not say what it waits on")
	}
	w := mgr.Watermark()
	if w == nil || w.Settled("", "", start, stop) {
		t.Fatal("a test case over a request its connection's parser had not read is final: it would be streamed before a failure of that parser could leave it out")
	}
	p0 := st.Progress

	close(parser.release)
	waitUntilT(t, 30*time.Second, "the parser to catch up", func() bool { return parser.chunksRead() >= 1 })
	waitUntilT(t, 30*time.Second, "the backlog to clear", func() bool {
		st = mgr.BacklogBefore(stop)
		return !st.Pending
	})
	if !w.Settled("", "", start, stop) {
		t.Fatal("a test case is still held once the parser has read everything and waits for more")
	}
	if st.Progress == p0 {
		t.Fatal("the backlog's progress did not move as the parser read")
	}
}
