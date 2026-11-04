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
// and keeps what it carried, whether the session says it ended where the
// client stopped taking it, and whether it says the client had ended its side
// before the destination's end reached it.
type destEndProbe struct {
	stubParser
	mu      sync.Mutex
	carried []byte
	stopped bool
	before  bool
	// readAfter: the session says the client read after its FIN.
	readAfter bool
	ended     chan struct{}
}

func (p *destEndProbe) IsV2() bool { return true }

func (p *destEndProbe) RecordOutgoing(ctx context.Context, s *integrations.RecordSession) error {
	go func() { _, _ = io.Copy(io.Discard, s.V2.ClientStream) }()
	for {
		c, err := s.V2.DestStream.ReadChunk()
		if err != nil {
			p.mu.Lock()
			p.stopped = s.V2.EndedWhereClientStopped()
			p.before = s.V2.ClientEndedBeforeDest()
			p.readAfter = s.V2.ClientReadAfterFIN()
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
// which the HTTP recorder reads to record a response cut there as far as the
// client took it, with a stopped_reading fault, unless the client's end came
// first and records no fault (then it is left out).
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

// What the proxy hands the relay as it records a connection: the relay's look
// at the client's socket as it reads the destination's end is wired to the
// session (relay.Config.ClientEndedBeforeDest, wired to
// Session.MarkClientEndedBeforeDest), which the HTTP recorder reads to judge a
// call the client closed partway by the client's own end, whichever end the
// relay read first. A client that closed before the destination's end, with
// nothing sent after its FIN, is said to have ended first; one that closed
// only once it had read the destination's end is not.
func TestRecordViaSupervisor_TheSessionSaysTheClientEndedBeforeTheDestinationsEnd(t *testing.T) {
	for _, clientFirst := range []bool{true, false} {
		t.Run(map[bool]string{true: "the client closes first", false: "the destination closes first"}[clientFirst], func(t *testing.T) {
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
			const request = "a request"
			if _, err := clientApp.Write([]byte(request)); err != nil {
				t.Fatal(err)
			}
			_ = destSvc.SetReadDeadline(time.Now().Add(10 * time.Second))
			if _, err := io.ReadFull(destSvc, make([]byte, len(request))); err != nil {
				t.Fatalf("the destination did not get the request: %v", err)
			}
			if clientFirst {
				if err := clientApp.Close(); err != nil {
					t.Fatal(err)
				}
				if rest, err := io.ReadAll(destSvc); err != nil || len(rest) != 0 {
					t.Fatalf("the destination read %d more bytes, %v, before the FIN", len(rest), err)
				}
				_ = destSvc.Close()
			} else {
				_ = destSvc.Close()
				_ = clientApp.SetReadDeadline(time.Now().Add(10 * time.Second))
				if rest, err := io.ReadAll(clientApp); err != nil || len(rest) != 0 {
					t.Fatalf("the client read %d bytes, %v, before the destination's FIN", len(rest), err)
				}
				_ = clientApp.Close()
			}
			select {
			case <-probe.ended:
			case <-time.After(10 * time.Second):
				t.Fatal("the parser's destination stream did not end")
			}
			probe.mu.Lock()
			defer probe.mu.Unlock()
			if probe.before != clientFirst {
				t.Fatalf("the session says the client ended its side before the destination's end reached it: %v, want %v", probe.before, clientFirst)
			}
			if probe.stopped {
				t.Fatal("the session says the client stopped taking the stream, though the destination sent nothing after the client's FIN")
			}
			if probe.readAfter {
				t.Fatal("the session says the client read after its FIN, though the destination sent nothing after it")
			}
		})
	}
}

// What the proxy hands the relay as it records a connection: the relay's word
// that the client's stack took what the destination sent after the client's
// FIN is wired to the session (relay.Config.ClientReadAfterFIN, wired to
// Session.MarkClientReadAfterFIN), which the HTTP recorder reads to judge a
// half-closed client's call by the destination's end. A client that shuts its
// sending side down and reads the answer to its end is said to read after its
// FIN, and not to have stopped taking the stream.
func TestRecordViaSupervisor_TheSessionSaysAHalfClosedClientReadAfterItsFIN(t *testing.T) {
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
	const request, answer = "a request", "the answer"
	if _, err := clientApp.Write([]byte(request)); err != nil {
		t.Fatal(err)
	}
	if err := clientApp.(*net.TCPConn).CloseWrite(); err != nil {
		t.Fatal(err)
	}
	_ = destSvc.SetReadDeadline(time.Now().Add(10 * time.Second))
	if got, err := io.ReadAll(destSvc); err != nil || string(got) != request {
		t.Fatalf("the destination read %q, %v, to the client's FIN; want %q", got, err, request)
	}
	if _, err := destSvc.Write([]byte(answer)); err != nil {
		t.Fatal(err)
	}
	_ = destSvc.Close()
	_ = clientApp.SetReadDeadline(time.Now().Add(10 * time.Second))
	if got, err := io.ReadAll(clientApp); err != nil || string(got) != answer {
		t.Fatalf("the half-closed client read %q, %v, to the destination's FIN; want %q", got, err, answer)
	}
	_ = clientApp.Close()
	select {
	case <-probe.ended:
	case <-time.After(10 * time.Second):
		t.Fatal("the parser's destination stream did not end")
	}
	probe.mu.Lock()
	defer probe.mu.Unlock()
	if !probe.readAfter {
		t.Fatal("the session does not say the half-closed client read after its FIN")
	}
	if probe.stopped {
		t.Fatal("the session says the client stopped taking the stream, though it read the answer to its end")
	}
	if string(probe.carried) != answer {
		t.Fatalf("the parser was given %q, want the answer %q", probe.carried, answer)
	}
}
