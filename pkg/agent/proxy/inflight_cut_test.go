package proxy

import (
	"strings"
	"testing"
)

// A read that spans the test's window's begin: the frame reader is busy (not
// reading) as a stream opened before the window sends on, the window begins
// with those bytes in the socket, and the test's own call (a new stream)
// arrives before the reader reads again, so one read returns both (a
// read-ahead reader: grpc-go's buffered reader, or a TLS layer under an
// HTTP/2 or gRPC connection). The test's stream is the test's: its end
// waits for it until it is answered, and the next begin with it.
func TestAReadSpanningTheWindowsBeginKeepsTheTestsStream(t *testing.T) {
	const bg, call = "DATA stream 1", "HEADERS stream 3"
	p := newStreamParser(t)
	if r := func() string { p.send("PING"); return <-p.got }(); r != "PING" {
		t.Fatalf("read %q", r)
	}
	p.send(bg)
	waitInq(t, p.proxy, len(bg))
	p.tr.beginWindow()
	p.send(call)
	waitInq(t, p.proxy, len(bg)+len(call))
	go readOnce(p.proxy, p.got)
	if r := <-p.got; r != bg+call {
		t.Fatalf("one read returned %q, want %q", r, bg+call)
	}
	release := p.proxy.Hold() // the test's own stream
	go readOnce(p.proxy, p.got)
	waitSettled(t, p.tr, func() bool { return p.proxy.reading.Load() == 1 })
	sc := p.tr.scope() // the test's end
	if n, _ := sc.State(true); n != 1 {
		t.Fatalf("lost call: the test's own stream, opened after its window began, is held and unanswered, but its end's scope pending = %d, want 1", n)
	}
	release()
	if n, _ := sc.State(true); n != 0 {
		t.Fatalf("answered: scope pending %d, want 0", n)
	}
}

// The stated limit of that read: a stream whose opening frame was in the
// socket as the window began, returned by one read with bytes sent after the
// begin, is the window's too, so the test's end waits for it until it is
// answered (a long-lived one costs the end a give-up: the test is not
// verified, never passed). One returned by a read that ends at the begin is
// not the window's (TestAStreamInTheSocketAsTheWindowBeganIsNotTheTests).
func TestAStreamInTheReadThatSpansTheWindowsBeginHoldsTheTestsEnd(t *testing.T) {
	const before, call = "HEADERS stream 1", "HEADERS stream 3"
	p := newStreamParser(t)
	if r := func() string { p.send("PING"); return <-p.got }(); r != "PING" {
		t.Fatalf("read %q", r)
	}
	p.send(before)
	waitInq(t, p.proxy, len(before))
	p.tr.beginWindow()
	p.send(call)
	waitInq(t, p.proxy, len(before)+len(call))
	go readOnce(p.proxy, p.got)
	if r := <-p.got; r != before+call {
		t.Fatalf("one read returned %q, want %q", r, before+call)
	}
	early := p.proxy.Hold()
	test := p.proxy.Hold()
	go readOnce(p.proxy, p.got)
	waitSettled(t, p.tr, func() bool { return p.proxy.reading.Load() == 1 })
	sc := p.tr.scope()
	if n, _ := sc.State(true); n != 1 {
		t.Fatalf("both streams held: scope pending %d, want 1", n)
	}
	test()
	if got := sc.PendingProgress(true)[p.proxy.id]; got != 1 {
		t.Fatalf("the test's stream answered: the end weighs %d of the connection's holds, want 1 (the stream in the read that spans its begin)", got)
	}
	early()
	if n, _ := sc.State(true); n != 0 {
		t.Fatalf("both answered: scope pending %d, want 0", n)
	}
}

// The same read spanning an end's cut: the test's last request is in the
// socket as its end cuts, a later stream (the next test's, or a background
// one) arrives before the reader reads, and one read returns both. The end's
// bound is fixed at the holds taken by the next read's begin, so it waits
// for the later stream too (the conservative side: never a lost call), and
// its watchdog names it.
func TestAReadSpanningTheEndsCutWaitsForTheLaterStreamToo(t *testing.T) {
	const call, later = "HEADERS stream 1", "HEADERS stream 3"
	p := newStreamParser(t)
	if r := func() string { p.send("PING"); return <-p.got }(); r != "PING" {
		t.Fatalf("read %q", r)
	}
	p.send(call)
	waitInq(t, p.proxy, len(call))
	sc := p.tr.scope() // the test's end, its call in the socket
	p.send(later)
	waitInq(t, p.proxy, len(call)+len(later))
	go readOnce(p.proxy, p.got)
	if r := <-p.got; r != call+later {
		t.Fatalf("one read returned %q", r)
	}
	test := p.proxy.Hold()
	next := p.proxy.Hold()
	go readOnce(p.proxy, p.got)
	waitSettled(t, p.tr, func() bool { return p.proxy.reading.Load() == 1 })
	test()
	if n, _ := sc.State(true); n != 1 {
		t.Fatalf("the test's stream answered, the later one read with it held: scope pending %d, want 1", n)
	}
	if words := sc.GiveUp([]uint64{p.proxy.id}, true); len(words) != 1 || !strings.Contains(words[0], "which held 1 request(s) its parser had read and not answered") {
		t.Fatalf("give-up words %q: want the later stream's 1 request", words)
	}
	next()
}

// A give-up on a multiplexed connection whose given-up stream is never
// answered (a long-lived one) gives up on that stream alone: each later test
// whose own stream on the connection was opened and answered in its window
// is verified, test after test, and one whose own stream is still held
// waits for it.
func TestAGiveUpOnANeverAnsweredStreamLeavesLaterTestsOnItsConnectionVerified(t *testing.T) {
	p := newStreamParser(t)
	p.tr.beginWindow()
	stuck := p.open("HEADERS stream 1", false) // the test's stream, never answered
	defer stuck()
	sc := p.tr.scope()
	if words := sc.GiveUp([]uint64{p.proxy.id}, true); len(words) != 1 {
		t.Fatalf("give-up words %q", words)
	}
	for i, s := range []string{"HEADERS stream 3", "HEADERS stream 5", "HEADERS stream 7"} {
		p.tr.beginWindow()
		own := p.open(s, false)
		held := p.tr.scope()
		if n, _ := held.State(true); n != 1 {
			t.Fatalf("later test %d: its own stream held: scope pending %d, want 1", i+1, n)
		}
		own() // the later test's own call, answered in its window
		later := p.tr.scope()
		n, _ := later.State(true)
		b, charge := later.Behind()
		if n != 0 || len(b) != 0 {
			t.Errorf("later test %d: its own stream answered: pending %d, behind %q, want 0 and none", i+1, n, b)
		}
		charge()
		if n, _ := held.State(true); n != 0 {
			t.Fatalf("later test %d: its own stream answered: the scope cut while it was held pending %d, want 0", i+1, n)
		}
	}
}

// The begin after an end that gave up on a stream it covered waits for it no
// more (the begin reads the end's scope again): it returns at once, while the
// stream is still held. A scope cut later in the same window, under the same
// floor, leaves the given-up stream out too, and waits for a stream opened
// since until it is answered.
func TestAGiveUpOnAStreamEndsWithItForTheNextBegin(t *testing.T) {
	p := newStreamParser(t)
	p.tr.beginWindow()
	stuck := p.open("HEADERS stream 1", false)
	defer stuck()
	sc := p.tr.scope() // the test's end
	if words := sc.GiveUp([]uint64{p.proxy.id}, true); len(words) != 1 {
		t.Fatalf("give-up words %q", words)
	}
	if n, _ := sc.State(true); n != 0 {
		t.Fatalf("the begin after the give-up: the end's scope pending %d, want 0 (the given-up stream still held)", n)
	}
	if got := sc.PendingProgress(true); len(got) != 0 {
		t.Fatalf("the begin's watchdog weighs %v, want nothing", got)
	}
	if b, _ := sc.Behind(); len(b) != 0 {
		t.Fatalf("behind %q, want none", b)
	}
	own := p.open("HEADERS stream 3", false)
	again := p.tr.scope()
	if n, _ := again.State(true); n != 1 {
		t.Fatalf("a stream opened after the give-up held: scope pending %d, want 1", n)
	}
	if got := again.PendingProgress(true)[p.proxy.id]; got != 1 {
		t.Fatalf("the watchdog weighs %d of the connection's holds, want 1 (the stream opened since)", got)
	}
	own()
	if n, _ := again.State(true); n != 0 {
		t.Fatalf("it answered: scope pending %d, want 0", n)
	}
	stuck()
	p.proxy.mu.Lock()
	kept := len(p.proxy.lostHolds)
	p.proxy.mu.Unlock()
	if kept != 0 {
		t.Fatalf("the given-up stream answered at last: %d given-up hold(s) still kept, want none", kept)
	}
}

// A give-up on a multiplexed connection whose client has hung up, its
// handler not having told it (pTls.HangUpHolder), gives up on all it held,
// the hang-up with its streams: the next begin does not wait for the
// hang-up again.
func TestAGiveUpOnAConnectionWhoseClientHungUpEndsWithAllItHeld(t *testing.T) {
	p := newStreamParser(t)
	p.tr.beginWindow()
	stuck := p.open("HEADERS stream 1", false)
	defer stuck()
	release := p.proxy.HoldHangUp()
	defer release()
	_ = p.app.Close()
	if r := <-p.got; r != "" {
		t.Fatalf("read %q, want the end of the stream", r)
	}
	sc := p.tr.scope() // the test's end
	words := sc.GiveUp([]uint64{p.proxy.id}, true)
	if len(words) != 1 || !strings.Contains(words[0], "its client's hang-up its handler had not told") {
		t.Fatalf("give-up words %q: want the stream and the hang-up", words)
	}
	if n, _ := sc.State(true); n != 0 {
		t.Fatalf("the begin after the give-up: the end's scope pending %d, want 0 (the hang-up and the stream given up on)", n)
	}
}

// The latch at exactly the cut: the scope cuts while the handler answers the
// test's request (its bytes all delivered); the handler goes back to reading
// at exactly the cut, holding nothing and no mark. The application's next
// request then arrives and its handler marks it unknown (a read-ahead
// group). The scope's own work was answered with no mark standing: the test
// is verified (no unknown).
func TestTheLatchAtExactlyTheCutIgnoresTheNextRequestsMark(t *testing.T) {
	tr := &inflight{}
	app, proxy := acceptedPair(t, tr)
	got := make(chan string, 1)
	go readOnce(proxy, got)
	if _, err := app.Write([]byte("req1")); err != nil {
		t.Fatal(err)
	}
	<-got
	sc := tr.scope() // cut at 4 bytes, the handler answering req1
	go readOnce(proxy, got)
	waitSettled(t, tr, func() bool { return proxy.reading.Load() == 1 })
	if _, err := app.Write([]byte("req2")); err != nil {
		t.Fatal(err)
	}
	<-got
	release := proxy.MarkUnknown("mongo")
	defer release()
	if n, unknown := sc.State(true); n != 0 || len(unknown) != 0 {
		t.Fatalf("the test's request answered at exactly the cut, the next one marked: pending %d unknown %q, want 0 and none", n, unknown)
	}
}
