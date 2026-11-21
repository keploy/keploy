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
