package proxy

import (
	"context"
	"net"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/connseq"
	"go.keploy.io/server/v3/pkg/agent/proxy/fakeconn"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/agent/proxy/util"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
	"golang.org/x/sync/errgroup"
)

// orderProbe asks, for the first client chunk, whether the destination's
// stream is through every chunk captured before it, waiting on what the
// session gives, and says what it found.
type orderProbe struct {
	stubParser
	found chan string
}

func (o *orderProbe) IsV2() bool { return true }

func (o *orderProbe) RecordOutgoing(ctx context.Context, s *integrations.RecordSession) error {
	c, err := s.V2.ClientStream.ReadChunk()
	if err != nil {
		o.found <- "the client's stream ended: " + err.Error()
		<-ctx.Done()
		return ctx.Err()
	}
	for {
		end, ok, changed := s.V2.StreamedBefore(fakeconn.FromDest, c.ConnSeq)
		switch {
		case ok && end == 0:
			o.found <- ""
		case ok:
			o.found <- "the destination's stream is through more than it carried"
		case changed == nil:
			o.found <- "the session cannot say how far the relay has put its streams: recordViaSupervisor did not give it the relay's order"
		default:
			select {
			case <-changed:
				continue
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		<-ctx.Done()
		return ctx.Err()
	}
}

// The relay owns the capture order, and the session a parser gets says what
// the relay says of it (Session.Order): with nothing captured from the
// destination, its stream is through every chunk of it captured before the
// client's first.
func TestRecordViaSupervisorGivesTheSessionTheRelaysOrder(t *testing.T) {
	t.Parallel()
	tcpPair := func() (dialed, accepted net.Conn) {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatalf("listen: %v", err)
		}
		defer func() { _ = ln.Close() }()
		acc := make(chan net.Conn, 1)
		go func() {
			c, _ := ln.Accept()
			acc <- c
		}()
		d, err := net.Dial("tcp", ln.Addr().String())
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		a := <-acc
		if a == nil {
			t.Fatal("accept failed")
		}
		t.Cleanup(func() { _ = d.Close(); _ = a.Close() })
		return d, a
	}
	clientApp, rawSrc := tcpPair()
	dstConn, _ := tcpPair()
	probe := &orderProbe{found: make(chan string, 1)}
	p := &Proxy{recordBufferCap: 64 * 1024, recordBufferQueueSize: 64, recordBufferStallGrace: 2 * time.Second}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() {
		_ = p.recordViaSupervisor(ctx, &util.Conn{Conn: rawSrc, Reader: rawSrc, Logger: zap.NewNop()},
			connseq.NewUpstream(dstConn), probe, "test", make(chan *models.Mock, 8), &errgroup.Group{},
			zap.NewNop(), 1, 2, models.OutgoingOptions{})
	}()
	if _, err := clientApp.Write([]byte("hello")); err != nil {
		t.Fatal(err)
	}
	select {
	case why := <-probe.found:
		if why != "" {
			t.Fatal(why)
		}
	case <-time.After(10 * time.Second): // a hang guard, not a verdict
		t.Fatal("the parser never learned how far the destination's stream is")
	}
}
