//go:build linux

package proxy

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"syscall"
	"testing"
	"time"

	"go.uber.org/zap"

	"go.keploy.io/server/v3/pkg/agent/proxy/connseq"
	ossHttp "go.keploy.io/server/v3/pkg/agent/proxy/integrations/http"
	"go.keploy.io/server/v3/pkg/agent/proxy/relay"
	"go.keploy.io/server/v3/pkg/agent/proxy/util"
	"go.keploy.io/server/v3/pkg/models"
)

// A reset keploy gives the application, on a connection the proxy pairs (under
// the proven switch, or in a learning run that settles its reads:
// keepsConnections), as the accept loop wires it (heldPair: the trackedConn
// whose beforeClose is pairClosing). The pairing reads what the socket holds
// unread before it closes, so the reset must close the socket through that
// conn: a close of the raw socket under it leaves the handler's later close
// reading a closed socket, which fails the pairing for the rest of the run
// and charges a capture loss for a request read whole. The application still
// reads the reset (or, for an eof, the clean end) the upstream gave.

// pairKeptWhole fails t when the pairing failed or a capture loss was
// charged, and when the application did not read want as its connection's end.
func pairKeptWhole(t *testing.T, p *Proxy, app net.Conn, kind models.FaultKind, want error) {
	t.Helper()
	_ = app.SetReadDeadline(time.Now().Add(10 * time.Second))
	n, rerr := app.Read(make([]byte, 64))
	if n != 0 || !errors.Is(rerr, want) {
		t.Errorf("after a %s fault the application read %d bytes, %v; want nothing, then %v", kind, n, rerr, want)
	}
	if err := p.paired.failure(); err != nil {
		t.Errorf("a %s fault on a paired connection failed the pairing for the rest of the run: %v", kind, err)
	}
	if l := lossesOf(p, models.CaptureLossAppBytesLost); l != 0 {
		t.Errorf("a %s fault on a paired connection charged %d app_bytes_lost, after its request was read whole", kind, l)
	}
}

// Replay: a recorded fault served as decodeHTTP serves it (EnactFault), then
// the handler's close.
func TestAFaultServedOnAPairedConnectionKeepsThePairingWhole(t *testing.T) {
	for _, tc := range []struct {
		kind models.FaultKind
		want error
	}{
		{models.FaultReset, syscall.ECONNRESET},
		{models.FaultEOF, io.EOF},
	} {
		t.Run(string(tc.kind), func(t *testing.T) {
			p, app, _, reader := heldPair(t, "tcp4", "127.0.0.1:0")
			req := []byte("GET /call/x HTTP/1.1\r\nHost: dep\r\n\r\n")
			if _, err := app.Write(req); err != nil {
				t.Fatal(err)
			}
			if _, err := io.ReadFull(reader, make([]byte, len(req))); err != nil {
				t.Fatal(err)
			}
			h := &ossHttp.HTTP{Logger: zap.NewNop()}
			stub := &models.Mock{Name: "mock-0", Kind: models.HTTP, Spec: models.MockSpec{Fault: &models.Fault{Kind: tc.kind}}}
			over, err := h.EnactFault(context.Background(), reader, stub)
			if !over || err != nil {
				t.Fatalf("EnactFault = %v, %v; want the call over, served", over, err)
			}
			_ = reader.Close() // the handler's close
			pairKeptWhole(t, p, app, tc.kind, tc.want)
		})
	}
}

// Replay over HTTPS: the proxy serves the application through its TLS layer
// over the paired conn (HoldTLSLayer, as HandleTLSConnection wires it), with
// a wrapper of keploy's above the layer and one under it, as the proxy's
// chain has (util.Conn). The reset closes the socket under the layer, with no
// close_notify first, so the HTTPS client reads the reset, not the clean end
// of the stream; and through the paired conn, so the pairing reads what the
// socket holds as it closes.
func TestAResetServedOverTLSOnAPairedConnectionKeepsThePairingWhole(t *testing.T) {
	p, app, _, reader := heldPair(t, "tcp4", "127.0.0.1:0")
	under := &util.Conn{Conn: reader, Reader: reader, Logger: zap.NewNop()}
	layer := tls.Server(under, &tls.Config{Certificates: []tls.Certificate{selfSignedForDispatch(t)}})
	reader.(*trackedConn).HoldTLSLayer(layer)
	srv := &util.Conn{Conn: layer, Reader: layer, Logger: zap.NewNop()}
	cli := tls.Client(app, &tls.Config{InsecureSkipVerify: true}) // #nosec G402 -- a test's self-signed peer
	hs := make(chan error, 1)
	go func() { hs <- layer.Handshake() }()
	if err := cli.Handshake(); err != nil {
		t.Fatal(err)
	}
	if err := <-hs; err != nil {
		t.Fatal(err)
	}
	req := []byte("GET /call/x HTTP/1.1\r\nHost: dep\r\n\r\n")
	if _, err := cli.Write(req); err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadFull(srv, make([]byte, len(req))); err != nil {
		t.Fatal(err)
	}
	h := &ossHttp.HTTP{Logger: zap.NewNop()}
	stub := &models.Mock{Name: "mock-0", Kind: models.HTTP, Spec: models.MockSpec{Fault: &models.Fault{Kind: models.FaultReset}}}
	if over, err := h.EnactFault(context.Background(), srv, stub); !over || err != nil {
		t.Fatalf("EnactFault = %v, %v; want the call over, served", over, err)
	}
	_ = srv.Close() // the handler's close
	pairKeptWhole(t, p, cli, models.FaultReset, syscall.ECONNRESET)
}

// Record: the relay gives the application the reset its upstream gave
// (relay.go, destReset), then the handler's close. (An upstream's eof is
// relayed as a half-close, with no reset: the serve row's eof covers that the
// pairing reads a plain close as before.)
func TestAnUpstreamResetRelayedOnAPairedConnectionKeepsThePairingWhole(t *testing.T) {
	p, app, _, reader := heldPair(t, "tcp4", "127.0.0.1:0")
	dst, upstream := loopbackPair(t)
	r := relay.New(relay.Config{Logger: zap.NewNop(), HalfCloseGrace: relay.DefaultHalfCloseGrace}, reader, connseq.NewUpstream(dst))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	for _, s := range []net.Conn{r.ClientStream(), r.DestStream()} {
		go func() { _, _ = io.Copy(io.Discard, s) }()
	}
	done := make(chan error, 1)
	go func() { done <- r.Run(ctx) }()

	req := "GET /call/x HTTP/1.1\r\nHost: dep\r\n\r\n"
	if _, err := app.Write([]byte(req)); err != nil {
		t.Fatal(err)
	}
	_ = upstream.SetReadDeadline(time.Now().Add(10 * time.Second))
	if _, err := io.ReadFull(upstream, make([]byte, len(req))); err != nil {
		t.Fatalf("the upstream did not get the request: %v", err)
	}
	if err := upstream.(*net.TCPConn).SetLinger(0); err != nil {
		t.Fatal(err)
	}
	_ = upstream.Close()
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("the relay did not end within 15s of the upstream's reset")
	}
	_ = reader.Close() // the handler's close
	pairKeptWhole(t, p, app, models.FaultReset, syscall.ECONNRESET)
}
