package proxy

import (
	"context"
	"net"
	"sync"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/connseq"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
	"golang.org/x/sync/errgroup"
)

// stubParser is the minimum that satisfies integrations.Integrations. It
// exists so the capability probe can be tested without dragging a real
// protocol implementation in.
type stubParser struct{}

func (stubParser) MatchType(context.Context, []byte) bool { return false }
func (stubParser) RecordOutgoing(context.Context, *integrations.RecordSession) error {
	return nil
}
func (stubParser) MockOutgoing(context.Context, net.Conn, *models.ConditionalDstCfg, integrations.MockMemDb, models.OutgoingOptions) error {
	return nil
}

// resyncParser claims the gap-resync capability, answering with whatever it
// was built with — so the probe is pinned to the METHOD'S ANSWER, not merely
// to the presence of the method.
type resyncParser struct {
	stubParser
	can bool
}

func (r resyncParser) CanResyncAfterGap() bool { return r.can }

// TestParserCanResyncAfterGapProbe pins the discovery rule that decides
// whether a desynced connection keeps feeding its parser.
//
// The stakes are asymmetric, which is why the default matters more than the
// opt-in. Wrongly answering false for mongo/v2 costs it its recovery: it
// re-anchors by content-scanning the bytes that arrive after the hole, so
// with no bytes it stays desynced for the life of a pooled connection.
// Wrongly answering true for anything else keeps a mis-framing parser fed —
// no mocks, and in the postgres case a bogus uint32 length read out of
// misread row data driving a multi-gigabyte allocation.
func TestParserCanResyncAfterGapProbe(t *testing.T) {
	t.Parallel()

	t.Run("a parser without the capability cannot resync", func(t *testing.T) {
		t.Parallel()
		if parserCanResyncAfterGap(stubParser{}) {
			t.Fatal("a parser that does not implement GapResyncCapable must default to " +
				"false: never having heard of the capability is evidence AGAINST having a " +
				"resync path, so omission must not opt a parser in")
		}
	})

	t.Run("a parser that claims the capability is taken at its word", func(t *testing.T) {
		t.Parallel()
		if !parserCanResyncAfterGap(resyncParser{can: true}) {
			t.Fatal("a parser implementing GapResyncCapable and returning true must keep " +
				"its feed after a desync — that is the whole point of the capability")
		}
	})

	t.Run("the method's answer is consulted, not just its presence", func(t *testing.T) {
		t.Parallel()
		if parserCanResyncAfterGap(resyncParser{can: false}) {
			t.Fatal("CanResyncAfterGap() returned false but the probe said true: the " +
				"capability is allowed to be answered dynamically (a parser may only " +
				"resync under some configurations), so the return value is the contract")
		}
	})

	t.Run("a nil parser cannot resync", func(t *testing.T) {
		t.Parallel()
		if parserCanResyncAfterGap(nil) {
			t.Fatal("a nil parser must take the safe default rather than panic")
		}
	})
}

// feedProbe is a V2 parser that does nothing but record which chunks the
// relay actually delivered to it on the client stream. It never emits a mock
// and never returns while the connection is alive, so the supervisor leaves
// it alone and the only thing under test is the feed.
type feedProbe struct {
	stubParser
	mu   sync.Mutex
	got  [][]byte
	fedC chan struct{}
}

func newFeedProbe() *feedProbe {
	return &feedProbe{fedC: make(chan struct{}, 64)}
}

func (f *feedProbe) IsV2() bool { return true }

func (f *feedProbe) RecordOutgoing(ctx context.Context, s *integrations.RecordSession) error {
	cs := s.V2.ClientStream
	for {
		c, err := cs.ReadChunk()
		if err != nil {
			// The stream is gone (test teardown). Park until the supervised
			// context ends so the parser never "returns early", which would
			// retire it and muddy the signal with a fallthrough.
			<-ctx.Done()
			return ctx.Err()
		}
		f.mu.Lock()
		f.got = append(f.got, append([]byte(nil), c.Bytes...))
		f.mu.Unlock()
		select {
		case f.fedC <- struct{}{}:
		default:
		}
	}
}

func (f *feedProbe) delivered() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, 0, len(f.got))
	for _, b := range f.got {
		out = append(out, string(b))
	}
	return out
}

// resyncFeedProbe is the same parser with the gap-resync capability declared,
// standing in for mongo/v2. The ONLY difference from feedProbe is this method
// — which is precisely the variable under test.
type resyncFeedProbe struct{ *feedProbe }

func (resyncFeedProbe) CanResyncAfterGap() bool { return true }

// desyncFeedHarness runs recordViaSupervisor over real socket pairs with a
// per-connection cap small enough that the first write blows through it, and
// returns the probe plus a func that writes from the application side.
type desyncFeedHarness struct {
	probe    *feedProbe
	writeApp func(t *testing.T, b []byte)
	destSeen func() string
	stop     func()
}

func newDesyncFeedHarness(t *testing.T, parser integrations.Integrations, probe *feedProbe, perConnCap int64) *desyncFeedHarness {
	t.Helper()
	return newDesyncFeedHarnessCtx(t, context.Background(), parser, probe, perConnCap)
}

// newDesyncFeedHarnessCtx is newDesyncFeedHarness on a parent context, which
// can carry a private syncMock manager (syncMock.NewContext) for a test that
// reads what the connection did to its recording.
func newDesyncFeedHarnessCtx(t *testing.T, parent context.Context, parser integrations.Integrations, probe *feedProbe, perConnCap int64) *desyncFeedHarness {
	t.Helper()

	clientApp, srcConn := net.Pipe()
	dstConn, destSvc := net.Pipe()

	p := &Proxy{
		recordBufferCap:        perConnCap,
		recordBufferQueueSize:  64,
		recordBufferStallGrace: 2 * time.Second,
	}

	// The destination must be drained or the relay's forward Write blocks on
	// the synchronous pipe — and then nothing reaches the tee at all, which
	// would make every assertion below vacuous.
	var destMu sync.Mutex
	var destBuf []byte
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := destSvc.Read(buf)
			if n > 0 {
				destMu.Lock()
				destBuf = append(destBuf, buf[:n]...)
				destMu.Unlock()
			}
			if err != nil {
				return
			}
		}
	}()

	ctx, cancel := context.WithCancel(parent)
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = p.recordViaSupervisor(ctx, srcConn, connseq.NewUpstream(dstConn), parser, "test",
			make(chan *models.Mock, 8), &errgroup.Group{}, zap.NewNop(), 1, 2,
			models.OutgoingOptions{})
	}()

	h := &desyncFeedHarness{
		probe: probe,
		writeApp: func(t *testing.T, b []byte) {
			t.Helper()
			if _, err := clientApp.Write(b); err != nil {
				t.Fatalf("app write %q: %v", b, err)
			}
		},
		destSeen: func() string {
			destMu.Lock()
			defer destMu.Unlock()
			return string(destBuf)
		},
	}
	h.stop = func() {
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
	}
	t.Cleanup(h.stop)
	return h
}

// waitForDelivery blocks until the probe has been handed at least n chunks,
// or fails. Returns true if it got there.
func waitForDelivery(probe *feedProbe, n int, budget time.Duration) bool {
	deadline := time.Now().Add(budget)
	for time.Now().Before(deadline) {
		if len(probe.delivered()) >= n {
			return true
		}
		time.Sleep(2 * time.Millisecond)
	}
	return len(probe.delivered()) >= n
}

// TestRecordViaSupervisorWiresGapResyncCapabilityIntoTheRelay closes the loop
// that the unit tests each cover only half of.
//
// parserCanResyncAfterGap is tested against hand-made parsers, and the relay's
// gate is tested against a hand-set tee field — but nothing pinned the wire
// BETWEEN them: that recordViaSupervisor probes the real parser and puts the
// answer on relay.Config.ParserCanResyncAfterGap. Hardcoding that field to
// false left every other test in this change passing while mongo/v2 silently
// lost its feed, which is the regression this whole capability exists to
// prevent. So this drives the production entry point over real sockets and
// asserts on what the parser actually receives.
func TestRecordViaSupervisorWiresGapResyncCapabilityIntoTheRelay(t *testing.T) {
	t.Parallel()

	// oversized blows through the 4-byte cap, so the tee refuses it with
	// per_conn_cap and latches the permanent desync. followUp fits with room
	// to spare, so the ONLY thing that can withhold it is the desync gate.
	oversized := []byte("0123456789")
	followUp := []byte("ok")

	t.Run("a parser without the capability stops being fed after the desync", func(t *testing.T) {
		t.Parallel()
		probe := newFeedProbe()
		h := newDesyncFeedHarness(t, probe, probe, 4)

		h.writeApp(t, oversized)
		h.writeApp(t, followUp)

		// Give the relay well past the point where a delivery would have
		// happened; the positive control below proves this budget is ample.
		if waitForDelivery(probe, 1, 750*time.Millisecond) {
			t.Fatalf("parser was fed %q after the connection desynced; it would frame "+
				"those bytes from the wrong offset, produce no mock, and can read a "+
				"bogus multi-gigabyte length out of misread payload", probe.delivered())
		}

		// The forward path is untouched — that is the invariant that makes
		// cutting the feed safe to ship at all.
		waitForCondition(t, 2*time.Second, func() bool {
			return h.destSeen() == string(oversized)+string(followUp)
		})
	})

	t.Run("a parser that declares the capability keeps its feed", func(t *testing.T) {
		t.Parallel()
		probe := newFeedProbe()
		h := newDesyncFeedHarness(t, resyncFeedProbe{probe}, probe, 4)

		h.writeApp(t, oversized)
		h.writeApp(t, followUp)

		if !waitForDelivery(probe, 1, 3*time.Second) {
			t.Fatal("a parser declaring CanResyncAfterGap must keep receiving bytes after " +
				"a desync — the post-hole bytes are the only thing it can re-anchor on. " +
				"If this fails, recordViaSupervisor is not putting the probe's answer on " +
				"relay.Config.ParserCanResyncAfterGap")
		}
		if got := probe.delivered(); got[0] != string(followUp) {
			t.Fatalf("parser got %q, want the post-hole chunk %q", got, followUp)
		}
	})

	t.Run("without a desync the same parser is fed normally", func(t *testing.T) {
		t.Parallel()
		// The negative case above asserts an absence, which passes just as
		// well if the harness never delivers anything. This is the control:
		// same parser, same bytes, a cap big enough that nothing desyncs.
		probe := newFeedProbe()
		h := newDesyncFeedHarness(t, probe, probe, 1<<20)

		h.writeApp(t, oversized)

		if !waitForDelivery(probe, 1, 3*time.Second) {
			t.Fatal("a parser with no desync must be fed: the harness itself is broken " +
				"if this fails, and the negative case above proves nothing")
		}
	})
}

// waitForCondition polls cond until it holds or the budget expires.
func waitForCondition(t *testing.T, budget time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(budget)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal("timed out waiting for condition")
}
