package pkg

import (
	"bufio"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// upgradeApp answers every request with a 101 (Switching Protocols) and then
// speaks the protocol it switched to on the connection, as an h2c tunnel's
// server does: it sends HTTP/2's client preface. With hold it then keeps the
// connection until its client closes it, and says so on closed; without, it
// gives up on its peer and closes it, as api-server's tunnel does when no
// HTTP/2 answer comes.
func upgradeApp(t *testing.T, hold bool) (url string, closed <-chan struct{}) {
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
		if hold {
			_, _ = io.Copy(io.Discard, bufio.NewReader(conn))
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
// another kind than the recorded one was compared (keploy v3.6.130).
func TestASwitchingProtocolsAnswerEndsAtItsHeaders(t *testing.T) {
	url, _ := upgradeApp(t, false)
	resp, err := SimulateHTTP(context.Background(), upgradeCase(url), "set", zap.NewNop(), SimulationConfig{APITimeout: 5})
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

// keploy speaks no protocol a test switches to, so it does not wait on it: the
// answer is in once its headers are, and keploy closes the connection, which
// an app that keeps its side open for its peer (a WebSocket, an h2c tunnel)
// sees. Waiting for the app to close, as reading the rest as a body did, held
// each upgrade test until the app gave up on its peer.
func TestASwitchingProtocolsAnswerClosesTheUpgradedConnection(t *testing.T) {
	url, closed := upgradeApp(t, true)
	resp, err := SimulateHTTP(context.Background(), upgradeCase(url), "set", zap.NewNop(), SimulationConfig{APITimeout: 5})
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusSwitchingProtocols || resp.Body != "" {
		t.Fatalf("got %d %q, want a 101 with no body", resp.StatusCode, resp.Body)
	}
	<-closed
}

// The streaming path ends a 101 at its headers too: its reader holds nothing.
func TestASwitchingProtocolsAnswerStreamsNothing(t *testing.T) {
	url, closed := upgradeApp(t, true)
	resp, err := SimulateHTTPStreaming(context.Background(), upgradeCase(url), "set", zap.NewNop(), SimulationConfig{APITimeout: 5})
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(resp.Reader)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusSwitchingProtocols || len(body) != 0 {
		t.Fatalf("got %d %q, want a 101 with nothing to stream", resp.StatusCode, body)
	}
	if err := resp.Reader.Close(); err != nil {
		t.Fatal(err)
	}
	<-closed
}
