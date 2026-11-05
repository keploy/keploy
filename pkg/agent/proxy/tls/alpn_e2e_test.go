package tls

import (
	"context"
	"crypto/tls"
	"net"
	"testing"
	"time"

	"go.uber.org/zap"
)

// preferH2ForEvery is the replay "prefer h2" decision for every server name.
func preferH2ForEvery(ctx context.Context) context.Context {
	return WithPreferH2For(ctx, func(string) bool { return true })
}

// mitmNegotiate runs the REAL keploy MITM (HandleTLSConnection) against a TLS
// client offering the given ALPN, and returns the protocol both ends
// negotiated. ctx controls the replay "prefer h2" decision (WithPreferH2For).
func mitmNegotiate(t *testing.T, hsCtx context.Context, clientALPN []string) (clientProto, serverProto string) {
	t.Helper()
	return mitmNegotiateNamed(t, hsCtx, "", clientALPN)
}

// mitmNegotiateNamed is mitmNegotiate for a client whose ClientHello names
// serverName (SNI; "" sends none).
func mitmNegotiateNamed(t *testing.T, hsCtx context.Context, serverName string, clientALPN []string) (clientProto, serverProto string) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()

	type srvRes struct {
		proto string
		err   error
	}
	srvCh := make(chan srvRes, 1)
	go func() {
		c, aerr := ln.Accept()
		if aerr != nil {
			srvCh <- srvRes{err: aerr}
			return
		}
		defer c.Close() // closes the underlying conn on both paths (tconn wraps c)
		tconn, _, herr := HandleTLSConnection(hsCtx, zap.NewNop(), c, time.Now())
		if herr != nil {
			srvCh <- srvRes{err: herr}
			return
		}
		srvCh <- srvRes{proto: tconn.(*tls.Conn).ConnectionState().NegotiatedProtocol}
	}()

	client, err := tls.Dial("tcp", ln.Addr().String(), &tls.Config{
		InsecureSkipVerify: true, //nolint:gosec // test MITM cert
		ServerName:         serverName,
		NextProtos:         clientALPN,
	})
	if err != nil {
		t.Fatalf("client dial/handshake: %v", err)
	}
	defer client.Close()

	r := <-srvCh
	if r.err != nil {
		t.Fatalf("MITM server handshake: %v", r.err)
	}
	return client.ConnectionState().NegotiatedProtocol, r.proto
}

// TestMITM_DualProtocol_PreferH2KeepsH2 is the replay-with-Http2-mocks case:
// with a decision that prefers h2, a dual-protocol client ([h2, http/1.1])
// stays on h2 through the MITM, so its request matches the recorded Http2 mock.
func TestMITM_DualProtocol_PreferH2KeepsH2(t *testing.T) {
	cp, sp := mitmNegotiate(t, preferH2ForEvery(context.Background()), []string{"h2", "http/1.1"})
	if cp != "h2" || sp != "h2" {
		t.Fatalf("with PreferH2, dual-protocol client should keep h2; got client=%q server=%q", cp, sp)
	}
	t.Logf("PreferH2 + dual-protocol client → negotiated h2 (no downgrade)")
}

// TestMITM_DualProtocol_DefaultDowngrades is the record / no-Http2-mocks case:
// WITHOUT a WithPreferH2For decision, the MITM downgrades a dual-protocol client to http/1.1
// (the historical behaviour) so http/1.1 recordings still replay as http/1.1.
func TestMITM_DualProtocol_DefaultDowngrades(t *testing.T) {
	cp, sp := mitmNegotiate(t, context.Background(), []string{"h2", "http/1.1"})
	if cp != "http/1.1" || sp != "http/1.1" {
		t.Fatalf("without PreferH2, dual-protocol client should downgrade to http/1.1; got client=%q server=%q", cp, sp)
	}
	t.Logf("no PreferH2 + dual-protocol client → downgraded to http/1.1 (unchanged behaviour)")
}

// TestMITM_HTTP1Only_StaysHTTP1 guards the non-regression: an http/1.1-only
// client stays http/1.1 regardless of the PreferH2 hint, and the decision,
// which reads the recorded mocks, is not asked about a client that does not
// offer h2.
func TestMITM_HTTP1Only_StaysHTTP1(t *testing.T) {
	asked := 0
	hsCtx := WithPreferH2For(context.Background(), func(string) bool {
		asked++
		return true
	})
	cp, sp := mitmNegotiate(t, hsCtx, []string{"http/1.1"})
	if cp != "http/1.1" || sp != "http/1.1" {
		t.Fatalf("http/1.1-only client should stay http/1.1; got client=%q server=%q", cp, sp)
	}
	if asked != 0 {
		t.Fatalf("the decision was asked %d times about a client that does not offer h2, want 0", asked)
	}
}

// TestMITM_PreferH2ForDecidesByTheClientHelloServerName: the decision is
// handed the server name the client's ClientHello asks for, and its answer
// is what the MITM offers. Two dual-protocol clients on the same decision:
// the one naming the host recorded over HTTP/2 stays on h2, the one naming
// another host is downgraded to http/1.1, and one naming no host is asked
// about "".
func TestMITM_PreferH2ForDecidesByTheClientHelloServerName(t *testing.T) {
	var asked []string
	hsCtx := WithPreferH2For(context.Background(), func(serverName string) bool {
		asked = append(asked, serverName)
		return serverName == "h2.example.test"
	})
	dual := []string{"h2", "http/1.1"}
	for _, tc := range []struct {
		serverName, want string
	}{
		{"h2.example.test", "h2"},
		{"h1.example.test", "http/1.1"},
		{"", "http/1.1"},
	} {
		asked = nil
		cp, sp := mitmNegotiateNamed(t, hsCtx, tc.serverName, dual)
		if cp != tc.want || sp != tc.want {
			t.Errorf("client naming %q: got client=%q server=%q, want %q", tc.serverName, cp, sp, tc.want)
		}
		if len(asked) != 1 || asked[0] != tc.serverName {
			t.Errorf("client naming %q: the decision was asked about %q, want exactly [%q]", tc.serverName, asked, tc.serverName)
		}
	}
}
