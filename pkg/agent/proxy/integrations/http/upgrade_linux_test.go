package http

import (
	"bufio"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
)

// A 101 (Switching Protocols), recorded through the proxy's relay on real
// sockets, with net/http's client reading it at the app: net/http frames it as
// its head alone, and the connection then carries the protocol it switched to,
// both ways. The 101 is recorded as the client read it, as soon as its head is
// whole, and nothing after it is recorded: not the server's bytes as its body,
// not the client's as a request, though they read as one. A WebSocket still
// open at the recording's stop costs nothing (TestRecordThroughTheRelayKeepsA101OpenAtTheStop).
func TestRecordThroughTheRelayHandsAnUpgradeOver(t *testing.T) {
	const req = "GET /ws HTTP/1.1\r\nHost: up\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n\r\n"
	for _, row := range []struct {
		name string
		head string // the 101's head
		with string // the switched-to protocol's first bytes, in the head's write
	}{
		{name: "a WebSocket's 101 in a write of its own",
			head: "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n\r\n"},
		{name: "a 101 whose write carries the first frame",
			head: "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n\r\n", with: "\x81\x05first"},
		{name: "a 101 with a Content-Length, which frames none of the bytes after it",
			head: "HTTP/1.1 101 Switching Protocols\r\nContent-Length: 5\r\nUpgrade: echo\r\nConnection: Upgrade\r\n\r\n"},
		{name: "a chunked 101, whose chunks are none of its own",
			head: "HTTP/1.1 101 Switching Protocols\r\nTransfer-Encoding: chunked\r\nUpgrade: echo\r\nConnection: Upgrade\r\n\r\n"},
		{name: "a 101 whose lines end at a bare LF",
			head: "HTTP/1.1 101 Switching Protocols\nUpgrade: websocket\nConnection: Upgrade\n\n", with: "\x81\x05first"},
	} {
		t.Run(row.name, func(t *testing.T) {
			c := newRelayedConn(t)
			c.start(t)
			app := bufio.NewReader(c.app)
			write(t, c.app, c.src, req)
			relayed(t, c.dest, req)
			write(t, c.dest, c.dst, row.head+row.with)
			parsed, err := http.ReadRequest(bufio.NewReader(strings.NewReader(req)))
			if err != nil {
				t.Fatal(err)
			}
			resp, err := http.ReadResponse(app, parsed)
			if err != nil || resp.StatusCode != http.StatusSwitchingProtocols {
				t.Fatalf("net/http's client read %v, %v; want the 101", resp, err)
			}
			// The 101 is recorded at its head: nothing after it is its own.
			var mock *models.Mock
			select {
			case mock = <-c.mocks:
			case <-time.After(5 * time.Second):
				t.Fatal("the 101 was not recorded within 5s of its head: keploy waits on bytes after it")
			}
			if !strings.HasSuffix(mock.Spec.HTTPReq.URL, "/ws") || mock.Spec.HTTPResp.StatusCode != http.StatusSwitchingProtocols || mock.Spec.HTTPResp.Body != "" {
				t.Fatalf("recorded %s -> %d %q, want GET /ws -> 101 with no body",
					mock.Spec.HTTPReq.URL, mock.Spec.HTTPResp.StatusCode, mock.Spec.HTTPResp.Body)
			}
			// The switched-to protocol, both ways, relayed whole. The client's
			// bytes read as an HTTP request, and the server's as its answer:
			// neither is one.
			if row.with != "" {
				buf := make([]byte, len(row.with))
				if _, err := io.ReadFull(app, buf); err != nil || string(buf) != row.with {
					t.Fatalf("the app read %q, %v after the 101; want %q", buf, err, row.with)
				}
			}
			fromApp, fromDest := len(req), len(row.head+row.with)
			for _, b := range []struct{ fromApp, fromDest string }{
				{"\x81\x05hello", "\x81\x05world"},
				{"GET /next HTTP/1.1\r\nHost: up\r\n\r\n", "HTTP/1.1 200 OK\r\nContent-Length: 2\r\n\r\nno"},
			} {
				fromApp, fromDest = fromApp+len(b.fromApp), fromDest+len(b.fromDest)
				write(t, c.app, c.src, b.fromApp)
				relayed(t, c.dest, b.fromApp)
				write(t, c.dest, c.dst, b.fromDest)
				buf := make([]byte, len(b.fromDest))
				if _, err := io.ReadFull(app, buf); err != nil || string(buf) != b.fromDest {
					t.Fatalf("the app read %q, %v; want %q", buf, err, b.fromDest)
				}
			}
			// The recording's stop closes the app with the WebSocket open.
			c.stopBegun.Store(true)
			appClosed(t, c)
			mocks, err := c.finish(t)
			if err != nil {
				t.Fatalf("recordV2 = %v, want nil", err)
			}
			for _, m := range mocks {
				t.Errorf("recorded %s %s -> %d %q after the 101; want nothing more",
					m.Spec.HTTPReq.Method, m.Spec.HTTPReq.URL, m.Spec.HTTPResp.StatusCode, m.Spec.HTTPResp.Body)
			}
			if n := c.mgr.MocksLeftOut(); n != 0 || len(c.spans.all()) != 0 {
				t.Fatalf("%d mocks left out, spans %v; want none", n, c.spans.all())
			}
			// Every byte both ways was read: none of the client's is a request
			// left unread (which leaves out the test cases over it), and the
			// capture holds none of the server's for no one.
			if got := c.sess.ClientStream.ReadThrough(); got != int64(fromApp) {
				t.Errorf("the client's stream was read through %d of its %d bytes", got, fromApp)
			}
			if got := c.sess.DestStream.ReadThrough(); got != int64(fromDest) {
				t.Errorf("the server's stream was read through %d of its %d bytes", got, fromDest)
			}
		})
	}
}

// A WebSocket still open at the recording's stop, or at the client's own
// close, through the relay on real sockets: its 101 is recorded once, with no
// body, and nothing is left out. Nothing here waits for the 101 to be recorded
// at its head, so this row reads what the close leaves: keploy read the
// server's frames as the 101's body to the connection's close, and recorded
// the 101 only then; at the recording's stop, partway through that body, the
// exchange was left out. The client's own close is a guard: keploy recorded
// the 101 at it before, and records it once now.
func TestRecordThroughTheRelayKeepsA101OpenAtTheStop(t *testing.T) {
	const (
		req  = "GET /ws HTTP/1.1\r\nHost: up\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n\r\n"
		head = "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n\r\n"
	)
	for _, row := range []struct {
		name string
		stop bool // the recording's stop closes the app; else the app closes
	}{
		{name: "at the recording's stop", stop: true},
		{name: "at the client's own close"},
	} {
		t.Run(row.name, func(t *testing.T) {
			c := newRelayedConn(t)
			c.start(t)
			app := bufio.NewReader(c.app)
			write(t, c.app, c.src, req)
			relayed(t, c.dest, req)
			write(t, c.dest, c.dst, head)
			parsed, err := http.ReadRequest(bufio.NewReader(strings.NewReader(req)))
			if err != nil {
				t.Fatal(err)
			}
			if resp, err := http.ReadResponse(app, parsed); err != nil || resp.StatusCode != http.StatusSwitchingProtocols {
				t.Fatalf("net/http's client read %v, %v; want the 101", resp, err)
			}
			for _, b := range []struct{ fromApp, fromDest string }{
				{"\x81\x05hello", "\x81\x05world"},
				{"\x81\x03bye", "\x81\x02ok"},
			} {
				write(t, c.app, c.src, b.fromApp)
				relayed(t, c.dest, b.fromApp)
				write(t, c.dest, c.dst, b.fromDest)
				buf := make([]byte, len(b.fromDest))
				if _, err := io.ReadFull(app, buf); err != nil || string(buf) != b.fromDest {
					t.Fatalf("the app read %q, %v; want %q", buf, err, b.fromDest)
				}
			}
			c.stopBegun.Store(row.stop)
			appClosed(t, c)
			mocks, err := c.finish(t)
			if err != nil {
				t.Fatalf("recordV2 = %v, want nil", err)
			}
			if len(mocks) != 1 || mocks[0].Spec.HTTPResp.StatusCode != http.StatusSwitchingProtocols || mocks[0].Spec.HTTPResp.Body != "" {
				for _, m := range mocks {
					t.Logf("recorded %s %s -> %d %q", m.Spec.HTTPReq.Method, m.Spec.HTTPReq.URL, m.Spec.HTTPResp.StatusCode, m.Spec.HTTPResp.Body)
				}
				t.Errorf("recorded %d mocks; want the 101 once, with no body", len(mocks))
			}
			if n := c.mgr.MocksLeftOut(); n != 0 {
				t.Errorf("%d mocks left out; want none", n)
			}
		})
	}
}
