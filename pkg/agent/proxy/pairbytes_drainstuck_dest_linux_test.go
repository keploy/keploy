//go:build linux

package proxy

import (
	"context"
	"io"
	"net"
	"strings"
	"testing"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/agent/containerkey"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/mocknoise"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// stuckParser is a parser that, handed a connection, never finishes serving
// it until the test ends: its handler never closes the connection.
type stuckParser struct {
	handed  chan struct{}
	release chan struct{}
}

func (stuckParser) MatchType(context.Context, []byte) bool { return true }
func (stuckParser) MockNoise() mocknoise.Adapter           { return nil }
func (stuckParser) RecordOutgoing(context.Context, *integrations.RecordSession) error {
	return nil
}
func (s stuckParser) MockOutgoing(context.Context, net.Conn, *models.ConditionalDstCfg, integrations.MockMemDb, models.OutgoingOptions) error {
	close(s.handed)
	<-s.release
	return nil
}

// A drain that gives up on a stuck connection names where it was going from
// what the proxy's own handler learned as it took the connection
// (serveConnection), not from a destination a row set by hand: a paired
// connection served through handleConnection, whose parser never returns
// once the application has closed its end, is owned naming the destination
// the hooks gave for it.
func TestADrainNamesWhereHandleConnectionServedAStuckConnection(t *testing.T) {
	saved := containerkey.Default
	containerkey.Default = containerkey.NewKeyer(containerAt{addr: "none"})
	t.Cleanup(func() { containerkey.Default = saved })
	cfg := config.New()
	cfg.Agent.Mode = models.MODE_TEST
	p := New(zap.NewNop(), fakeDestInfo{dest: ipv4Dest(t, "127.0.0.1:50051")}, cfg)
	sp := stuckParser{handed: make(chan struct{}), release: make(chan struct{})}
	p.Integrations = map[integrations.IntegrationType]integrations.Integrations{integrations.HTTP: sp}
	p.integrationsPriority = []ParserPriority{{Priority: 1, ParserType: integrations.HTTP}}
	_ = p.Mock(context.Background(), models.OutgoingOptions{DisableMysqlAutoDetect: true, Mocking: true})

	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	c, err := net.Dial("tcp4", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	app := c.(*net.TCPConn)
	t.Cleanup(func() { _ = app.Close() })
	s, err := ln.Accept()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	p.paired.add(s.(*net.TCPConn), 1, 0)
	conn, tracked := p.acceptedConn(context.Background(), s)
	if tracked == nil {
		t.Fatal("premise: the proxy's accept keeps a paired connection")
	}
	t.Cleanup(func() { p.inflight.forget(tracked) })
	done := make(chan error, 1)
	go func() { done <- p.handleConnection(context.Background(), conn) }()
	t.Cleanup(func() {
		close(sp.release)
		awaitEvent(t, done, "handleConnection returning once its parser is released")
	})
	if _, err := io.WriteString(app, "hello\n"); err != nil {
		t.Fatal(err)
	}
	awaitEvent(t, sp.handed, "the parser being handed the connection")
	_ = app.Close()
	awaitState(t, s.(*net.TCPConn), peerEnded, "the application's FIN")

	clk, got := drainAt(p)
	awaitDrainLook(t, clk, got)
	clk.Advance(drainBudget)
	if err := drainAnswers(t, got, "once its bound passed with the handler stuck"); err != nil {
		t.Fatalf("the drain: %v; want an answer, the stuck close owned", err)
	}
	owned := stuckLosses(t, p)
	if len(owned) != 1 {
		t.Fatalf("owned %+v, want the stuck close once", owned)
	}
	if !strings.Contains(owned[0].Loss.What, "to 127.0.0.1:50051 from ") {
		t.Fatalf("what %q, want it to name the destination handleConnection served the connection to, 127.0.0.1:50051", owned[0].Loss.What)
	}
}
