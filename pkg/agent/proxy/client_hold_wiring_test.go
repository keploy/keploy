package proxy

import (
	"context"
	"net"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/agent/proxy/util"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
	"golang.org/x/sync/errgroup"
)

// TestRecordViaSupervisorArmsTheClientWriteHold pins the ONE line that
// turns the hold on.
//
// client_brakes_test.go tests applyClientBrakes itself, and an earlier
// review found the feature had shipped entirely inert because nothing
// called it. That review's fix is still undefended by that file:
// replacing the applyClientBrakes(...) call in recordViaSupervisor with
// `_ = applyClientBrakes` leaves every test in the package green. A
// feature whose only wiring is one line in a 400-line dispatcher needs a
// test that fails when the line goes away — this is it.
//
// It asserts the OBSERVABLE effect rather than the config value: with a
// hold armed, bytes the client writes must not reach the destination
// until a directive ends it. Real sockets, because net.Pipe would let
// the forwarder's write block and mask the difference.
type holdWantingParser struct{ feedProbe }

func (h *holdWantingParser) WantsClientWriteHold() bool { return true }

func TestRecordViaSupervisorArmsTheClientWriteHold(t *testing.T) {
	t.Parallel()

	tcpPair := func(t *testing.T) (dialed, accepted net.Conn) {
		t.Helper()
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatalf("listen: %v", err)
		}
		defer func() { _ = ln.Close() }()
		type res struct {
			c   net.Conn
			err error
		}
		ch := make(chan res, 1)
		go func() {
			c, err := ln.Accept()
			ch <- res{c, err}
		}()
		d, err := net.Dial("tcp", ln.Addr().String())
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		got := <-ch
		if got.err != nil {
			t.Fatalf("accept: %v", got.err)
		}
		t.Cleanup(func() { _ = d.Close(); _ = got.c.Close() })
		return d, got.c
	}

	run := func(t *testing.T, parser integrations.Integrations) (clientApp, destSvc net.Conn) {
		t.Helper()
		clientApp, rawSrc := tcpPair(t)
		dstConn, destSvc := tcpPair(t)
		srcConn := &util.Conn{Conn: rawSrc, Reader: rawSrc, Logger: zap.NewNop()}

		p := &Proxy{
			recordBufferCap:        64 * 1024,
			recordBufferQueueSize:  64,
			recordBufferStallGrace: 2 * time.Second,
		}
		ctx, cancel := context.WithCancel(context.Background())
		t.Cleanup(cancel)
		go func() {
			_ = p.recordViaSupervisor(ctx, srcConn, dstConn, parser, "test",
				make(chan *models.Mock, 8), &errgroup.Group{}, zap.NewNop(), 1, 2,
				models.OutgoingOptions{})
		}()
		return clientApp, destSvc
	}

	reached := func(t *testing.T, destSvc net.Conn, within time.Duration) bool {
		t.Helper()
		_ = destSvc.SetReadDeadline(time.Now().Add(within))
		buf := make([]byte, 32)
		n, _ := destSvc.Read(buf)
		return n > 0
	}

	t.Run("a parser that asks for the hold gets it", func(t *testing.T) {
		clientApp, destSvc := run(t, &holdWantingParser{*newFeedProbe()})
		if _, err := clientApp.Write([]byte("held-bytes")); err != nil {
			t.Fatalf("write: %v", err)
		}
		if reached(t, destSvc, 750*time.Millisecond) {
			t.Fatal("the client's bytes reached the destination although the parser asked for " +
				"a client write hold. recordViaSupervisor is not putting that answer on " +
				"relay.Config.HoldClientWrites, so MySQL's ClientHello still leaks upstream " +
				"in cleartext — the exact defect this feature exists to prevent, and the " +
				"exact way it shipped inert the first time.")
		}
	})

	// Positive control: without it, the assertion above could pass
	// because nothing forwards at all.
	t.Run("a parser that does not ask is forwarded normally", func(t *testing.T) {
		clientApp, destSvc := run(t, newFeedProbe())
		if _, err := clientApp.Write([]byte("plain-bytes")); err != nil {
			t.Fatalf("write: %v", err)
		}
		if !reached(t, destSvc, 3*time.Second) {
			t.Fatal("nothing reached the destination for a parser with no hold; the negative " +
				"case above would then pass for the wrong reason")
		}
	})
}
