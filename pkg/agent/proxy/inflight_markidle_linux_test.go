//go:build linux

package proxy

import (
	"context"
	"io"
	"testing"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
)

// A parser that waits out an answer the dependency held idle (a long poll
// that has caught up with the recording: SQS waits WaitTimeSeconds less half
// a second before it answers a ReceiveMessage that finds no message) is not
// blocked reading, so its connection held every test's end for the whole wait:
// in the sqs-localstack lane each test ended about 3.5 s after its call was
// answered, and the consumer's next poll began between tests. Marked idle
// (integrations.MarkIdle), the connection is settled for the wait; released,
// the handler is answering, and the connection is pending until it reads
// again. A record waits on no answer either way, and a mark changes nothing
// there.
func TestAConnectionWaitingOutAnIdleAnswerIsSettled(t *testing.T) {
	tr := &inflight{}
	app, proxy := acceptedPair(t, tr)
	read := make(chan string)
	again := make(chan struct{})
	go func() {
		buf := make([]byte, 64)
		n, err := proxy.Read(buf)
		read <- string(buf[:n])
		if err != nil {
			return
		}
		<-again
		_, _ = proxy.Read(buf)
	}()
	if _, err := io.WriteString(app, "ReceiveMessage"); err != nil {
		t.Fatal(err)
	}
	if got := <-read; got != "ReceiveMessage" {
		t.Fatalf("read %q", got)
	}
	if n := tr.Pending(true); n != 1 {
		t.Fatalf("a request read, its answer not chosen: pending %d, want 1", n)
	}

	ctx := integrations.WithRequestHolder(context.Background(), proxy)
	changed := tr.Changed()
	before := tr.Progress()
	release := integrations.MarkIdle(ctx)
	<-changed
	if tr.Progress() == before {
		t.Fatal("an idle mark moved no progress: a wait would not read the connection again")
	}
	if n := tr.Pending(true); n != 0 {
		t.Fatalf("waiting out an idle answer: pending %d, want 0", n)
	}
	if pending, unknown := tr.State(true); pending != 0 || len(unknown) != 0 {
		t.Fatalf("waiting out an idle answer: state %d %v, want 0 and known", pending, unknown)
	}
	if got := tr.PendingProgress(true); len(got) != 0 {
		t.Fatalf("waiting out an idle answer: the watchdog watches %v, want nothing", got)
	}

	inner := integrations.MarkIdle(ctx)
	release()
	release() // a second release changes nothing
	if n := tr.Pending(true); n != 0 {
		t.Fatalf("one of two idle marks released: pending %d, want 0", n)
	}
	inner()
	if n := tr.Pending(true); n != 1 {
		t.Fatalf("every idle mark released, the answer being written: pending %d, want 1", n)
	}
	if got := tr.PendingProgress(true); len(got) != 1 {
		t.Fatalf("answering: the watchdog watches %v, want the connection", got)
	}
	if n := tr.Pending(false); n != 0 {
		t.Fatalf("a record: pending %d, want 0", n)
	}
	close(again)
	waitSettled(t, tr, func() bool { return proxy.reading.Load() == 1 })
	if n := tr.Pending(true); n != 0 {
		t.Fatalf("back reading: pending %d, want 0", n)
	}
	if n := proxy.idle.Load(); n != 0 {
		t.Fatalf("idle marks left: %d", n)
	}
}

// The mark covers the handler, not the connection: bytes the application sent
// meanwhile are its next call in hand, and a request the parser holds
// (HoldRequest) is one it has not answered, so either keeps the connection
// pending however it is marked. A parser that holds the request it waits on
// releases that hold for the wait.
func TestAnIdleMarkCoversNeitherUnreadBytesNorAHeldRequest(t *testing.T) {
	tr := &inflight{}
	app, proxy := acceptedPair(t, tr)
	ctx := integrations.WithRequestHolder(context.Background(), proxy)
	idle := integrations.MarkIdle(ctx)
	defer idle()
	if n := tr.Pending(true); n != 0 {
		t.Fatalf("idle, nothing read: pending %d, want 0", n)
	}

	hold := integrations.HoldRequest(ctx)
	if n := tr.Pending(true); n != 1 {
		t.Fatalf("idle with a request held: pending %d, want 1", n)
	}
	hold()
	if n := tr.Pending(true); n != 0 {
		t.Fatalf("idle, the hold released: pending %d, want 0", n)
	}

	if _, err := io.WriteString(app, "the next call"); err != nil {
		t.Fatal(err)
	}
	waitUnread(t, proxy)
	if n := tr.Pending(true); n != 1 {
		t.Fatalf("idle with unread bytes: pending %d in a replay, want 1", n)
	}
	if n := tr.Pending(false); n != 1 {
		t.Fatalf("idle with unread bytes: pending %d in a record, want 1", n)
	}
	if pending, _ := tr.State(true); pending != 1 {
		t.Fatalf("idle with unread bytes: state pending %d, want 1", pending)
	}
}

// Without a holder (the switch off, or a connection the proxy does not keep)
// MarkIdle marks nothing, and a holder that cannot mark a connection idle (a
// stand-in for one) drops the mark: release is safe to call either way.
func TestMarkIdleWithoutAMarkerMarksNothing(t *testing.T) {
	integrations.MarkIdle(context.Background())()
	tr := &inflight{}
	_, proxy := acceptedPair(t, tr)
	ctx := integrations.WithRequestHolder(context.Background(), holdOnly{proxy})
	release := integrations.MarkIdle(ctx)
	if n := proxy.idle.Load(); n != 0 {
		t.Fatalf("a holder that is no IdleMarker: %d idle marks, want 0", n)
	}
	release()
}

// holdOnly is a RequestHolder and nothing more.
type holdOnly struct{ c *trackedConn }

func (h holdOnly) Hold() (release func()) { return h.c.Hold() }

// An idle mark never covers a close: a connection whose close has begun holds
// work until its handler is done (its miss may still be filed), whatever wait
// its parser marked, and is settled once the handler is done with it.
func TestAnIdleMarkDoesNotCoverAClose(t *testing.T) {
	tr := &inflight{}
	_, proxy := acceptedPair(t, tr)
	release := integrations.MarkIdle(integrations.WithRequestHolder(context.Background(), proxy))
	defer release()
	if n := tr.Pending(true); n != 0 {
		t.Fatalf("idle, before the close: pending %d, want 0", n)
	}
	if err := proxy.Close(); err != nil {
		t.Fatal(err)
	}
	if n := tr.Pending(true); n != 1 {
		t.Fatalf("idle, its close begun: pending %d, want 1", n)
	}
	if pending, _ := tr.State(true); pending != 1 {
		t.Fatalf("idle, its close begun: state pending %d, want 1", pending)
	}
	if got := tr.PendingProgress(true); len(got) != 1 {
		t.Fatalf("idle, its close begun: the watchdog watches %v, want the connection", got)
	}
	tr.forget(proxy)
	if n := tr.Pending(true); n != 0 {
		t.Fatalf("its handler done: pending %d, want 0", n)
	}
}
