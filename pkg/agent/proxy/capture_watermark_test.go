package proxy

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/connseq"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/agent/proxy/relay"
	syncMock "go.keploy.io/server/v3/pkg/agent/proxy/syncMock"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
	"golang.org/x/sync/errgroup"
)

// heldParser is a V2 parser held behind its connection's traffic: it waits
// for the client's chunks, holds on the first one it takes until released,
// then reads every client chunk as it comes.
type heldParser struct {
	stubParser
	release chan struct{}
	// took is closed once the parser has taken the first chunk: teed to it,
	// so stamped and told of by the relay before.
	took chan struct{}
	mu   sync.Mutex
	read int
}

func (p *heldParser) IsV2() bool { return true }

func (p *heldParser) RecordOutgoing(ctx context.Context, s *integrations.RecordSession) error {
	for first := true; ; first = false {
		if _, err := s.V2.ClientStream.ReadChunk(); err != nil {
			<-ctx.Done()
			return ctx.Err()
		}
		if first {
			close(p.took)
			select {
			case <-p.release:
			case <-ctx.Done():
				return ctx.Err()
			}
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
	parser := &heldParser{release: make(chan struct{}), took: make(chan struct{})}
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
// watermark, a parser still on a request its connection carried keeps the
// backlog pending and its test cases held; one waiting for more, with nothing
// carried that it has not read, neither.
func TestCaptureWatermark_AParserBehindItsConnectionHoldsTheRecording(t *testing.T) {
	mgr := syncMock.New(nil)
	app, parser := startHeldConnection(t, mgr)

	// Nothing carried yet, and the parser waits for it: nothing is held,
	// over the same bound the rest of the test asks about.
	var p0 uint64
	waitUntilT(t, 30*time.Second, "the idle connection's backlog to read as clear", func() bool {
		st := mgr.BacklogBefore(time.Now().Add(time.Minute))
		p0 = st.Progress
		return st.Known && !st.Pending
	})

	start := time.Now()
	if _, err := app.Write([]byte("GET /a HTTP/1.1\r\n\r\n")); err != nil {
		t.Fatal(err)
	}
	// net.Pipe's Write returns once the relay's Read has the bytes, before
	// the relay has stamped the chunk and told of it: the parser taking it
	// comes after both. The bound is later than any stamp it can take.
	waitFor(t, parser.took, "the parser never took the request")
	stop := time.Now().Add(time.Minute)

	st := mgr.BacklogBefore(stop)
	if !st.Known {
		t.Fatal("the agent cannot tell what it still holds: proxy mode has no capture watermark")
	}
	if !st.Pending {
		t.Fatal("the agent says it holds nothing from before the stop while a parser is still on a request its connection carried")
	}
	if st.WaitingOn == "" {
		t.Fatal("a pending backlog does not say what it waits on")
	}
	w := mgr.Watermark()
	if w == nil || w.Settled("", "", start, stop) {
		t.Fatal("a test case over a request its connection's parser is still on is final: it would be streamed before a failure of that parser could leave it out")
	}

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

// A test case's verdict is final only once the recording's mock manager holds
// no mock of its window either: every parser may be past it (the relay
// watermark settles it) while the manager still has a mock of it buffered,
// not yet handed on with its test case.
func TestCaptureWatermark_AMockTheManagerStillHoldsKeepsTheWindowOpen(t *testing.T) {
	mgr := syncMock.New(zap.NewNop())
	mgr.SetFirstRequestSignaled() // mocks now wait in the buffer for their window
	w := proxyWatermark{Watermark: relay.NewWatermark(), mgr: mgr}

	start := time.Now().Add(-time.Second)
	end := start.Add(100 * time.Millisecond)
	if !w.Settled("", "", start, end) {
		t.Fatal("setup: a window nothing holds is not settled")
	}
	mgr.AddMock(&models.Mock{Kind: models.HTTP, Spec: models.MockSpec{ReqTimestampMock: start.Add(10 * time.Millisecond)}})
	if w.Settled("", "", start, end) {
		t.Fatal("a window whose mock the manager still holds was settled: its test case would go out before its mock")
	}
	if w.HeldBy("", "", start, end, 0) == nil {
		t.Fatal("nothing named as holding a window the manager still holds a mock of")
	}
}

// returnsAfterOneParser reads the client's first chunk and, once released,
// returns on its own, with the requests queued behind it never read.
type returnsAfterOneParser struct {
	stubParser
	read    chan struct{}
	release chan struct{}
}

func (returnsAfterOneParser) IsV2() bool { return true }

func (p returnsAfterOneParser) RecordOutgoing(ctx context.Context, s *integrations.RecordSession) error {
	if _, err := s.V2.ClientStream.ReadChunk(); err != nil {
		return err
	}
	close(p.read)
	select {
	case <-p.release:
	case <-ctx.Done():
	}
	return nil
}

// A test case held for a request its parser never read is let go only once
// that request is left out (parserReads, as the parser's Run returns): let go
// before, it was checked against no span and saved without its mock. The
// dispatcher's log of the return is held, so the stretch between the return
// and the requests left out is open for as long as the test looks.
func TestCaptureWatermark_ATestCaseIsLetGoOnlyOnceWhatTheParserNeverReadIsLeftOut(t *testing.T) {
	mgr := syncMock.New(nil)
	clientApp, srcConn := net.Pipe()
	dstConn, destSvc := net.Pipe()
	w := relay.NewWatermark()
	p := &Proxy{recordBufferQueueSize: 64, capture: w}
	t.Cleanup(mgr.AddWatermark(proxyWatermark{Watermark: w, mgr: mgr}))
	// forwarded counts the bytes the relay forwarded on: it forwards a
	// chunk only once it has stamped it and told of it.
	var forwarded atomic.Int64
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := destSvc.Read(buf)
			forwarded.Add(int64(n))
			if err != nil {
				return
			}
		}
	}()
	logged := newHeldLog(zapcore.DebugLevel, "V2 parser recorded outgoing message successfully")
	defer logged.Release()
	parser := returnsAfterOneParser{read: make(chan struct{}), release: make(chan struct{})}
	ctx, cancel := context.WithCancel(syncMock.NewContext(context.Background(), mgr))
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = p.recordViaSupervisor(ctx, srcConn, connseq.NewUpstream(dstConn), parser, "test",
			make(chan *models.Mock, 8), &errgroup.Group{}, zap.New(logged), 1, 2, models.OutgoingOptions{})
	}()
	t.Cleanup(func() {
		logged.Release()
		cancel()
		_ = clientApp.Close()
		_ = destSvc.Close()
		<-done
		_ = srcConn.Close()
		_ = dstConn.Close()
	})
	waitUntilT(t, 30*time.Second, "the proxy to follow the connection", func() bool { return w.Following() == 1 })

	if _, err := clientApp.Write([]byte("GET /one HTTP/1.1\r\n\r\n")); err != nil {
		t.Fatal(err)
	}
	waitFor(t, parser.read, "the parser never read the first request")
	start := time.Now()
	two := []byte("GET /two HTTP/1.1\r\n\r\n")
	if _, err := clientApp.Write(two); err != nil { // never read
		t.Fatal(err)
	}
	// net.Pipe's Write returns once the relay's Read has the bytes, before
	// it has stamped and told of the chunk; its forward comes after both.
	waitUntilT(t, 30*time.Second, "the relay to forward the second request", func() bool {
		return forwarded.Load() >= int64(len("GET /one HTTP/1.1\r\n\r\n")+len(two))
	})
	end := time.Now().Add(time.Minute)
	wm := mgr.Watermark()
	if wm.Settled("", "", start, end) {
		t.Fatal("setup: a test case over a request the parser has not read is final")
	}

	close(parser.release)
	waitFor(t, logged.entered, "the dispatcher never logged the parser's return")
	if wm.Settled("", "", start, end) {
		if left, _ := mgr.WasMockOrphanedInWindow(start, end); !left {
			t.Fatal("a test case over a request the parser never read was let go before that request was left out: it is saved without its mock")
		}
	}
	logged.Release()
	waitUntilT(t, 30*time.Second, "the test case to be let go", func() bool { return wm.Settled("", "", start, end) })
	if left, _ := mgr.WasMockOrphanedInWindow(start, end); !left {
		t.Fatal("a test case over a request the parser never read is not left out")
	}
}

// KEPLOY_DEBUG_PARSER_GATE numbers the connections the proxy records from 1,
// and opens the n-th one's gate on the event of a file named n, or "all",
// being created in its directory: a test orders parsers by events of its
// choosing. A file there before the gate is asked for opens it at once.
func TestParserGateOpensTheNthConnectionOnItsFile(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(debugParserGateEnv, dir)
	touch := func(name string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	touch("3") // before any gate is asked for
	p := &Proxy{parserGates: debugParserGate(zap.NewNop())}
	if p.parserGates == nil || p.parserGates.dir != dir {
		t.Fatalf("the gates are %+v, want the directory %q", p.parserGates, dir)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	first := p.parserGate(ctx, zap.NewNop(), nil)
	second := p.parserGate(ctx, zap.NewNop(), nil)
	third := p.parserGate(ctx, zap.NewNop(), nil)
	<-third
	touch("2")
	<-second
	select {
	case <-first:
		t.Fatal("the first connection's gate opened on another's file")
	default:
	}
	touch("all")
	<-first
	if (&Proxy{}).parserGate(ctx, nil, nil) != nil {
		t.Fatal("a proxy with no gate directory gated a connection")
	}
	p.parserGates.close()
}

// A gate directory the proxy cannot watch opens no gate, and says so at
// ERROR: a gate that opened anyway let the parsers through in no order, and a
// test that ordered them by their gates passed without the order it set up.
func TestParserGateThatCannotBeWatchedOpensNone(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "never-made")
	t.Setenv(debugParserGateEnv, dir)
	core, logs := observer.New(zapcore.ErrorLevel)
	p := &Proxy{parserGates: debugParserGate(zap.New(core))}
	defer p.parserGates.close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	gate := p.parserGate(ctx, zap.NewNop(), nil)
	select {
	case <-gate:
		t.Fatal("a gate opened on a directory that cannot be watched")
	default:
	}
	if n := logs.FilterMessage(cannotWatchGates).Len(); n != 1 {
		t.Fatalf("%d ERROR lines that the gates cannot be watched, want 1", n)
	}
}
