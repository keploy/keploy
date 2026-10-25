package relay

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/directive"
	"go.keploy.io/server/v3/pkg/agent/proxy/fakeconn"
	"go.keploy.io/server/v3/pkg/models"
)

// A client-side TLS upgrade whose handshake failed is handed, with the
// upgrader's own error, to OnClientHandshakeFailed -- the proxy charges it
// as any client-side handshake -- and is no STARTTLS loss; a destination-side
// failure, an upgrade keploy could not make, still is. Without the hook, a
// client-side failure is one too, as before.
func TestAFailedClientSideUpgradeGoesToTheClientHandshakeHook(t *testing.T) {
	for _, tc := range []struct {
		name         string
		failSide     string
		hook         bool
		wantHook     bool
		wantStartTLS int
	}{
		{"the client's side, with the hook", "client", true, true, 0},
		{"the destination's side, with the hook", "dest", true, false, 1},
		{"the client's side, without the hook", "client", false, false, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var mu sync.Mutex
			var order []string
			fn, _ := recordingUpgrader(&mu, &order, tc.failSide)
			var starttls int
			var got error
			cfg := Config{TLSUpgradeFn: fn, OnCaptureLoss: func(k models.CaptureLossKind, _ string) {
				mu.Lock()
				defer mu.Unlock()
				if k == models.CaptureLossSTARTTLS {
					starttls++
				}
			}}
			if tc.hook {
				cfg.OnClientHandshakeFailed = func(_ context.Context, err error) {
					mu.Lock()
					defer mu.Unlock()
					got = err
				}
			}
			h := newHarness(t, cfg)
			h.r.Directives() <- directive.UpgradeTLS(&tls.Config{}, &tls.Config{}, "upgrade")
			if ack := awaitAck(t, h); ack.OK {
				t.Fatal("the upgrade succeeded")
			}
			mu.Lock()
			defer mu.Unlock()
			if (got != nil) != tc.wantHook || (got != nil && got.Error() != "client upgrade failed (simulated)") {
				t.Fatalf("hook got %v, want called %v with the upgrader's error", got, tc.wantHook)
			}
			if starttls != tc.wantStartTLS {
				t.Fatalf("%d starttls_failed, want %d", starttls, tc.wantStartTLS)
			}
		})
	}
}

// The failed ack still says the client's side failed, and wraps its error.
func TestAClientHandshakeErrorReadsAsBefore(t *testing.T) {
	inner := errors.New("tls: bad certificate")
	err := error(&clientHandshakeError{err: inner})
	if err.Error() != "client TLS upgrade: tls: bad certificate" || !errors.Is(err, inner) {
		t.Fatalf("%v: want the old text, wrapping the handshake's error", err)
	}
}

// stashBound only ends a run that is broken (a relay that never tees what it
// delivers, or writes before it tees, blocks in its Write below); no verdict
// depends on how long anything takes.
const stashBound = 30 * time.Second

// readFromDest is a destination chunk as its forwarder reads one: stamped now,
// and numbered by the relay's connseq.Upstream (ConnSeq and AfterClientClose).
func readFromDest(r *Relay, b []byte) fakeconn.Chunk {
	c := fakeconn.Chunk{Dir: fakeconn.FromDest, Bytes: b, ReadAt: time.Now()}
	c.ConnSeq, c.AfterClientClose = r.order.FromDest()
	return c
}

// awaitParkedOnPause waits until dir's forwarder has signalled that it parks
// on the pause barrier (markForwarderParked): in a pre-dispatch pause it does
// so only after its Read returned and it stashed what it read.
func awaitParkedOnPause(t *testing.T, r *Relay, dir fakeconn.Direction) {
	t.Helper()
	r.pauseMu.Lock()
	parked := r.parkedC2D
	if dir == fakeconn.FromDest {
		parked = r.parkedD2C
	}
	r.pauseMu.Unlock()
	if parked == nil {
		t.Fatalf("no pause is up for the %v forwarder to park on", dir)
	}
	select {
	case <-parked:
	case <-time.After(stashBound):
		t.Fatalf("the %v forwarder never parked on the pause", dir)
	}
}

// stashDeliveryHarness is a relay on net.Pipe sockets whose client reads only
// once the parser has been given a destination chunk (Config.OnCarried with
// the tee taking it), and whose writes to the client fail once failWrites is
// set (writeFails), or fall short (writeFallsShort). A relay that writes a destination chunk to the client before teeing it,
// or never tees it, then blocks in that Write (a net.Pipe Write waits for the
// read) and never acks.
type stashDeliveryHarness struct {
	*relayHarness
	teed       chan struct{}
	failWrites *atomic.Int32
	marked     chan string
	clientGot  chan []byte
}

const (
	writeFails int32 = iota + 1
	writeFallsShort
)

type clientWriteFailer struct {
	net.Conn
	fail *atomic.Int32
}

func (c clientWriteFailer) Write(p []byte) (int, error) {
	switch c.fail.Load() {
	case writeFails:
		return 0, errors.New("the client is gone")
	case writeFallsShort:
		return len(p) / 2, nil
	}
	return c.Conn.Write(p)
}

func newStashDeliveryHarness(t *testing.T, cfg Config, clientReads int) *stashDeliveryHarness {
	t.Helper()
	h := &stashDeliveryHarness{
		teed:       make(chan struct{}),
		failWrites: new(atomic.Int32),
		marked:     make(chan string, 16),
		clientGot:  make(chan []byte, 1),
	}
	var once sync.Once
	cfg.OnCarried = func(dir fakeconn.Direction, _ time.Time, teed bool, _ int64) {
		if dir == fakeconn.FromDest && teed {
			once.Do(func() { close(h.teed) })
		}
	}
	cfg.OnMarkMockIncomplete = func(reason string) {
		select {
		case h.marked <- reason:
		default:
		}
	}
	h.relayHarness = newHarnessOver(t, cfg, func(c net.Conn) net.Conn {
		return clientWriteFailer{Conn: c, fail: h.failWrites}
	})
	if clientReads > 0 {
		go func() {
			select {
			case <-h.teed:
			case <-h.runDone:
				return
			}
			buf := make([]byte, clientReads)
			if _, err := io.ReadFull(h.clientApp, buf); err == nil {
				h.clientGot <- buf
			}
		}()
	}
	return h
}

func (h *stashDeliveryHarness) ack(t *testing.T, kind directive.Kind) directive.Ack {
	t.Helper()
	select {
	case a := <-h.r.Acks():
		if a.Kind != kind {
			t.Fatalf("ack for %v, want %v: %+v", a.Kind, kind, a)
		}
		return a
	case <-time.After(stashBound):
		t.Fatalf("no ack for %v: the relay is stuck writing the destination's stashed bytes to a "+
			"client that reads only once the parser has them, so it wrote them before teeing them, or never teed them", kind)
		return directive.Ack{}
	}
}

// parserChunk is the next chunk on the parser's destination stream.
func (h *stashDeliveryHarness) parserChunk(t *testing.T) fakeconn.Chunk {
	t.Helper()
	fc := h.r.DestStream()
	_ = fc.SetReadDeadline(time.Now().Add(stashBound))
	defer func() { _ = fc.SetReadDeadline(time.Time{}) }()
	c, err := fc.ReadChunk()
	if err != nil {
		t.Fatalf("the parser was never given the destination's stashed bytes: %v", err)
	}
	return c
}

// A plain ResumePreDispatch writes the destination's bytes the pause stashed
// to the client. They are ordinary server bytes the client reads, so the
// parser is given them too, teed before the client can read them, numbered and
// stamped as they were read. Written on untee'd, the app read bytes no parser
// saw and no report named.
func TestAResumeTeesTheDestinationsStashBeforeTheClientReadsIt(t *testing.T) {
	t.Parallel()
	h := newStashDeliveryHarness(t, Config{PreDispatchPause: true}, len("greet"))

	// The destination speaks first: its forwarder reads the greeting, stashes
	// it and parks, so neither the client nor the parser has it.
	before := time.Now()
	h.writeDest([]byte("greet"))
	awaitParkedOnPause(t, h.r, fakeconn.FromDest)

	h.r.Directives() <- directive.ResumePreDispatch("parser-decided-no-tls")
	if a := h.ack(t, directive.KindResumePreDispatch); !a.OK {
		t.Fatalf("resume failed: %+v", a)
	}
	c := h.parserChunk(t)
	if string(c.Bytes) != "greet" || c.Dir != fakeconn.FromDest {
		t.Fatalf("the parser got %v %q, want the destination's stashed greet", c.Dir, c.Bytes)
	}
	if c.ConnSeq == 0 || c.SeqNo == 0 || c.ReadAt.Before(before) {
		t.Fatalf("the stashed chunk reached the parser as ConnSeq %d SeqNo %d ReadAt %v: want it "+
			"numbered and stamped as it was read (after %v)", c.ConnSeq, c.SeqNo, c.ReadAt, before)
	}
	if got := <-h.clientGot; string(got) != "greet" {
		t.Fatalf("the client got %q, want greet", got)
	}
}

// A plain ReleaseClient delivers the destination's bytes stashed at the
// pause boundary to the client the same way: teed first.
func TestAReleaseTeesTheDestinationsStashBeforeTheClientReadsIt(t *testing.T) {
	t.Parallel()
	h := newStashDeliveryHarness(t, Config{HoldClientWrites: true}, len("late reply"))
	h.r.stashInflightFromPause(readFromDest(h.r, []byte("late reply")))

	h.r.Directives() <- directive.ReleaseClient("plaintext auth")
	if a := h.ack(t, directive.KindReleaseClient); !a.OK {
		t.Fatalf("release failed: %+v", a)
	}
	if c := h.parserChunk(t); string(c.Bytes) != "late reply" || c.ConnSeq == 0 || c.SeqNo == 0 {
		t.Fatalf("the parser got %q (ConnSeq %d, SeqNo %d), want the stashed late reply, numbered",
			c.Bytes, c.ConnSeq, c.SeqNo)
	}
	if got := <-h.clientGot; string(got) != "late reply" {
		t.Fatalf("the client got %q, want late reply", got)
	}
}

// When the write to the client fails or falls short, the parser has been
// given bytes the client never got: the mock is marked incomplete, by a resume
// and by a release alike.
func TestAFailedDeliveryOfTheDestinationsStashMarksTheMockIncomplete(t *testing.T) {
	resume, release := Config{PreDispatchPause: true}, Config{HoldClientWrites: true}
	for _, tc := range []struct {
		name   string
		cfg    Config
		send   directive.Directive
		fail   int32
		reason string
	}{
		{"resume/error", resume, directive.ResumePreDispatch("parser-decided-no-tls"), writeFails, "pre_dispatch_drain_d2c_write_error"},
		{"resume/short", resume, directive.ResumePreDispatch("parser-decided-no-tls"), writeFallsShort, "pre_dispatch_drain_d2c_short_write"},
		{"release/error", release, directive.ReleaseClient("plaintext auth"), writeFails, "release_d2c_delivery_write_error"},
		{"release/short", release, directive.ReleaseClient("plaintext auth"), writeFallsShort, "release_d2c_delivery_short_write"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := newStashDeliveryHarness(t, tc.cfg, 0)
			if tc.cfg.PreDispatchPause {
				h.writeDest([]byte("greet"))
				awaitParkedOnPause(t, h.r, fakeconn.FromDest)
			} else {
				h.r.stashInflightFromPause(readFromDest(h.r, []byte("greet")))
			}
			h.failWrites.Store(tc.fail)

			h.r.Directives() <- tc.send
			a := h.ack(t, tc.send.Kind)
			if tc.cfg.PreDispatchPause && a.OK {
				t.Fatalf("resume acked OK though its write to the client failed: %+v", a)
			}
			select {
			case got := <-h.marked:
				if got != tc.reason {
					t.Fatalf("the mock was marked incomplete for %q, want %q", got, tc.reason)
				}
			default:
				t.Fatalf("the write to the client failed or fell short after the parser was given its bytes, and the mock was not marked incomplete")
			}
			if c := h.parserChunk(t); string(c.Bytes) != "greet" {
				t.Fatalf("the parser got %q, want greet", c.Bytes)
			}
		})
	}
}
