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

// A client that closes its socket cleanly (a FIN) partway through a response
// tells the relay nothing here: what the server sends after the FIN is the
// client's to take or refuse (client_took.go), and a refused chunk ends the
// parser's stream where the client stopped (Config.ClientStopped), not here.
//
// A client that resets its connection leaves: the relay stops reading
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
