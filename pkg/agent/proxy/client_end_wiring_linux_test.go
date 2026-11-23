//go:build linux

package proxy

import (
	"context"
	"errors"
	"io"
	"net"
	"sync"
	"syscall"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/connseq"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/agent/proxy/util"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
	"golang.org/x/sync/errgroup"
	"golang.org/x/sys/unix"
)

// clientEndProbe is a V2 parser that reads the destination's stream to its
// end, and keeps what it carried and what the session says of the client's
// socket there.
type clientEndProbe struct {
	stubParser
	mu      sync.Mutex
	carried []byte
	endErr  error
	ended   chan struct{}
}

func (p *clientEndProbe) IsV2() bool { return true }

func (p *clientEndProbe) RecordOutgoing(ctx context.Context, s *integrations.RecordSession) error {
	go func() { _, _ = io.Copy(io.Discard, s.V2.ClientStream) }()
	for {
		c, err := s.V2.DestStream.ReadChunk()
		if err != nil {
			p.mu.Lock()
			p.endErr = s.V2.ClientEndError()
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

// What the proxy hands the relay as it records a connection: a half-closed
// client whose stack takes the answer, and whose application closes its socket
// on it unread. The parser is given the answer, and, at its stream's end, the
// session holds the reset that came after the client's FIN
// (relay.Config.ClientEndError, wired to Session.MarkClientEndError), which a
// recorder reads to tell an answer the client never read.
func TestRecordViaSupervisor_TheSessionHoldsTheClientsResetAfterItsFIN(t *testing.T) {
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
	probe := &clientEndProbe{ended: make(chan struct{})}
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

	const request, answer = "a request", "an answer the client's stack takes and its application never reads"
	if _, err := clientApp.Write([]byte(request)); err != nil {
		t.Fatal(err)
	}
	if err := clientApp.(*net.TCPConn).CloseWrite(); err != nil {
		t.Fatal(err)
	}
	_ = destSvc.SetReadDeadline(time.Now().Add(10 * time.Second))
	if got, err := io.ReadAll(destSvc); err != nil || string(got) != request {
		t.Fatalf("the destination read %q, %v, want the request and the FIN", got, err)
	}
	if _, err := destSvc.Write([]byte(answer)); err != nil {
		t.Fatal(err)
	}
	info := func() *unix.TCPInfo {
		rc, err := rawSrc.(*net.TCPConn).SyscallConn()
		if err != nil {
			t.Fatal(err)
		}
		var got *unix.TCPInfo
		var ierr error
		if err := rc.Control(func(fd uintptr) { got, ierr = unix.GetsockoptTCPInfo(int(fd), unix.IPPROTO_TCP, unix.TCP_INFO) }); err != nil || ierr != nil {
			t.Fatalf("TCP_INFO: %v, %v", err, ierr)
		}
		return got
	}
	for limit := time.Now().Add(10 * time.Second); info().Bytes_acked < uint64(len(answer)); {
		if time.Now().After(limit) {
			t.Fatal("the client's stack did not take the answer in 10s")
		}
		time.Sleep(50 * time.Microsecond)
	}
	_ = clientApp.Close()
	for limit := time.Now().Add(10 * time.Second); info().State != unix.BPF_TCP_CLOSE; {
		if time.Now().After(limit) {
			t.Fatal("the client did not reset the answer it closed on unread in 10s")
		}
		time.Sleep(50 * time.Microsecond)
	}
	_ = destSvc.Close()
	select {
	case <-probe.ended:
	case <-time.After(10 * time.Second):
		t.Fatal("the parser's destination stream did not end")
	}
	probe.mu.Lock()
	defer probe.mu.Unlock()
	if string(probe.carried) != answer {
		t.Fatalf("the parser was given %q, want the answer the client's stack took", probe.carried)
	}
	if !errors.Is(probe.endErr, syscall.EPIPE) && !errors.Is(probe.endErr, syscall.ECONNRESET) {
		t.Fatalf("at the stream's end the session says %v, want the client's reset (EPIPE or ECONNRESET)", probe.endErr)
	}
}
