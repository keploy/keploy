package pkg

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// upgradePayload is what upgradeApp sends after the preface when asked to.
const upgradePayload = 1 << 20

// upgradeApp answers every request with a 101 (Switching Protocols) and then
// speaks the protocol it switched to on the connection, as an h2c tunnel's
// server does: it sends HTTP/2's client preface. With more it then sends
// upgradePayload bytes more, ends its side of the connection (a half-close:
// it has nothing more to send) and keeps reading its peer, as a server that
// waits on it does, until its peer ends the connection, which ended then
// says. Without more it closes the connection at once (api-server's tunnel
// closes it 10 s later, when no HTTP/2 answer has come).
//
// What tells whether keploy read on is the answer's body: everything the app
// sent before its half-close is there for a client that reads on. Whether the
// app's writes completed is no signal: on Windows a 64 MiB loopback write
// completed with no error after its peer had closed unread. And a keploy that
// keeps the connection open is a test that hangs at <-ended.
func upgradeApp(t *testing.T, more bool) (url string, ended <-chan struct{}) {
	t.Helper()
	ch := make(chan struct{}, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, rw, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Errorf("hijack: %v", err)
			return
		}
		defer conn.Close()
		_, _ = rw.WriteString("HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: h2c\r\n\r\n")
		_, _ = rw.WriteString("PRI * HTTP/2.0\r\n\r\nSM\r\n\r\n")
		_ = rw.Flush()
		if more {
			// A write or half-close that fails is a peer that has already
			// gone: either way the app goes on to read until it ends.
			_, _ = conn.Write(make([]byte, upgradePayload))
			if cw, ok := conn.(interface{ CloseWrite() error }); ok {
				_ = cw.CloseWrite()
			}
			_, _ = io.Copy(io.Discard, rw)
			ch <- struct{}{}
		}
	}))
	t.Cleanup(srv.Close)
	return srv.URL, ch
}

func upgradeCase(url string) *models.TestCase {
	return &models.TestCase{Name: "get-tunnel-connect", Kind: models.HTTP, HTTPReq: models.HTTPReq{
		Method: "GET", URL: url + "/tunnel/v1/connect",
		Header: map[string]string{"Connection": "Upgrade", "Upgrade": "h2c"},
	}}
}

// A 101's response ends at its header section; what follows on the
// connection belongs to the protocol it switched to (RFC 9110 §15.2.2). keploy
// test's answer is the 101 with no body, as its recording has: the next
// protocol's bytes (an h2c tunnel's preface, a WebSocket's frames) read as the
// body made every replayed upgrade differ from its recording once a body of
// another kind than the recorded one was compared (keploy v3.6.129).
func TestASwitchingProtocolsAnswerEndsAtItsHeaders(t *testing.T) {
	url, _ := upgradeApp(t, false)
	resp, err := SimulateHTTP(context.Background(), upgradeCase(url), "set", zap.NewNop(), SimulationConfig{})
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("status %d, want 101", resp.StatusCode)
	}
	if resp.Body != "" {
		t.Fatalf("the 101's body is %q: the switched-to protocol's bytes were read as the response", resp.Body)
	}
	if got := resp.Header["Upgrade"]; got != "h2c" {
		t.Fatalf("Upgrade header %q, want h2c", got)
	}
}

// keploy speaks no protocol a test switches to, so it does not read on: the
// answer is in once its headers are, and keploy ends the connection, which an
// app that goes on speaking and waits on its peer (a WebSocket, an h2c tunnel)
// sees end. Reading the rest as a body lasted as long as the app kept the
// connection open, for ever for one that never closes it; this app ends its
// side, so a keploy that reads on returns with all it sent as the body.
func TestASwitchingProtocolsAnswerClosesTheUpgradedConnection(t *testing.T) {
	url, ended := upgradeApp(t, true)
	resp, err := SimulateHTTP(context.Background(), upgradeCase(url), "set", zap.NewNop(), SimulationConfig{})
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusSwitchingProtocols || resp.Body != "" {
		t.Fatalf("got %d with a %d-byte body: keploy read on past its 101's headers, want a 101 with none", resp.StatusCode, len(resp.Body))
	}
	<-ended // the app saw keploy end the connection
}

// The streaming path ends a 101 at its headers too: its reader holds nothing,
// and the connection is ended.
func TestASwitchingProtocolsAnswerStreamsNothing(t *testing.T) {
	url, ended := upgradeApp(t, true)
	resp, err := SimulateHTTPStreaming(context.Background(), upgradeCase(url), "set", zap.NewNop(), SimulationConfig{})
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(resp.Reader)
	if err != nil {
		t.Fatal(err)
	}
	if err := resp.Reader.Close(); err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusSwitchingProtocols || len(body) != 0 {
		t.Fatalf("got %d with %d bytes to stream: keploy read on past its 101's headers, want a 101 with nothing", resp.StatusCode, len(body))
	}
	<-ended // the app saw keploy end the connection
}
