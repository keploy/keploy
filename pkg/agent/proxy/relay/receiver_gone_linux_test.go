package relay

import (
	"fmt"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/fakeconn"
)

// receiverEvents records, in order, what the relay told of a receiver that
// went away (OnReceiverGone) and the mocks it marked incomplete.
type receiverEvents struct {
	mu     sync.Mutex
	events []string
}

func (e *receiverEvents) config() Config {
	return Config{
		OnReceiverGone: func(dir fakeconn.Direction, undelivered int) {
			e.add(fmt.Sprintf("gone %s %d", dir, undelivered))
		},
		OnMarkMockIncomplete: func(reason string) { e.add("mark " + reason) },
	}
}

func (e *receiverEvents) add(s string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.events = append(e.events, s)
}

func (e *receiverEvents) snapshot() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]string(nil), e.events...)
}

// waitFor polls until ok holds of the events, or fails the test.
func (e *receiverEvents) waitFor(t *testing.T, what string, ok func([]string) bool) []string {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if ev := e.snapshot(); ok(ev) {
			return ev
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("%s: events %v", what, e.snapshot())
	return nil
}

// A browser that navigates away closes its connection in the middle of a
// streamed response. The server goes on streaming, and keploy's write of its
// next bytes to the client fails. The server's stream ends there, and the
// relay says so first (OnReceiverGone, for the server's direction, with the
// bytes of that write the client never took), and then marks the write error.
func TestReceiverGone_AClientThatClosedMidStream(t *testing.T) {
	var ev receiverEvents
	h := newHalfCloseHarness(t, ev.config())

	if _, err := h.clientApp.Write([]byte("GET /progress HTTP/1.1\r\n\r\n")); err != nil {
		t.Fatal(err)
	}
	got := make([]byte, 5)
	if _, err := io.ReadFull(h.destSvc, got); err != nil {
		t.Fatalf("the server never got the request: %v", err)
	}
	head := []byte("HTTP/1.1 200 OK\r\nTransfer-Encoding: chunked\r\n\r\n")
	if _, err := h.destSvc.Write(head); err != nil {
		t.Fatal(err)
	}
	_ = h.clientApp.SetReadDeadline(time.Now().Add(3 * time.Second))
	if _, err := io.ReadFull(h.clientApp, make([]byte, len(head))); err != nil {
		t.Fatalf("the client never got the response's head: %v", err)
	}
	// The browser navigates away, having read all it was sent: a close,
	// not a reset.
	_ = h.clientApp.Close()

	// The server streams on; once the client's side refuses, keploy's
	// write fails.
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			case <-time.After(20 * time.Millisecond):
			}
			line := fmt.Sprintf("{\"progress\":%d}\n", i)
			if _, err := fmt.Fprintf(h.destSvc, "%x\r\n%s\r\n", len(line), line); err != nil {
				return
			}
		}
	}()

	events := ev.waitFor(t, "the relay never said the client left", func(ev []string) bool {
		for _, e := range ev {
			if e == "mark "+MarkWriteError {
				return true
			}
		}
		return false
	})
	var gone, undelivered int
	if _, err := fmt.Sscanf(events[0], "gone dest %d", &undelivered); err != nil || undelivered <= 0 {
		t.Fatalf("events %v: want the client's leaving first, for the server's stream, with the failed write's bytes", events)
	}
	for _, e := range events {
		if len(e) > 4 && e[:4] == "gone" {
			gone++
		}
	}
	if gone != 1 {
		t.Fatalf("events %v: the client's leaving told %d times, want once", events, gone)
	}
}

// A client that resets its connection leaves too: the relay stops reading
// the server, and its stream ends there, with nothing written that failed.
func TestReceiverGone_AClientThatResetTheConnection(t *testing.T) {
	var ev receiverEvents
	h := newHalfCloseHarness(t, ev.config())

	if _, err := h.clientApp.Write([]byte("GET /progress HTTP/1.1\r\n\r\n")); err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadFull(h.destSvc, make([]byte, 5)); err != nil {
		t.Fatalf("the server never got the request: %v", err)
	}
	if err := h.clientApp.(*net.TCPConn).SetLinger(0); err != nil {
		t.Fatal(err)
	}
	_ = h.clientApp.Close() // RST

	ev.waitFor(t, "the relay never said the client left", func(ev []string) bool {
		return len(ev) > 0 && ev[0] == "gone dest 0"
	})
}

// A client that half-closes (shutdown(SHUT_WR)) has not left: it reads its
// answer, and nothing says it went away.
func TestReceiverGone_NotForAClientThatHalfCloses(t *testing.T) {
	var ev receiverEvents
	h := newHalfCloseHarness(t, ev.config())

	if _, err := h.clientApp.Write([]byte("ping\n")); err != nil {
		t.Fatal(err)
	}
	if err := h.clientApp.(*net.TCPConn).CloseWrite(); err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadAll(h.destSvc); err != nil {
		t.Fatalf("the server never saw the request end: %v", err)
	}
	if _, err := h.destSvc.Write([]byte("pong\n")); err != nil {
		t.Fatal(err)
	}
	_ = h.clientApp.SetReadDeadline(time.Now().Add(3 * time.Second))
	if _, err := io.ReadFull(h.clientApp, make([]byte, 5)); err != nil {
		t.Fatalf("the client never got its answer: %v", err)
	}
	_ = h.destSvc.Close()
	select {
	case <-h.done:
	case <-time.After(3 * time.Second):
		t.Fatal("the relay did not end after both sides closed")
	}
	for _, e := range ev.snapshot() {
		if len(e) > 4 && e[:4] == "gone" {
			t.Fatalf("events %v: a half-closing client was said to have left", ev.snapshot())
		}
	}
}
