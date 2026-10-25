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
	"testing/synctest"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/connseq"
	"go.keploy.io/server/v3/pkg/agent/proxy/directive"
	"go.keploy.io/server/v3/pkg/agent/proxy/fakeconn"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
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

// stashBound ends a run that is broken (a relay that never tees what it
// delivers, or writes before it tees, blocks in its Write below). The rows that
// use it run in a synctest bubble, where it is the bubble's clock: that moves
// only once every goroutine in the bubble is durably blocked, so a stuck relay
// fails at once, and a correct one, however slow, never reaches it. No verdict
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
// stamped as they were read, and with the time they were handed on as their
// WrittenAt (a recorder takes a response's WrittenAt as its mock's time).
// Written on untee'd, the app read bytes no parser saw and no report named.
//
// The capture watermark: stashed, the chunk held no window, as the client had
// not read it (TestWatermark_AForwarderParkedInAPauseHoldsNoWindowWithTheChunkItStashed).
// Once the client has read it, a window over its stamp is held until the
// parser has taken it: it reached the parser's queue before the client.
func TestAResumeTeesTheDestinationsStashBeforeTheClientReadsIt(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		h := newStashDeliveryHarness(t, Config{PreDispatchPause: true}, len("greet"))
		w := NewWatermark()
		defer w.Track(h.r, "conn-1")()

		// The destination speaks first: its forwarder reads the greeting,
		// stashes it and parks, so neither the client nor the parser has it.
		before := time.Now()
		h.writeDest([]byte("greet"))
		awaitParkedOnPause(t, h.r, fakeconn.FromDest)

		h.r.Directives() <- directive.ResumePreDispatch("parser-decided-no-tls")
		if a := h.ack(t, directive.KindResumePreDispatch); !a.OK {
			t.Fatalf("resume failed: %+v", a)
		}
		if got := <-h.clientGot; string(got) != "greet" {
			t.Fatalf("the client got %q, want greet", got)
		}
		if by := w.HeldBy("", "", before, time.Now(), 0); by == nil {
			t.Error("a window over the delivered greet is settled though the client has read it and the parser has not taken it")
		}
		c := h.parserChunk(t)
		if string(c.Bytes) != "greet" || c.Dir != fakeconn.FromDest {
			t.Fatalf("the parser got %v %q, want the destination's stashed greet", c.Dir, c.Bytes)
		}
		if c.ConnSeq == 0 || c.SeqNo == 0 || c.ReadAt.Before(before) {
			t.Fatalf("the stashed chunk reached the parser as ConnSeq %d SeqNo %d ReadAt %v: want it "+
				"numbered and stamped as it was read (after %v)", c.ConnSeq, c.SeqNo, c.ReadAt, before)
		}
		if c.WrittenAt.IsZero() || c.WrittenAt.Before(c.ReadAt) {
			t.Fatalf("the stashed chunk reached the parser with WrittenAt %v (ReadAt %v): want the time it was handed on",
				c.WrittenAt, c.ReadAt)
		}
	})
}

// A plain ReleaseClient delivers the destination's bytes stashed at the
// pause boundary to the client the same way: teed first, with its WrittenAt.
func TestAReleaseTeesTheDestinationsStashBeforeTheClientReadsIt(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		h := newStashDeliveryHarness(t, Config{HoldClientWrites: true}, len("late reply"))
		h.r.stashInflightFromPause(readFromDest(h.r, []byte("late reply")))

		h.r.Directives() <- directive.ReleaseClient("plaintext auth")
		if a := h.ack(t, directive.KindReleaseClient); !a.OK {
			t.Fatalf("release failed: %+v", a)
		}
		c := h.parserChunk(t)
		if string(c.Bytes) != "late reply" || c.ConnSeq == 0 || c.SeqNo == 0 {
			t.Fatalf("the parser got %q (ConnSeq %d, SeqNo %d), want the stashed late reply, numbered",
				c.Bytes, c.ConnSeq, c.SeqNo)
		}
		if c.WrittenAt.IsZero() || c.WrittenAt.Before(c.ReadAt) {
			t.Fatalf("the stashed chunk reached the parser with WrittenAt %v (ReadAt %v): want the time it was handed on",
				c.WrittenAt, c.ReadAt)
		}
		if got := <-h.clientGot; string(got) != "late reply" {
			t.Fatalf("the client got %q, want late reply", got)
		}
	})
}

// A destination chunk read after the client's FIN reaches the parser saying
// so (fakeconn.Chunk.AfterClientClose), stashed or not: a parser judges by it
// whether the client can have read the bytes. The stash keeps the flag the
// chunk was numbered with, and the delivery hands it on.
func TestAStashedDestinationChunkReadAfterTheClientsFINReachesTheParserSayingSo(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		h := newStashDeliveryHarness(t, Config{HoldClientWrites: true}, len("bye reply"))
		h.r.order.ClientClosed() // the client's FIN, numbered as its forwarder numbers it
		read := readFromDest(h.r, []byte("bye reply"))
		if !read.AfterClientClose {
			t.Fatal("a destination chunk numbered after the client's FIN does not say so")
		}
		h.r.stashInflightFromPause(read)

		h.r.Directives() <- directive.ReleaseClient("plaintext auth")
		if a := h.ack(t, directive.KindReleaseClient); !a.OK {
			t.Fatalf("release failed: %+v", a)
		}
		if c := h.parserChunk(t); string(c.Bytes) != "bye reply" || !c.AfterClientClose {
			t.Fatalf("the parser got %q with AfterClientClose %v, want the stashed bye reply saying it came after the client's FIN",
				c.Bytes, c.AfterClientClose)
		}
		<-h.clientGot
	})
}

// A stashed destination chunk is handed on as forward() hands on one it
// writes: the client's side decides whether the parser gets it
// (client_took.go). Once the client has stopped taking the stream
// (Config.ClientStopped), the chunk is refused: neither teed nor told of, and
// counted as not taken; it is still written. After the client's FIN, it is set
// aside, before it is written, until the client answers, and teed once the
// client says it took it. Teed at once, the parser would be given bytes past
// where its stream ended, or bytes a closed client may never read. A write
// that fails marks the mock incomplete as forward() marks it: not for a
// refused chunk, which is no part of the parser's stream, and for one set
// aside only once it is teed.
func TestAStashedDestinationChunkIsHandedOnAsTheClientsSideDecides(t *testing.T) {
	for _, tc := range []struct {
		name          string
		stopped, fail bool
	}{
		{"client stopped", true, false}, {"after the client's FIN", false, false},
		{"client stopped/write fails", true, true}, {"after the client's FIN/write fails", false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			synctest.Test(t, func(t *testing.T) {
				var carried atomic.Int32
				var last atomic.Pointer[time.Time]
				marks := make(chan string, 4)
				failWrites := new(atomic.Int32)
				if tc.fail {
					failWrites.Store(writeFails)
				}
				clientApp, srcProxy := net.Pipe()
				dstProxy, destSvc := net.Pipe()
				t.Cleanup(func() {
					for _, c := range []net.Conn{clientApp, srcProxy, dstProxy, destSvc} {
						_ = c.Close()
					}
				})
				r := New(Config{Logger: zap.NewNop(), OnCarried: func(dir fakeconn.Direction, at time.Time, teed bool, _ int64) {
					if dir == fakeconn.FromDest && teed {
						carried.Add(1)
						last.Store(&at)
					}
				}, OnMarkMockIncomplete: func(reason string) { marks <- reason },
				}, clientWriteFailer{Conn: srcProxy, fail: failWrites}, connseq.NewUpstream(dstProxy))
				t.Cleanup(func() { r.teeC2D.close(); r.teeD2C.close() }) // their drains; the relay is not run
				// The client's FIN reached the proxy, and nothing written to it
				// since has been taken; then it takes it all.
				sock := &scriptedSocket{script: []clientLook{finned(0), finned(64)}}
				r.client = sock
				r.clientFIN.Store(true)
				if tc.stopped {
					r.clientStopped.Store(true)
				}
				got := make(chan []byte, 1)
				go func() {
					buf := make([]byte, len("late reply"))
					if _, err := io.ReadFull(clientApp, buf); err == nil {
						got <- buf
					}
				}()

				read := fakeconn.Chunk{Dir: fakeconn.FromDest, Bytes: []byte("late reply"), ReadAt: time.Now()}
				read.ConnSeq, read.AfterClientClose = r.order.FromDest()
				r.stashInflightFromPause(read)
				err := r.deliverStashedToClient(t.Context(), r.takeStashedParts(fakeconn.FromDest), "release_d2c_delivery")
				if tc.fail != (err != nil) {
					t.Fatalf("delivery returned %v, want an error only for a failed write", err)
				}
				if !tc.fail {
					if b := <-got; string(b) != "late reply" {
						t.Fatalf("the client got %q, want late reply", b)
					}
				}
				synctest.Wait()
				if len(marks) != 0 {
					t.Fatalf("the mock was marked incomplete (%q) for a stashed chunk the parser has not been given", <-marks)
				}
				if tc.stopped {
					if n := carried.Load(); n != 0 {
						t.Fatalf("a stashed chunk the client refused was teed %d times: the parser's stream ended where the client stopped", n)
					}
					if n := r.notTakenChunks.Load(); n != 1 {
						t.Fatalf("%d chunks counted as not taken, want the refused one", n)
					}
					return
				}
				if n, aside := carried.Load(), r.teeD2C.awaitingLen(); n != 0 || aside != 1 {
					t.Fatalf("after the client's FIN, with no answer from it yet, the stashed chunk was teed %d times and %d set aside: "+
						"want it set aside until the client answers", n, aside)
				}
				time.Sleep(lastLookAgain) // the resolver's next look, on the bubble's clock
				synctest.Wait()
				if n, aside := carried.Load(), r.teeD2C.awaitingLen(); n != 1 || aside != 0 {
					t.Fatalf("once the client took it, the stashed chunk was teed %d times and %d still set aside: want it teed once", n, aside)
				}
				if at := last.Load(); at == nil || !at.Equal(read.ReadAt) {
					t.Fatalf("the stashed chunk was told of at %v, want its ReadAt %v", at, read.ReadAt)
				}
				if tc.fail {
					select {
					case m := <-marks:
						if m != "release_d2c_delivery_write_error" {
							t.Fatalf("the mock was marked incomplete for %q, want release_d2c_delivery_write_error", m)
						}
					default:
						t.Fatal("a stashed chunk whose write failed was teed once the client answered, and the mock was not marked incomplete")
					}
				}
			})
		})
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
			synctest.Test(t, func(t *testing.T) {
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
		})
	}
}

// With no client conn to write it to, a stashed destination chunk reaches
// neither the client nor the parser: the mock is marked incomplete
// (reason+"_write_error"), nothing is teed and nothing is written. The relay
// sets its client conn in New, before any forwarder runs, so a delivery meets
// no conn only if that order changes; this row holds the guard to it.
func TestAStashWithNoClientConnToDeliverItToMarksTheMockIncomplete(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var teed atomic.Int32
		marks := make(chan string, 2)
		dstProxy, destSvc := net.Pipe()
		t.Cleanup(func() { _ = dstProxy.Close(); _ = destSvc.Close() })
		r := New(Config{Logger: zap.NewNop(), OnCarried: func(_ fakeconn.Direction, _ time.Time, wasTeed bool, _ int64) {
			if wasTeed {
				teed.Add(1)
			}
		}, OnMarkMockIncomplete: func(reason string) { marks <- reason },
		}, nil, connseq.NewUpstream(dstProxy))
		t.Cleanup(func() { r.teeC2D.close(); r.teeD2C.close() }) // their drains; the relay is not run

		r.stashInflightFromPause(readFromDest(r, []byte("greet")))
		if err := r.deliverStashedToClient(t.Context(), r.takeStashedParts(fakeconn.FromDest), "release_d2c_delivery"); err == nil {
			t.Fatal("a stash with no client conn to write it to was delivered without an error")
		}
		synctest.Wait()
		select {
		case m := <-marks:
			if m != "release_d2c_delivery_write_error" {
				t.Fatalf("the mock was marked incomplete for %q, want release_d2c_delivery_write_error", m)
			}
		default:
			t.Fatal("a stash with no client conn to write it to was dropped, and the mock was not marked incomplete")
		}
		if n := teed.Load(); n != 0 {
			t.Fatalf("a stashed chunk no client could get was teed %d times", n)
		}
		if n := r.teeD2C.awaitingLen(); n != 0 {
			t.Fatalf("a stashed chunk no client could get was set aside (%d)", n)
		}
	})
}
