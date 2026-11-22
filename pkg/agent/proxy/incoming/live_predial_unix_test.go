//go:build linux || darwin

package proxy

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"testing"
	"time"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/agent/proxy/connseq"
	"go.keploy.io/server/v3/pkg/agent/proxy/util"
	"go.uber.org/zap"
)

// The live handler serves a call over the connection keploy's handshake hold
// dialed for it (util.WithPredialed), the one the app's listener accepted,
// with requests recorded and without: never over a second dial. A listener
// that takes one connection and closes has none to give a second dial: the
// call waited for it for ever.
func TestServeLiveServesACallOverTheHoldsDial(t *testing.T) {
	for _, recorded := range []bool{false, true} {
		t.Run(fmt.Sprintf("recorded=%v", recorded), func(t *testing.T) {
			l, err := net.Listen("tcp4", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = l.Close() }()
			up, err := net.Dial("tcp4", l.Addr().String())
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = up.Close() }()
			app, err := l.Accept()
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = app.Close() }()
			_ = l.Close() // it took the hold's dial, and listens no more
			pre := util.NewPredialed(l.Addr().String(), connseq.NewUpstream(up))

			cfg := &config.Config{}
			cfg.Agent.MockMode = recorded
			cfg.Agent.RecordRequests = recorded
			pm := New(zap.NewNop(), nil, cfg)
			// Recorded, the call's capture runs after the handler ends, and reads
			// the package's capture hook: it ends before the next row replaces it.
			t.Cleanup(pm.captures.Wait)
			client, proxied := net.Pipe()
			defer func() { _ = client.Close() }()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan struct{})
			go func() {
				defer close(done)
				pm.ServeLive(util.WithPredialed(ctx, pre), proxied, l.Addr().String(), 8080)
			}()

			const req = "GET /ping HTTP/1.1\r\nHost: app\r\n\r\n"
			const resp = "HTTP/1.1 200 OK\r\nContent-Length: 4\r\n\r\npong"
			go func() { _, _ = client.Write([]byte(req)) }()
			_ = app.SetDeadline(time.Now().Add(10 * time.Second))
			// Recorded, the request is written to the app as keploy read it.
			r, err := http.ReadRequest(bufio.NewReader(app))
			if err != nil || r.Method != http.MethodGet || r.URL.Path != "/ping" {
				t.Fatalf("the app's end of the hold's dial read %v, %v; want the call's GET /ping", r, err)
			}
			go func() { _, _ = app.Write([]byte(resp)) }()
			_ = client.SetDeadline(time.Now().Add(10 * time.Second))
			got := make([]byte, len(resp))
			if _, err := io.ReadFull(client, got); err != nil || string(got) != resp {
				t.Fatalf("the caller read %q, %v; want the app's answer", got, err)
			}
			_ = client.Close()
			_ = app.Close()
			cancel()
			<-done
		})
	}
}

// The live handler's dial of the app takes the hold's dial only at the
// address the hold dialed (util.Predialed.Take): a call to another address is
// dialed there, and the hold's dial waits for a dial of its own address.
// Taken at any address, the call reached whatever listener the hold had
// dialed, not the app at the address it was given.
func TestDialAppTakesTheHoldsDialOnlyAtItsAddress(t *testing.T) {
	held, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = held.Close() }()
	up, err := net.Dial("tcp4", held.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = up.Close() }()
	heldEnd, err := held.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = heldEnd.Close() }()
	other, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = other.Close() }()
	pre := util.NewPredialed(held.Addr().String(), connseq.NewUpstream(up))
	ctx := util.WithPredialed(context.Background(), pre)
	pm := New(zap.NewNop(), nil, &config.Config{})
	client, proxied := net.Pipe()
	defer func() { _ = client.Close() }()

	conn, err := pm.dialApp(ctx, other.Addr().String(), proxied)
	if err != nil {
		t.Fatalf("dialApp(%s) = %v, want a connection there", other.Addr(), err)
	}
	defer func() { _ = conn.Close() }()
	if pre.Is(conn) || conn.RemoteAddr().String() != other.Addr().String() {
		t.Fatalf("dialApp(%s) gave a connection to %v; want its own dial of that address, not the hold's dial of %s", other.Addr(), conn.RemoteAddr(), held.Addr())
	}
	got, err := pm.dialApp(ctx, held.Addr().String(), proxied)
	if err != nil || !pre.Is(got) {
		t.Fatalf("dialApp(%s) = %v, %v; want the hold's dial of that address, still there for it", held.Addr(), got, err)
	}
}
