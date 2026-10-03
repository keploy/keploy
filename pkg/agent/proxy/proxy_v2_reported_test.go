package proxy

import (
	"context"
	"errors"
	"fmt"
	"net"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/agent/proxy/supervisor"
	syncMock "go.keploy.io/server/v3/pkg/agent/proxy/syncMock"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
	"golang.org/x/sync/errgroup"
)

// stoppingParser is a V2 parser whose recording stops at once with err.
type stoppingParser struct {
	stubParser
	err error
}

func (p stoppingParser) RecordOutgoing(context.Context, *integrations.RecordSession) error {
	return p.err
}

// A parser that stops a connection's recording and has already said why, at
// WARN and rate-limited across connections (one fault on hundreds of pooled
// connections logs one line, with a count), wraps supervisor.ErrReported. The
// dispatcher must not log that parser's retirement again at WARN, once per
// connection, or the fault logs hundreds of lines after all. Every other
// parser error it still logs at WARN. Only that line goes: every retirement
// still logs the passthrough fallback at Debug with its error, and still
// leaves out the test cases the connection carries from there, so none is
// saved without its mocks.
func TestRecordViaSupervisorDoesNotWarnAgainAboutAStopTheParserReported(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name  string
		err   error
		warns int
	}{
		{"a stop the parser reported", fmt.Errorf("framing lost: %w", supervisor.ErrReported), 0},
		{"any other parser error", errors.New("decode failed"), 1},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			core, logs := observer.New(zapcore.DebugLevel)
			mgr := syncMock.New(nil)
			ctx, cancel := context.WithCancel(syncMock.NewContext(context.Background(), mgr))
			defer cancel()

			clientApp, srcConn := net.Pipe()
			dstConn, destSvc := net.Pipe()
			started := time.Now()
			p := &Proxy{recordBufferCap: 1 << 20, recordBufferQueueSize: 64, recordBufferStallGrace: 2 * time.Second}
			done := make(chan struct{})
			go func() {
				defer close(done)
				_ = p.recordViaSupervisor(ctx, srcConn, dstConn, stoppingParser{err: c.err}, "test",
					make(chan *models.Mock, 8), &errgroup.Group{}, zap.New(core), 1, 2, models.OutgoingOptions{})
			}()

			fallback := func() *observer.ObservedLogs {
				return logs.FilterMessage("parser supervisor triggered passthrough fallback; relay continues raw forwarding until peer close")
			}
			retired := func() bool { return fallback().Len() > 0 }
			// The span is opened just after the fallback line is logged.
			spanOpened := func() bool { recorded, _, _ := mgr.OrphanRangeCount(); return recorded > 0 }
			deadline := time.Now().Add(5 * time.Second)
			for !(retired() && spanOpened()) && time.Now().Before(deadline) {
				time.Sleep(5 * time.Millisecond)
			}
			// The span stays open after the retirement, while the connection can
			// still carry traffic: both peers are open here. A span closed at the
			// retirement, or soon after it, would leave out nothing the connection
			// carries from there on. Only a connection idle for UnrecordedIdleGrace
			// closes it, and this one has carried nothing since it started, so the
			// span is sampled as late as that allows: one UnrecordedIdleCheck short
			// of UnrecordedIdleGrace after the start. A span ended shortly after
			// the retirement, or followed with a shorter grace, is closed by then.
			time.Sleep(time.Until(started.Add(syncMock.UnrecordedIdleGrace - syncMock.UnrecordedIdleCheck)))
			sampled := time.Now()
			stillOpen, _ := mgr.WasMockOrphanedInWindow(sampled, sampled)
			idleFor := sampled.Sub(started)
			// The connection ends: the relay forwards until a peer closes.
			_ = clientApp.Close()
			_ = destSvc.Close()
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("recordViaSupervisor did not return after both peers closed")
			}
			_ = srcConn.Close()
			_ = dstConn.Close()

			if !retired() {
				t.Fatal("the parser was not retired")
			}
			got := logs.FilterMessage("parser retired; this connection can no longer be recorded").FilterLevelExact(zapcore.WarnLevel).Len()
			if got != c.warns {
				t.Fatalf("%d WARN lines for the parser's retirement, want %d", got, c.warns)
			}
			// The Debug line says why, for this stop too.
			if n := fallback().FilterLevelExact(zapcore.DebugLevel).FilterField(zap.Error(c.err)).Len(); n != 1 {
				t.Fatalf("%d Debug passthrough-fallback lines carrying the parser's error %q, want 1", n, c.err)
			}
			// What the connection carries from the retirement on is left out:
			// one span, opened at the retirement.
			if recorded, _, _ := mgr.OrphanRangeCount(); recorded != 1 {
				t.Fatalf("%d spans left out, want 1: the test cases recorded over this connection after its parser retired are saved without their mocks", recorded)
			}
			// Sampled at UnrecordedIdleGrace or later, the span may have closed
			// for idleness, as it should, so a closed one shows nothing wrong and
			// the check below cannot be made. Fail rather than pass unchecked.
			if idleFor >= syncMock.UnrecordedIdleGrace {
				t.Fatalf("the span was sampled %v after the connection started, not under UnrecordedIdleGrace (%v): the retirement, or the scheduler, took that long, so whether the span stays open after it while both peers are open went unchecked", idleFor, syncMock.UnrecordedIdleGrace)
			}
			if !stillOpen {
				t.Fatalf("the span left out was closed by %v after the connection started, with both peers open and the connection idle for less than %v: the test cases recorded over it after its parser retired are saved without their mocks", idleFor, syncMock.UnrecordedIdleGrace)
			}
		})
	}
}
