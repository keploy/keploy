package tls

import (
	"context"
	"crypto/tls"
	"net"
	"testing"
	"time"

	"go.uber.org/zap"
)

// mitmNegotiate runs the REAL keploy MITM (HandleTLSConnection) against a TLS
// client offering the given ALPN, and returns the protocol both ends
// negotiated. ctx controls the replay "prefer h2" hint (WithPreferH2).
func mitmNegotiate(t *testing.T, hsCtx context.Context, clientALPN []string) (clientProto, serverProto string) {
	t.Helper()
	return mitmNegotiateSNI(t, hsCtx, clientALPN, "")
}

// mitmNegotiateSNI is mitmNegotiate for a client whose hello carries
// serverName as its SNI ("" sends none).
func mitmNegotiateSNI(t *testing.T, hsCtx context.Context, clientALPN []string, serverName string) (clientProto, serverProto string) {
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
		NextProtos:         clientALPN,
		ServerName:         serverName,
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
// with WithPreferH2 set, a dual-protocol client ([h2, http/1.1]) stays on h2
// through the MITM, so its request matches the recorded Http2 mock.
func TestMITM_DualProtocol_PreferH2KeepsH2(t *testing.T) {
	cp, sp := mitmNegotiate(t, WithPreferH2(context.Background()), []string{"h2", "http/1.1"})
	if cp != "h2" || sp != "h2" {
		t.Fatalf("with PreferH2, dual-protocol client should keep h2; got client=%q server=%q", cp, sp)
	}
	t.Logf("PreferH2 + dual-protocol client → negotiated h2 (no downgrade)")
}

// TestMITM_DualProtocol_DefaultDowngrades is the record / no-Http2-mocks case:
// WITHOUT WithPreferH2, the MITM downgrades a dual-protocol client to http/1.1
// (the historical behaviour) so http/1.1 recordings still replay as http/1.1.
func TestMITM_DualProtocol_DefaultDowngrades(t *testing.T) {
	cp, sp := mitmNegotiate(t, context.Background(), []string{"h2", "http/1.1"})
	if cp != "http/1.1" || sp != "http/1.1" {
		t.Fatalf("without PreferH2, dual-protocol client should downgrade to http/1.1; got client=%q server=%q", cp, sp)
	}
	t.Logf("no PreferH2 + dual-protocol client → downgraded to http/1.1 (unchanged behaviour)")
}

// TestMITM_HTTP1Only_StaysHTTP1 guards the non-regression: an http/1.1-only
// client stays http/1.1 regardless of the PreferH2 hint.
func TestMITM_HTTP1Only_StaysHTTP1(t *testing.T) {
	cp, sp := mitmNegotiate(t, WithPreferH2(context.Background()), []string{"http/1.1"})
	if cp != "http/1.1" || sp != "http/1.1" {
		t.Fatalf("http/1.1-only client should stay http/1.1; got client=%q server=%q", cp, sp)
	}
}

// TestMITM_PreferH2For_DecidesBySNI is keploy #4713: the replay's h2 decision
// is asked with the SNI of the client's hello, so a dual-protocol client to an
// http/1.1 destination stays on http/1.1 even when another host has Http2
// mocks, and one to the HTTP/2 destination stays on h2.
func TestMITM_PreferH2For_DecidesBySNI(t *testing.T) {
	var asked []string
	ctx := WithPreferH2For(context.Background(), func(sni string) bool {
		asked = append(asked, sni)
		return sni == "h2.example"
	})
	for _, tc := range []struct{ sni, want string }{
		{"keploymocks.blob.core.windows.net", "http/1.1"},
		{"h2.example", "h2"},
	} {
		cp, sp := mitmNegotiateSNI(t, ctx, []string{"h2", "http/1.1"}, tc.sni)
		if cp != tc.want || sp != tc.want {
			t.Fatalf("SNI %q: want %q; got client=%q server=%q", tc.sni, tc.want, cp, sp)
		}
	}
	if len(asked) != 2 || asked[0] != "keploymocks.blob.core.windows.net" || asked[1] != "h2.example" {
		t.Fatalf("the decision was not asked with each hello's SNI: %q", asked)
	}
}
