//go:build linux

package proxy

import (
	"context"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/connseq"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/agent/proxy/util"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
	"golang.org/x/sync/errgroup"
)

// destEndProbe is a V2 parser that reads the destination's stream to its end,
// and keeps what it carried and whether the session says it ended where the
// client stopped taking it.
type destEndProbe struct {
	stubParser
	mu      sync.Mutex
	carried []byte
	stopped bool
	ended   chan struct{}
}

func (p *destEndProbe) IsV2() bool { return true }

func (p *destEndProbe) RecordOutgoing(ctx context.Context, s *integrations.RecordSession) error {
	go func() { _, _ = io.Copy(io.Discard, s.V2.ClientStream) }()
	for {
		c, err := s.V2.DestStream.ReadChunk()
		if err != nil {
			p.mu.Lock()
			p.stopped = s.V2.EndedWhereClientStopped()
			p.mu.Unlock()
			close(p.ended)
			<-ctx.Done()
			return ctx.Err()
		}
		p.mu.Lock()
		p.carried = append(p.carried, c.Bytes...)
		p.mu.Unlock()
	}
}

// What the proxy hands the relay as it records a connection: a client that
// closes its socket while its request is in flight, and a destination that
// answers once it has the FIN. The relay forwards the answer, the client
// refuses it, and the parser is not given it: its destination stream ends
// there, and the session says it ended where the client stopped taking it
// (relay.Config.ClientStopped, wired to Session.MarkEndedWhereClientStopped),
// which the HTTP recorder reads to leave out, and say so, a response cut
// there.
func TestRecordViaSupervisor_TheDestStreamEndsWhereTheClientStopped(t *testing.T) {
	t.Parallel()
	pair := func() (net.Conn, net.Conn) {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = ln.Close() }()
		got := make(chan net.Conn, 1)
		go func() { c, _ := ln.Accept(); got <- c }()
		d, err := net.Dial("tcp", ln.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		a := <-got
		if a == nil {
			t.Fatal("accept failed")
		}
		t.Cleanup(func() { _ = d.Close(); _ = a.Close() })
		return d, a
	}
	clientApp, rawSrc := pair()
	dstConn, destSvc := pair()
	// Exactly what handleConnection builds before dispatch.
	srcConn := &util.Conn{Conn: rawSrc, Reader: rawSrc, Logger: zap.NewNop()}
	p := &Proxy{recordBufferCap: 64 * 1024, recordBufferQueueSize: 64, recordBufferStallGrace: 2 * time.Second}
	probe := &destEndProbe{ended: make(chan struct{})}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = p.recordViaSupervisor(ctx, srcConn, connseq.NewUpstream(dstConn), probe, "test",
			make(chan *models.Mock, 8), &errgroup.Group{}, zap.NewNop(), 1, 2, models.OutgoingOptions{})
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})

	const request, answer = "a request", "the answer, once the client had closed"
	if _, err := clientApp.Write([]byte(request)); err != nil {
		t.Fatal(err)
	}
	_ = destSvc.SetReadDeadline(time.Now().Add(10 * time.Second))
	if _, err := io.ReadFull(destSvc, make([]byte, len(request))); err != nil {
		t.Fatalf("the destination did not get the request: %v", err)
	}
	if err := clientApp.Close(); err != nil {
		t.Fatal(err)
	}
	if rest, err := io.ReadAll(destSvc); err != nil || len(rest) != 0 {
		t.Fatalf("the destination read %d more bytes, %v, before the FIN", len(rest), err)
	}
	if _, err := destSvc.Write([]byte(answer)); err != nil {
		t.Fatal(err)
	}
	_ = destSvc.Close()
	select {
	case <-probe.ended:
	case <-time.After(10 * time.Second):
		t.Fatal("the parser's destination stream did not end")
	}
	probe.mu.Lock()
	defer probe.mu.Unlock()
	if len(probe.carried) != 0 {
		t.Fatalf("the parser was given %q, which the client never took", probe.carried)
	}
	if !probe.stopped {
		t.Fatal("the session does not say the destination's stream ended where the client stopped taking it")
	}
}
