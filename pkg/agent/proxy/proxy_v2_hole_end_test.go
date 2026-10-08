package proxy

import (
	"context"
	"errors"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/connseq"
	"go.keploy.io/server/v3/pkg/agent/proxy/fakeconn"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/agent/proxy/relay"
	"go.keploy.io/server/v3/pkg/agent/proxy/supervisor"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
	"golang.org/x/sync/errgroup"
)

// holeEndProbe is a V2 parser that reads both streams of its connection, each
// to its own end, once its gate opens. It notes what each delivered, how each
// ended, and what the session said then. It never returns while its context
// lives, so the supervisor leaves it alone.
type holeEndProbe struct {
	stubParser
	gate chan struct{}

	mu      sync.Mutex
	got     [2][]string
	endErr  [2]error
	marked  [2]bool
	holeWhy [2]string
	atHole  [2]bool
	endedC  [2]chan struct{}
	fedC    chan struct{}
}

func newHoleEndProbe() *holeEndProbe {
	return &holeEndProbe{
		gate:   make(chan struct{}),
		endedC: [2]chan struct{}{make(chan struct{}), make(chan struct{})},
		fedC:   make(chan struct{}, 64),
	}
}

func (p *holeEndProbe) IsV2() bool { return true }

func (p *holeEndProbe) RecordOutgoing(ctx context.Context, s *integrations.RecordSession) error {
	<-p.gate
	sess := s.V2
	var wg sync.WaitGroup
	for d, fc := range [2]*fakeconn.FakeConn{sess.ClientStream, sess.DestStream} {
		wg.Add(1)
		go func(d int, fc *fakeconn.FakeConn) {
			defer wg.Done()
			for {
				c, err := fc.ReadChunk()
				if err != nil {
					why, atHole := sess.EndedAtHole(fakeconn.Direction(d))
					p.mu.Lock()
					p.endErr[d] = err
					p.marked[d] = sess.IsMockIncomplete()
					p.holeWhy[d], p.atHole[d] = why, atHole
					p.mu.Unlock()
					close(p.endedC[d])
					return
				}
				p.mu.Lock()
				p.got[d] = append(p.got[d], string(c.Bytes))
				p.mu.Unlock()
				select {
				case p.fedC <- struct{}{}:
				default:
				}
			}
		}(d, fc)
	}
	wg.Wait()
	<-ctx.Done()
	return ctx.Err()
}

func (p *holeEndProbe) delivered(d fakeconn.Direction) []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.got[d]...)
}

// endAtHoleProbe is holeEndProbe declaring integrations.EndAtHoleCapable.
type endAtHoleProbe struct{ *holeEndProbe }

func (endAtHoleProbe) CanEndAtHole() bool { return true }

// resyncEndAtHoleProbe also declares that it re-aligns after a hole, which
// keeps its feed: it ends no direction at a hole.
type resyncEndAtHoleProbe struct{ *holeEndProbe }

func (resyncEndAtHoleProbe) CanEndAtHole() bool      { return true }
func (resyncEndAtHoleProbe) CanResyncAfterGap() bool { return true }

// holeEndHarness runs recordViaSupervisor over two socket pairs, with the
// application on one end and the destination on the other.
type holeEndHarness struct {
	writeApp  func(t *testing.T, b string)
	writeDest func(t *testing.T, b string)
	destSeen  func() string
	appSeen   func() string
	// returned is closed once recordViaSupervisor has returned.
	returned <-chan struct{}
}

func newHoleEndHarness(t *testing.T, parser integrations.Integrations, perConnCap int64) *holeEndHarness {
	t.Helper()
	clientApp, srcConn := net.Pipe()
	dstConn, destSvc := net.Pipe()
	p := &Proxy{
		recordBufferCap:        perConnCap,
		recordBufferQueueSize:  64,
		recordBufferStallGrace: 2 * time.Second,
	}
	// Both far ends are drained, or the relay's forward Write blocks on the
	// synchronous pipe and nothing reaches a tee.
	var destMu sync.Mutex
	var destBuf []byte
	drain := func(c net.Conn, keep func([]byte)) {
		buf := make([]byte, 4096)
		for {
			n, err := c.Read(buf)
			if n > 0 && keep != nil {
				keep(buf[:n])
			}
			if err != nil {
				return
			}
		}
	}
	go drain(destSvc, func(b []byte) {
		destMu.Lock()
		destBuf = append(destBuf, b...)
		destMu.Unlock()
	})
	var appMu sync.Mutex
	var appBuf []byte
	go drain(clientApp, func(b []byte) {
		appMu.Lock()
		appBuf = append(appBuf, b...)
		appMu.Unlock()
	})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = p.recordViaSupervisor(ctx, srcConn, connseq.NewUpstream(dstConn), parser, "test",
			make(chan *models.Mock, 8), &errgroup.Group{}, zap.NewNop(), 1, 2,
			models.OutgoingOptions{})
	}()
	t.Cleanup(func() {
		cancel()
		_ = clientApp.Close()
		_ = destSvc.Close()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Errorf("recordViaSupervisor did not return within 5s")
		}
		_ = srcConn.Close()
		_ = dstConn.Close()
	})
	// A write the relay no longer reads fails rather than hanging the test.
	write := func(t *testing.T, c net.Conn, who, b string) {
		t.Helper()
		_ = c.SetWriteDeadline(time.Now().Add(2 * time.Second))
		if _, err := c.Write([]byte(b)); err != nil {
			t.Fatalf("%s write %q: %v", who, b, err)
		}
	}
	return &holeEndHarness{
		writeApp:  func(t *testing.T, b string) { t.Helper(); write(t, clientApp, "app", b) },
		writeDest: func(t *testing.T, b string) { t.Helper(); write(t, destSvc, "destination", b) },
		destSeen: func() string {
			destMu.Lock()
			defer destMu.Unlock()
			return string(destBuf)
		},
		appSeen: func() string {
			appMu.Lock()
			defer appMu.Unlock()
			return string(appBuf)
		},
		returned: done,
	}
}

// waitEnded reports whether direction d of probe ended within budget.
func (p *holeEndProbe) waitEnded(d fakeconn.Direction, budget time.Duration) bool {
	select {
	case <-p.endedC[d]:
		return true
	case <-time.After(budget):
		return false
	}
}

// holeEndTraffic sends what each subtest below sends: a client chunk that
// fits the 8-byte cap, a server chunk, a client chunk the cap refuses (the
// hole, in the client's direction), and a client chunk after it. It returns
// once the destination has every client byte, so the forward path is intact
// and each chunk has reached its tee.
func holeEndTraffic(t *testing.T, h *holeEndHarness) {
	t.Helper()
	h.writeApp(t, "ab")
	h.writeDest(t, "srv1")
	h.writeApp(t, "0123456789")
	h.writeApp(t, "cd")
	waitForCondition(t, 2*time.Second, func() bool { return h.destSeen() == "ab0123456789cd" })
	// The forwarder tees a chunk after it writes it: give the last push time
	// to land.
	time.Sleep(50 * time.Millisecond)
}

// A parser that cannot re-align after a hole, and reads each direction to its
// own end (integrations.EndAtHoleCapable), gets a direction that lost a chunk
// ended where the hole is: every chunk teed before it, then io.EOF, while the
// connection, and the other direction, go on. The session says the stream
// ended at a hole, and why. No incomplete-mock mark is set for it: the parser
// runs behind its capture, so a mark, which voids whichever mock is emitted
// next, would void one from before the hole.
//
// Before, the direction was fed no more, and never ended while the connection
// lived. The parser could not tell where the hole was. The relay marked the
// flag for the lost chunk and for each one it refused after it, and an HTTP/2
// recorder that stopped at the mark lost the exchanges still queued before
// the hole.
func TestRecordViaSupervisorEndsADirectionAtItsHoleForAParserThatAsks(t *testing.T) {
	t.Parallel()

	t.Run("a parser that asks gets the direction ended at the hole", func(t *testing.T) {
		t.Parallel()
		probe := newHoleEndProbe()
		h := newHoleEndHarness(t, endAtHoleProbe{probe}, 8)
		holeEndTraffic(t, h)
		close(probe.gate)

		if !probe.waitEnded(fakeconn.FromClient, 3*time.Second) {
			t.Fatalf("the client direction did not end after its hole: the parser got %q and waits for more, and cannot tell where the hole is", probe.delivered(fakeconn.FromClient))
		}
		probe.mu.Lock()
		endErr, marked, atHole, why := probe.endErr[0], probe.marked[0], probe.atHole[0], probe.holeWhy[0]
		probe.mu.Unlock()
		if !errors.Is(endErr, io.EOF) {
			t.Fatalf("the client direction ended with %v, want io.EOF", endErr)
		}
		if got := probe.delivered(fakeconn.FromClient); len(got) != 1 || got[0] != "ab" {
			t.Fatalf("the client direction delivered %q, want the chunk teed before the hole, alone", got)
		}
		if marked {
			t.Fatal("the session's incomplete-mock flag was set at the end: the relay marked a mock for the hole, which voids whichever mock the parser emits next, one from before the hole")
		}
		if !atHole || why != relay.DropPerConnCap {
			t.Fatalf("the session said the client direction ended at a hole %v, for %q; want true, for %q", atHole, why, relay.DropPerConnCap)
		}

		// The other direction goes on.
		h.writeDest(t, "srv2")
		waitForCondition(t, 2*time.Second, func() bool { return len(probe.delivered(fakeconn.FromDest)) == 2 })
		if got := probe.delivered(fakeconn.FromDest); got[0] != "srv1" || got[1] != "srv2" {
			t.Fatalf("the server direction delivered %q, want srv1 and srv2", got)
		}
		if probe.waitEnded(fakeconn.FromDest, 100*time.Millisecond) {
			t.Fatal("the server direction ended, but it lost nothing and the connection goes on")
		}
	})

	t.Run("a parser that does not ask is fed no more, and its direction does not end", func(t *testing.T) {
		t.Parallel()
		probe := newHoleEndProbe()
		h := newHoleEndHarness(t, probe, 8)
		holeEndTraffic(t, h)
		close(probe.gate)

		if probe.waitEnded(fakeconn.FromClient, 750*time.Millisecond) {
			t.Fatal("the client direction ended at its hole for a parser that did not ask: it would take the end for the connection's, and record what the hole cut as though the connection had closed there")
		}
		if got := probe.delivered(fakeconn.FromClient); len(got) != 1 || got[0] != "ab" {
			t.Fatalf("the client direction delivered %q, want the chunk teed before the hole, alone", got)
		}
	})

	t.Run("a parser that re-aligns keeps its feed, whatever else it asks", func(t *testing.T) {
		t.Parallel()
		probe := newHoleEndProbe()
		h := newHoleEndHarness(t, resyncEndAtHoleProbe{probe}, 8)
		holeEndTraffic(t, h)
		close(probe.gate)

		waitForCondition(t, 2*time.Second, func() bool { return len(probe.delivered(fakeconn.FromClient)) == 2 })
		if got := probe.delivered(fakeconn.FromClient); got[1] != "cd" {
			t.Fatalf("the client direction delivered %q, want the chunk after the hole too", got)
		}
		if probe.waitEnded(fakeconn.FromClient, 100*time.Millisecond) {
			t.Fatal("the client direction of a parser that re-aligns ended at its hole: it re-aligns on the bytes after the hole, and needs them")
		}
	})
}

// askEndAtHole answers CanEndAtHole with what it was built with, so the probe
// is pinned to the method's answer, not to its presence.
type askEndAtHole struct {
	stubParser
	can bool
}

func (a askEndAtHole) CanEndAtHole() bool { return a.can }

// applyEndAtHole wires relay.Config.EndAtHole to the session only for a
// parser that asks, and cannot re-align after a hole.
func TestApplyEndAtHole(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		parser   integrations.Integrations
		canAlign bool
		want     bool
	}{
		{name: "a parser that does not implement it", parser: stubParser{}},
		{name: "a parser that answers no", parser: askEndAtHole{can: false}},
		{name: "a parser that asks", parser: askEndAtHole{can: true}, want: true},
		{name: "a parser that asks but re-aligns", parser: askEndAtHole{can: true}, canAlign: true},
		{name: "no parser", parser: nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			sess := &supervisor.Session{}
			cfg := relay.Config{ParserCanResyncAfterGap: tc.canAlign}
			applyEndAtHole(&cfg, sess, tc.parser)
			if got := cfg.EndAtHole != nil; got != tc.want {
				t.Fatalf("EndAtHole set %v, want %v", got, tc.want)
			}
			if !tc.want {
				return
			}
			cfg.EndAtHole(fakeconn.FromDest, relay.DropMemoryPressure)
			if why, ok := sess.EndedAtHole(fakeconn.FromDest); !ok || why != relay.DropMemoryPressure {
				t.Fatalf("EndAtHole reached the session as %q, %v; want %q, true", why, ok, relay.DropMemoryPressure)
			}
		})
	}
	applyEndAtHole(nil, &supervisor.Session{}, askEndAtHole{can: true})
	cfg := relay.Config{}
	applyEndAtHole(&cfg, nil, askEndAtHole{can: true})
	if cfg.EndAtHole != nil {
		t.Fatal("EndAtHole was set with no session to say it on")
	}
}

// returningEndAtHoleProbe asks for its directions to end at their holes, and
// reads each to its own end, like holeEndProbe; then it returns, as a parser
// that has read all of its connection does (keploy/integrations' HTTP/2
// recorder returns once both directions have ended).
type returningEndAtHoleProbe struct{ *holeEndProbe }

func (returningEndAtHoleProbe) CanEndAtHole() bool { return true }

func (p returningEndAtHoleProbe) RecordOutgoing(_ context.Context, s *integrations.RecordSession) error {
	<-p.gate
	sess := s.V2
	var wg sync.WaitGroup
	for d, fc := range [2]*fakeconn.FakeConn{sess.ClientStream, sess.DestStream} {
		wg.Add(1)
		go func(d int, fc *fakeconn.FakeConn) {
			defer wg.Done()
			for {
				if _, err := fc.ReadChunk(); err != nil {
					p.mu.Lock()
					p.endErr[d] = err
					p.mu.Unlock()
					close(p.endedC[d])
					return
				}
			}
		}(d, fc)
	}
	wg.Wait()
	return nil
}

// A parser's return never ends the relay: the application's connection
// outlives every way a parser stops. Here a hole in each direction (the
// client's chunk and the server's are each too large for the cap) ends both
// streams of a parser that asked for it, while the connection goes on. The
// parser reads both to their end and returns, with no error. The relay goes
// on forwarding the application's bytes and the destination's, and
// recordViaSupervisor returns only once the peers have closed.
//
// Before, the dispatcher cancelled the relay when the parser returned, and
// its caller then closed the application's socket and the destination's: a
// write the application made next never reached the destination.
func TestRecordViaSupervisorKeepsTheConnectionWhenItsParserReturns(t *testing.T) {
	t.Parallel()
	probe := newHoleEndProbe()
	h := newHoleEndHarness(t, returningEndAtHoleProbe{probe}, 8)
	h.writeApp(t, "ab")
	h.writeDest(t, "srv1")
	h.writeApp(t, "0123456789")
	h.writeDest(t, "a server chunk past the cap")
	waitForCondition(t, 2*time.Second, func() bool { return h.destSeen() == "ab0123456789" })
	waitForCondition(t, 2*time.Second, func() bool { return h.appSeen() == "srv1a server chunk past the cap" })
	// The forwarder tees a chunk after it writes it: give the last pushes time
	// to land.
	time.Sleep(50 * time.Millisecond)
	close(probe.gate)

	for _, d := range []fakeconn.Direction{fakeconn.FromClient, fakeconn.FromDest} {
		if !probe.waitEnded(d, 3*time.Second) {
			t.Fatalf("the %v direction did not end at its hole", d)
		}
	}
	select {
	case <-h.returned:
		t.Fatal("recordViaSupervisor returned when its parser did, with both peers open: it ended the relay, and its caller closes the application's connection")
	case <-time.After(300 * time.Millisecond):
	}

	h.writeApp(t, "after")
	h.writeDest(t, "answer")
	waitForCondition(t, 2*time.Second, func() bool { return h.destSeen() == "ab0123456789after" })
	waitForCondition(t, 2*time.Second, func() bool { return h.appSeen() == "srv1a server chunk past the capanswer" })
}

// holdingReturnProbe asks the relay to hold the client's writes, reads the
// client's first chunk, and returns with the hold still armed.
type holdingReturnProbe struct {
	stubParser
	returned chan struct{}
}

func (holdingReturnProbe) IsV2() bool                 { return true }
func (holdingReturnProbe) WantsClientWriteHold() bool { return true }

func (p holdingReturnProbe) RecordOutgoing(_ context.Context, s *integrations.RecordSession) error {
	defer close(p.returned)
	_, err := s.V2.ClientStream.ReadChunk()
	return err
}

// A parser that returns with the client's writes held does not take the
// application's connection with it either: the hold is released when it
// returns, so what it held reaches the destination, and so does what the
// application writes next.
func TestRecordViaSupervisorReleasesTheClientHoldWhenItsParserReturns(t *testing.T) {
	t.Parallel()
	probe := holdingReturnProbe{returned: make(chan struct{})}
	h := newHoleEndHarness(t, probe, 0)
	h.writeApp(t, "held")
	select {
	case <-probe.returned:
	case <-time.After(3 * time.Second):
		t.Fatal("the parser never read the client's chunk")
	}
	waitForCondition(t, 2*time.Second, func() bool { return h.destSeen() == "held" })
	h.writeApp(t, "after")
	waitForCondition(t, 2*time.Second, func() bool { return h.destSeen() == "heldafter" })
	select {
	case <-h.returned:
		t.Fatal("recordViaSupervisor returned when its parser did, with both peers open")
	default:
	}
}
