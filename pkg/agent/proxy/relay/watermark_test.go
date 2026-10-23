package relay

import (
	"net"
	"sync"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/fakeconn"
)

// awaitState polls cond until it holds: it waits only for a goroutine the test
// started to reach a state (a parser blocked in a read), and no assertion
// depends on how long that takes. The bound only ends a test that is broken.
func awaitState(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

// readChunk reads one chunk off a parser-facing stream, as a parser would.
func readChunk(t *testing.T, fc *fakeconn.FakeConn) fakeconn.Chunk {
	t.Helper()
	c, err := fc.ReadChunk()
	if err != nil {
		t.Fatalf("ReadChunk: %v", err)
	}
	return c
}

// blockInRead starts a reader on fc that blocks for the next chunk (a parser
// that has taken everything and waits for more), and waits until it does.
func blockInRead(t *testing.T, fc *fakeconn.FakeConn) {
	t.Helper()
	go func() { _, _ = fc.ReadChunk() }()
	awaitState(t, "the parser to block for more", func() bool { w, _ := fc.Waiting(); return w })
}

// A connection carried a request its parser has not got to (it is behind the
// traffic): the watermark holds every test case over it and says a stop may
// still get something from before it. Once the parser has taken the bytes and
// waits for more, it is done with them.
func TestWatermark_HoldsWhatAParserHasNotGotTo(t *testing.T) {
	h := newHarness(t, Config{})
	w := NewWatermark()
	untrack := w.Track(h.r, "conn-1")
	defer untrack()

	before := time.Now()
	h.writeClient([]byte("GET / HTTP/1.1\r\n\r\n"))
	h.readDest(18) // forwarded: the app's traffic does not wait on the parser
	after := time.Now()

	if !w.PendingBefore(after) {
		t.Fatal("PendingBefore: false while the parser had not read a request the connection carried before the time asked about")
	}
	if w.Settled("", "", before, after) {
		t.Fatal("Settled: true for a test case over a request its connection's parser had not got to")
	}
	if b := w.HeldBy("", "", before, after, 0); b == nil {
		t.Fatal("HeldBy: nil while the watermark held the test case")
	}
	// A time before the connection carried anything is behind no parser.
	if w.PendingBefore(before.Add(-time.Second)) {
		t.Fatal("PendingBefore: true for a time before the connection carried anything")
	}

	readChunk(t, h.r.ClientStream())
	blockInRead(t, h.r.ClientStream())
	if w.PendingBefore(after) {
		t.Fatalf("PendingBefore: true once the parser had taken everything and waited for more: %v", w.PendingOn(after))
	}
	if !w.Settled("", "", before, after) {
		t.Fatalf("Settled: false once the parser was done: %v", w.HeldBy("", "", before, after, 0))
	}
}

// A parser that has taken a chunk from after a time is done with what came
// before it, though newer bytes wait for it.
func TestWatermark_AParserPastATimeIsDoneWithIt(t *testing.T) {
	h := newHarness(t, Config{})
	w := NewWatermark()
	defer w.Track(h.r, "conn-1")()

	h.writeClient([]byte("one"))
	h.readDest(3)
	mid := time.Now()
	time.Sleep(2 * time.Millisecond) // the next chunk is stamped after mid
	h.writeClient([]byte("two"))
	h.readDest(3)
	h.writeClient([]byte("three"))
	h.readDest(5)

	if !w.PendingBefore(mid) {
		t.Fatal("PendingBefore(mid): false with the chunk from before mid unread")
	}
	readChunk(t, h.r.ClientStream()) // "one"
	// "one" is consumed, but the parser has neither taken "two" nor waits:
	// it is still working on "one", from before mid.
	if !w.PendingBefore(mid) {
		t.Fatal("PendingBefore(mid): false while the parser was still working on the chunk from before mid")
	}
	c := readChunk(t, h.r.ClientStream()) // "two", stamped after mid
	if !c.ReadAt.After(mid) {
		t.Fatalf("test setup: the second chunk was stamped %v, not after %v", c.ReadAt, mid)
	}
	if w.PendingBefore(mid) {
		t.Fatalf("PendingBefore(mid): true once the parser took a chunk from after it: %v", w.PendingOn(mid))
	}
	if !w.PendingBefore(time.Now()) {
		t.Fatal("PendingBefore(now): false with \"three\" unread")
	}
}

// A connection that carried nothing within a test case's window does not hold
// it back, whatever its parser is doing.
func TestWatermark_AConnectionIdleOverAWindowDoesNotHoldIt(t *testing.T) {
	h := newHarness(t, Config{})
	w := NewWatermark()
	defer w.Track(h.r, "conn-1")()

	h.writeClient([]byte("early"))
	h.readDest(5) // never read by the parser
	time.Sleep(2 * time.Millisecond)
	start := time.Now()
	end := start.Add(time.Millisecond)
	if !w.Settled("", "", start, end) {
		t.Fatalf("Settled: false for a window the connection carried nothing in: %v", w.HeldBy("", "", start, end, 0))
	}
}

// parkedDestForwarder is a Config whose destination forwarder parks with the
// first chunk it has written on, before it queues the chunk for the parser
// (beforeTee): holding is closed as it parks there, and it goes on, with that
// chunk and every later one, once the test closes release.
func parkedDestForwarder() (cfg Config, holding <-chan struct{}, release chan struct{}) {
	parked := make(chan struct{})
	release = make(chan struct{})
	var once sync.Once
	cfg = Config{beforeTee: func(dir fakeconn.Direction) {
		if dir == fakeconn.FromDest {
			once.Do(func() { close(parked) })
			<-release
		}
	}}
	return cfg, parked, release
}

// A chunk the forwarder has read and forwarded, and not yet queued for its
// parser, holds the watermark: a test case that used the bytes completed after
// the forwarder wrote them on, and is held until the parser has them.
func TestWatermark_HoldsAChunkInTheForwardersHand(t *testing.T) {
	cfg, holding, release := parkedDestForwarder()
	h := newHarness(t, cfg)
	defer close(release)
	w := NewWatermark()
	defer w.Track(h.r, "conn-1")()

	h.writeDest([]byte("answer"))
	h.readClient(6) // written on: the app has it
	<-holding
	got := time.Now()
	if !w.PendingBefore(got) {
		t.Fatal("PendingBefore: false while the forwarder still held a chunk it had written on")
	}
	if w.Settled("", "", got.Add(-time.Second), got) {
		t.Fatal("Settled: true for a test case over a chunk still in the forwarder's hand")
	}
}

// A connection that has carried nothing is behind no time and holds no test
// case, even one whose window has no start, though no parser waits on it.
func TestWatermark_AConnectionThatCarriedNothingHoldsNothing(t *testing.T) {
	h := newHarness(t, Config{})
	w := NewWatermark()
	defer w.Track(h.r, "conn-1")()

	now := time.Now()
	if w.PendingBefore(now) {
		t.Fatalf("PendingBefore: true for a connection that has carried nothing: %v", w.PendingOn(now))
	}
	if !w.Settled("", "", time.Time{}, now) {
		t.Fatalf("Settled: false for a connection that has carried nothing: %v", w.HeldBy("", "", time.Time{}, now, 0))
	}
}

// A connection's newest chunk counts on either direction: a window that starts
// after every chunk the client sent and covers an answer the parser has not got
// to is held by that answer.
func TestWatermark_AnAnswerNewerThanEveryRequestHoldsAWindowOverIt(t *testing.T) {
	h := newHarness(t, Config{})
	w := NewWatermark()
	defer w.Track(h.r, "conn-1")()

	h.writeClient([]byte("req"))
	h.readDest(3)
	h.writeDest([]byte("resp"))
	h.readClient(4)
	h.forwardersBackInRead() // neither forwarder holds a chunk: the answer is queued
	sent, answered := h.r.teeC2D.progress.last.Load(), h.r.teeD2C.progress.last.Load()
	if answered <= sent {
		t.Fatalf("test setup: the answer was stamped %d, not after the request's %d", answered, sent)
	}
	start, end := time.Unix(0, sent+1), time.Unix(0, answered)
	if w.Settled("", "", start, end) {
		t.Fatal("Settled: true for a window over an answer its connection's parser had not got to")
	}
}

// A chunk in the forwarder's hand counts by its stamp. A forwarder that keeps
// one from before a test case's window (its Write blocked on a full peer) does
// not hold the test case, though the parser has not got the chunk; a window
// that covers the stamp is held by it.
func TestWatermark_AChunkInTheForwardersHandFromBeforeAWindowDoesNotHoldIt(t *testing.T) {
	cfg, holding, release := parkedDestForwarder()
	h := newHarness(t, cfg)
	defer close(release)
	w := NewWatermark()
	defer w.Track(h.r, "conn-1")()

	h.writeDest([]byte("answer"))
	h.readClient(6) // written on: the app has it
	<-holding
	stamp := h.r.teeD2C.progress.held.Load()
	if stamp <= 0 {
		t.Fatalf("test setup: the forwarder holds a chunk with stamp %d", stamp)
	}
	start := time.Unix(0, stamp+1)
	end := start.Add(time.Millisecond)
	if !w.Settled("", "", start, end) {
		t.Fatalf("Settled: false for a window after the only chunk the connection carried, still in the forwarder's hand: %v",
			w.HeldBy("", "", start, end, 0))
	}
	if w.Settled("", "", time.Unix(0, stamp), end) {
		t.Fatal("Settled: true for a window that covers the stamp of a chunk still in the forwarder's hand")
	}
}

// parkedStampConn parks the forwarder that reads it as it stamps a chunk: the
// relay asks the conn it read from for the read's time (observedReadAt) after
// it took the chunk in hand and before it stamps it (heldUnstamped). stamping
// gets a value as the forwarder parks there; it goes on, the chunk stamped
// now, once the test closes release.
type parkedStampConn struct {
	net.Conn
	stamping chan struct{}
	release  chan struct{}
}

func (c parkedStampConn) LastReadTime() time.Time {
	c.stamping <- struct{}{}
	<-c.release
	return time.Time{}
}

// A chunk in a forwarder's hand that it has not stamped yet holds every window,
// even one from before anything the connection carried: its stamp is not known
// yet. The client forwarder is parked there through the conn it read the chunk
// from.
func TestWatermark_AnUnstampedChunkInTheForwardersHandHoldsEveryWindow(t *testing.T) {
	src := parkedStampConn{stamping: make(chan struct{}, 1), release: make(chan struct{})}
	h := newHarnessOver(t, Config{}, func(c net.Conn) net.Conn { src.Conn = c; return src })
	t.Cleanup(func() { close(src.release) }) // before the harness's: Run waits for the forwarder
	w := NewWatermark()
	defer w.Track(h.r, "conn-1")()

	start := time.Now().Add(-time.Hour)
	end := start.Add(time.Millisecond)
	go func() { _, _ = h.clientApp.Write([]byte("REQ1")) }()
	<-src.stamping
	if got := h.r.teeC2D.progress.held.Load(); got != heldUnstamped {
		t.Fatalf("test setup: held = %d with the forwarder parked as it stamps, want heldUnstamped", got)
	}
	if w.Settled("", "", start, end) {
		t.Fatal("Settled: true for a window while a chunk not yet stamped is in the forwarder's hand")
	}
}

// forwardersBackInRead returns once both forwarders are back in a Read, so
// neither still holds a chunk it queued: an empty write on a net.Pipe returns
// only once the other end's Read took it, a forwarder reads again only after
// it let go of its last chunk, and an empty read moves nothing.
func (h *relayHarness) forwardersBackInRead() {
	h.t.Helper()
	for _, c := range []net.Conn{h.clientApp, h.destSvc} {
		if _, err := c.Write(nil); err != nil {
			h.t.Fatalf("empty write: %v", err)
		}
	}
}

// A parser that reads ahead of itself (fakeconn.FakeConn.ReadAhead) is done
// with what its reader took only once its loop has marked it processed: a
// reader that took everything and waits for more says nothing of the loop.
// The loop working through what was read ahead is progress. The forwarder
// lets go of the chunk once it has queued it, so a reader that took it is all
// that holds it, even while the forwarder is still busy with it after that.
// That holds on each of the forwarder's three ways to queue a client chunk:
// forwarded, held from the destination, and stashed in a pre-dispatch pause.
func TestWatermark_AParserReadingAheadIsDoneOnlyWithWhatItProcessed(t *testing.T) {
	for _, tc := range []struct {
		name      string
		cfg       Config
		forwarded bool // the chunk reaches the destination before it is queued
	}{
		{"forwarded", Config{}, true},
		{"held", Config{HoldClientWrites: true}, false},
		{"pre-dispatch", Config{PreDispatchPause: true}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var readerDone chan struct{}
			t.Cleanup(func() { // after the harness's: the streams' close ends the read
				if readerDone != nil {
					<-readerDone
				}
			})
			teed, resume := make(chan struct{}, 1), make(chan struct{})
			cfg := tc.cfg
			cfg.OnClientChunkTeed = func() {
				teed <- struct{}{}
				<-resume
			}
			h := newHarness(t, cfg)
			t.Cleanup(func() { close(resume) }) // before the harness's: its Run waits for the forwarder
			w := NewWatermark()
			defer w.Track(h.r, "conn-1")()
			client := h.r.ClientStream()

			h.writeClient([]byte("GET / HTTP/1.1\r\n\r\n"))
			if tc.forwarded {
				h.readDest(18)
			}
			<-teed // the forwarder queued the chunk, and stays busy after it
			after := time.Now()
			c, err := client.ReadAhead()
			if err != nil {
				t.Fatalf("ReadAhead: %v", err)
			}
			// Told of the reader's next wait, not of one the read above made.
			blocked := make(chan struct{}, 1)
			defer fakeconn.BeforeWaitForTest(func(f *fakeconn.FakeConn) {
				if f == client {
					select {
					case blocked <- struct{}{}:
					default:
					}
				}
			})()
			readerDone = make(chan struct{})
			go func() {
				defer close(readerDone)
				_, _ = client.ReadAhead()
			}()
			<-blocked // the reader took everything and waits for more

			if !w.PendingBefore(after) {
				t.Fatal("PendingBefore: false while the parser's loop had not processed the request its reader took")
			}
			p0 := w.Progress()
			client.MarkProcessed(c)
			if p1 := w.Progress(); p1 <= p0 {
				t.Fatalf("Progress went from %d to %d as the loop processed what its reader took; it must move", p0, p1)
			}
			if w.PendingBefore(after) {
				t.Fatalf("PendingBefore: true once the loop processed all its reader took and the reader waited for more: %v",
					w.PendingOn(after))
			}
		})
	}
}

// A parser that reads ahead takes its two streams' chunks in no order of their
// stamps: past a time on one stream, with a chunk from before it read ahead
// and not processed on the other, it is not done with that time.
func TestWatermark_AParserReadingAheadPastATimeOnOneStreamIsNotDoneWithTheOther(t *testing.T) {
	h := newHarness(t, Config{})
	w := NewWatermark()
	defer w.Track(h.r, "conn-1")()
	client, dest := h.r.ClientStream(), h.r.DestStream()
	readAhead := func(s *fakeconn.FakeConn) fakeconn.Chunk {
		t.Helper()
		c, err := s.ReadAhead()
		if err != nil {
			t.Fatalf("ReadAhead: %v", err)
		}
		return c
	}

	h.writeClient([]byte("REQ1"))
	h.readDest(4)
	client.MarkProcessed(readAhead(client))
	h.writeDest([]byte("RES1"))
	h.readClient(4)
	res := readAhead(dest) // queued behind the next request, not processed
	h.writeClient([]byte("REQ2"))
	h.readDest(4)
	req := readAhead(client)
	h.forwardersBackInRead()
	if !req.ReadAt.After(res.ReadAt) {
		t.Fatalf("test setup: the second request was stamped %v, not after the answer's %v", req.ReadAt, res.ReadAt)
	}
	client.MarkProcessed(req)

	if !w.PendingBefore(res.ReadAt) {
		t.Fatal("PendingBefore: false while the answer from then was read ahead and not processed")
	}
	dest.MarkProcessed(res)
	if w.PendingBefore(res.ReadAt) {
		t.Fatalf("PendingBefore: true once the answer was processed: %v", w.PendingOn(res.ReadAt))
	}
}

// waitingReader starts a reader that reads ahead on s and returns once it
// waits for more with until true (nil: at its first wait); its read ends as the
// test ends, which closes s. It fails if the read returns without waiting, and
// waits for good if until never holds at a wait (the tee never delivers).
func waitingReader(t *testing.T, s *fakeconn.FakeConn, until func() bool) {
	t.Helper()
	blocked := make(chan struct{}, 1)
	defer fakeconn.BeforeWaitForTest(func(f *fakeconn.FakeConn) {
		if f == s && (until == nil || until()) {
			select {
			case blocked <- struct{}{}:
			default:
			}
		}
	})()
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = s.ReadAhead()
	}()
	t.Cleanup(func() { // before the harness's, which waits for nothing of it
		_ = s.Close()
		<-done
	})
	select {
	case <-blocked:
	case <-done:
		t.Fatal("test setup: the reader returned without waiting for more")
	}
}

// A parser that reads ahead is done with a time once it has processed every
// chunk from up to then, though it has a later chunk read ahead and queued and
// its loop has not got past the time on that stream: a loop that processed a
// request and waits on the destination's answer, with the next request queued
// behind it, is done with a time between the two requests.
func TestWatermark_AParserReadingAheadIsDoneWithATimeBeforeWhatItHasQueued(t *testing.T) {
	h := newHarness(t, Config{})
	w := NewWatermark()
	defer w.Track(h.r, "conn-1")()
	client, dest := h.r.ClientStream(), h.r.DestStream()

	h.writeClient([]byte("REQ1"))
	h.readDest(4)
	first, err := client.ReadAhead()
	if err != nil {
		t.Fatalf("ReadAhead: %v", err)
	}
	client.MarkProcessed(first)
	mid := time.Now()
	h.writeClient([]byte("REQ2"))
	h.readDest(4)
	queued, err := client.ReadAhead() // queued for the loop, not processed
	if err != nil {
		t.Fatalf("ReadAhead: %v", err)
	}
	if first.ReadAt.After(mid) || !queued.ReadAt.After(mid) {
		t.Fatalf("test setup: the requests were stamped %v and %v, not either side of %v", first.ReadAt, queued.ReadAt, mid)
	}
	waitingReader(t, client, nil)
	waitingReader(t, dest, nil) // the destination's answer has not come
	h.forwardersBackInRead()

	if w.PendingBefore(mid) {
		t.Fatalf("PendingBefore: true with every chunk from then processed: %v", w.PendingOn(mid))
	}
	if !w.PendingBefore(queued.ReadAt) {
		t.Fatal("PendingBefore: false while the request from then was read ahead and not processed")
	}
	client.MarkProcessed(queued)
	if w.PendingBefore(queued.ReadAt) {
		t.Fatalf("PendingBefore: true once the request was processed: %v", w.PendingOn(queued.ReadAt))
	}
}

// Bytes swallowed for a parser that reads ahead (DiscardBefore), behind a
// chunk it read ahead and has not processed, count processed before that
// chunk does, and ProcessedThrough cannot say exactly how far it is: the
// parser is not done with the chunk's time for them.
func TestWatermark_AParserReadingAheadIsNotDoneWithAChunkSwallowedBytesFollow(t *testing.T) {
	h := newHarness(t, Config{})
	w := NewWatermark()
	defer w.Track(h.r, "conn-1")()
	client := h.r.ClientStream()

	h.writeClient([]byte("REQ1"))
	h.readDest(4)
	h.writeClient([]byte("SKIP"))
	h.readDest(4)
	first, err := client.ReadAhead()
	if err != nil {
		t.Fatalf("ReadAhead: %v", err)
	}
	client.DiscardBefore(8)
	// Swallows "SKIP" once the tee has delivered it, then waits for more.
	waitingReader(t, client, func() bool { return client.Consumed() == 8 })
	h.forwardersBackInRead()
	if through, exact := client.ProcessedThrough(); exact {
		t.Fatalf("test setup: ProcessedThrough = %d, exact, past a discard behind a chunk read ahead", through)
	}

	if !w.PendingBefore(first.ReadAt) {
		t.Fatal("PendingBefore: false while the chunk from then was read ahead and not processed, behind swallowed bytes")
	}
	client.MarkProcessed(first)
	if w.PendingBefore(first.ReadAt) {
		t.Fatalf("PendingBefore: true once the chunk was processed: %v", w.PendingOn(first.ReadAt))
	}
}

// A parser that reads ahead, on a stream where ProcessedThrough cannot say
// exactly how far it is and never can again while it stays a chunk ahead of
// its loop, is done with a time once its loop has processed a chunk from after
// it on that stream, and not before.
func TestWatermark_AParserReadingAheadThatCannotSayExactlyIsDoneWithATimeItProcessedPast(t *testing.T) {
	h := newHarness(t, Config{})
	w := NewWatermark()
	defer w.Track(h.r, "conn-1")()
	client := h.r.ClientStream()
	readAhead := func() fakeconn.Chunk {
		t.Helper()
		c, err := client.ReadAhead()
		if err != nil {
			t.Fatalf("ReadAhead: %v", err)
		}
		return c
	}
	notExact := func(when string) {
		t.Helper()
		if through, exact := client.ProcessedThrough(); exact {
			t.Fatalf("test setup: ProcessedThrough = %d, exact, %s", through, when)
		}
	}

	for _, b := range []string{"REQ1", "SKIP", "REQ3"} {
		h.writeClient([]byte(b))
		h.readDest(4)
	}
	first := readAhead()
	client.DiscardBefore(8)
	third := readAhead() // swallows "SKIP" behind "REQ1", read ahead and not processed
	notExact("past a discard behind a chunk read ahead")
	if !third.ReadAt.After(first.ReadAt) {
		t.Fatalf("test setup: the third request was stamped %v, not after the first's %v", third.ReadAt, first.ReadAt)
	}
	if !w.PendingBefore(first.ReadAt) {
		t.Fatal("PendingBefore: false while the loop had processed nothing past the first request")
	}
	client.MarkProcessed(first)
	h.writeClient([]byte("REQ4"))
	h.readDest(4)
	fourth := readAhead() // the reader stays a chunk ahead of the loop
	client.MarkProcessed(third)
	h.forwardersBackInRead()
	notExact("with a chunk read ahead and not processed since the discard")

	if w.PendingBefore(first.ReadAt) {
		t.Fatalf("PendingBefore: true once the loop processed a chunk from after it: %v", w.PendingOn(first.ReadAt))
	}
	if !w.PendingBefore(fourth.ReadAt) {
		t.Fatal("PendingBefore: false while the request from then was read ahead and not processed")
	}
}

// notExactAhead writes three chunks on one direction of h (write and read
// carry them across), reads the first ahead on s, discards the second behind
// it and reads the third ahead: s then cannot say exactly how far its parser
// is, as in the test above, and its loop has processed nothing.
func notExactAhead(t *testing.T, s *fakeconn.FakeConn, write func([]byte), read func(int) []byte) (first, third fakeconn.Chunk) {
	t.Helper()
	for _, b := range []string{"MSG1", "SKIP", "MSG3"} {
		write([]byte(b))
		read(4)
	}
	var err error
	if first, err = s.ReadAhead(); err != nil {
		t.Fatalf("ReadAhead: %v", err)
	}
	s.DiscardBefore(8)
	if third, err = s.ReadAhead(); err != nil {
		t.Fatalf("ReadAhead: %v", err)
	}
	if through, exact := s.ProcessedThrough(); exact {
		t.Fatalf("test setup: ProcessedThrough = %d, exact, past a discard behind a chunk read ahead", through)
	}
	return first, third
}

// On a stream that cannot say exactly, a chunk stamped at t that is read ahead
// and not processed holds t, though the loop processed one stamped at t too:
// the loop is done with t only once it has processed a chunk from after t. The
// client's conn gives every read one stamp, as a TLS layer can.
func TestWatermark_ANotExactStreamWithAChunkStampedAtTheTimeUnprocessedHoldsIt(t *testing.T) {
	at := time.Now().Add(-time.Hour)
	h := newHarnessOver(t, Config{}, func(c net.Conn) net.Conn { return fixedReadAtConn{Conn: c, readAt: at} })
	w := NewWatermark()
	defer w.Track(h.r, "conn-1")()
	client := h.r.ClientStream()
	first, third := notExactAhead(t, client, h.writeClient, h.readDest)
	if !first.ReadAt.Equal(at) || !third.ReadAt.Equal(at) {
		t.Fatalf("test setup: stamps %v and %v, want both %v", first.ReadAt, third.ReadAt, at)
	}
	client.MarkProcessed(first)
	waitingReader(t, client, nil) // and so caught up, but for the loop
	h.forwardersBackInRead()
	if !w.PendingBefore(at) {
		t.Fatal("PendingBefore: false with a chunk stamped at that time read ahead and not processed")
	}
}

// A stream that cannot say exactly is judged by the last chunk its own loop
// processed: the client's loop has processed nothing, and the other stream's
// loop processing an answer from after t says nothing of it.
func TestWatermark_ANotExactStreamIsJudgedByItsOwnLastProcessedChunk(t *testing.T) {
	h := newHarness(t, Config{})
	w := NewWatermark()
	defer w.Track(h.r, "conn-1")()
	client, dest := h.r.ClientStream(), h.r.DestStream()
	first, _ := notExactAhead(t, client, h.writeClient, h.readDest)
	h.writeDest([]byte("RES1"))
	h.readClient(4)
	if c := readChunk(t, dest); !c.ReadAt.After(first.ReadAt) {
		t.Fatalf("test setup: the answer was stamped %v, not after the request's %v", c.ReadAt, first.ReadAt)
	}
	waitingReader(t, client, nil)
	h.forwardersBackInRead()
	if !w.PendingBefore(first.ReadAt) {
		t.Fatal("PendingBefore: false while the client's loop had processed nothing, and the request from then was read ahead")
	}
}

// The destination's stream is judged as the client's is: one that cannot say
// exactly, with nothing from after t processed, holds t, though the client's
// stream is exact and past t.
func TestWatermark_ANotExactDestinationStreamHoldsWhatItsLoopHasNotProcessedPast(t *testing.T) {
	h := newHarness(t, Config{})
	w := NewWatermark()
	defer w.Track(h.r, "conn-1")()
	client, dest := h.r.ClientStream(), h.r.DestStream()
	first, third := notExactAhead(t, dest, h.writeDest, h.readClient)
	h.writeClient([]byte("REQ9"))
	h.readDest(4)
	if c := readChunk(t, client); !c.ReadAt.After(third.ReadAt) {
		t.Fatalf("test setup: the request was stamped %v, not after the answers' %v", c.ReadAt, third.ReadAt)
	}
	h.forwardersBackInRead()
	if !w.PendingBefore(first.ReadAt) {
		t.Fatal("PendingBefore: false while the destination's loop had processed nothing, and the answer from then was read ahead")
	}
	dest.MarkProcessed(first)
	dest.MarkProcessed(third)
	if w.PendingBefore(first.ReadAt) {
		t.Fatalf("PendingBefore: true once the destination's loop processed all it read ahead: %v", w.PendingOn(first.ReadAt))
	}
}

// A chunk delivered to a parser that reads synchronously, below the discard
// watermark (DiscardBefore, as for a TLS upgrade's handshake bytes), is never
// handed to the parser: it holds no time back while it waits to be swallowed.
func TestWatermark_ASynchronousParserIsDoneWithAChunkBelowTheDiscardWatermark(t *testing.T) {
	h := newHarness(t, Config{})
	w := NewWatermark()
	defer w.Track(h.r, "conn-1")()
	client, dest := h.r.ClientStream(), h.r.DestStream()

	h.writeClient([]byte("HELLO"))
	h.readDest(5)
	hello, err := client.Peek() // delivered to the FakeConn, not read
	if err != nil {
		t.Fatalf("Peek: %v", err)
	}
	h.writeDest([]byte("X"))
	h.readClient(1)
	answer := readChunk(t, dest)
	if !answer.ReadAt.After(hello.ReadAt) {
		t.Fatalf("test setup: the answer was stamped %v, not after %v", answer.ReadAt, hello.ReadAt)
	}
	h.forwardersBackInRead()
	if !w.PendingBefore(hello.ReadAt) {
		t.Fatal("PendingBefore: false while the parser had not read the chunk from then")
	}
	client.DiscardBefore(5)
	if w.PendingBefore(hello.ReadAt) {
		t.Fatalf("PendingBefore: true for a chunk below the discard watermark: %v", w.PendingOn(hello.ReadAt))
	}
}

// A parser that returned is no longer followed, and its progress is counted.
func TestWatermark_UntrackAndProgress(t *testing.T) {
	h := newHarness(t, Config{})
	w := NewWatermark()
	untrack := w.Track(h.r, "conn-1")
	h.writeClient([]byte("abc"))
	h.readDest(3)
	p0 := w.Progress()
	readChunk(t, h.r.ClientStream())
	p1 := w.Progress()
	if p1 == p0 {
		t.Fatal("Progress did not move as the parser consumed bytes")
	}
	h.writeClient([]byte("unread"))
	h.readDest(6)
	if !w.PendingBefore(time.Now()) {
		t.Fatal("PendingBefore: false with bytes unread")
	}
	untrack()
	untrack() // idempotent
	if w.Following() != 0 {
		t.Fatalf("Following: %d after untrack", w.Following())
	}
	if w.PendingBefore(time.Now()) {
		t.Fatal("PendingBefore: true for a connection whose parser returned")
	}
	if p2 := w.Progress(); p2 <= p1 {
		t.Fatalf("Progress went from %d to %d as the parser returned; it must move", p1, p2)
	}
}

// ReaderLag delivers each chunk to the parser no earlier than the lag after it
// was read, and the watermark holds what is lagging.
func TestWatermark_ReaderLagHoldsTheParserBehind(t *testing.T) {
	const lag = 300 * time.Millisecond
	h := newHarness(t, Config{ReaderLag: lag})
	w := NewWatermark()
	defer w.Track(h.r, "conn-1")()

	h.writeClient([]byte("late"))
	h.readDest(4) // the application is not delayed
	if !w.PendingBefore(time.Now()) {
		t.Fatal("PendingBefore: false while the chunk lagged on its way to the parser")
	}
	c := readChunk(t, h.r.ClientStream())
	if d := time.Since(c.ReadAt); d < lag {
		t.Fatalf("the chunk reached the parser %v after it was read, want at least %v", d, lag)
	}
}

// A response chunk from before mid that the drain has taken off the queue
// (delivered) but the parser has not consumed, while the parser has already
// taken a later client chunk: only the delivered list sees the response, and
// without it the watermark says the parser is past mid.
func TestWatermark_ADeliveredChunkOnTheOtherDirectionHoldsIt(t *testing.T) {
	h := newHarness(t, Config{})
	w := NewWatermark()
	defer w.Track(h.r, "conn-1")()

	h.writeDest([]byte("resp"))
	h.readClient(4)
	mid := time.Now()
	time.Sleep(2 * time.Millisecond) // the next chunk is stamped after mid
	h.writeClient([]byte("next"))
	h.readDest(4)
	// The drain hands the response to the parser's stream at once (it is
	// delivered, not queued); the parser reads only the client direction.
	awaitState(t, "the response to leave the tee's queue", func() bool {
		h.r.teeD2C.mu.Lock()
		defer h.r.teeD2C.mu.Unlock()
		return len(h.r.teeD2C.q) == 0
	})
	c := readChunk(t, h.r.ClientStream())
	if !c.ReadAt.After(mid) {
		t.Fatalf("setup: client chunk stamped %v, not after %v", c.ReadAt, mid)
	}
	if !w.PendingBefore(mid) {
		t.Fatal("PendingBefore(mid) = false with a response from before mid delivered but never consumed")
	}
	if w.Settled("", "", mid.Add(-time.Second), mid) {
		t.Fatal("Settled = true for a window whose response the parser never consumed")
	}
}

// A parser that took a request from before mid and now waits on the client
// while the response, from after mid, waits for it on the other direction has
// not consumed everything the connection carried: it may still be building
// the exchange it took before mid, so it is not done with mid.
func TestWatermark_AParserWaitingWithTheOtherDirectionUnreadIsNotDone(t *testing.T) {
	h := newHarness(t, Config{})
	w := NewWatermark()
	defer w.Track(h.r, "conn-1")()

	h.writeClient([]byte("req"))
	h.readDest(3)
	readChunk(t, h.r.ClientStream())
	mid := time.Now()
	time.Sleep(2 * time.Millisecond) // the response is stamped after mid
	h.writeDest([]byte("resp"))
	h.readClient(4)
	awaitState(t, "the response to reach the parser's stream", func() bool {
		return h.r.teeD2C.acceptedBytes() == 4
	})
	blockInRead(t, h.r.ClientStream())
	if !w.PendingBefore(mid) {
		t.Fatal("PendingBefore(mid) = false while the parser waits on the client with the response to the request it took before mid unread")
	}
}
