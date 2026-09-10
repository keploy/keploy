package recorder

import (
	"bytes"
	"context"
	"net"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
)

// TestFetchServerGreeting_RefusesFabricatedAddr pins the dial guard: when the
// capture layer marked the destination as a stand-in (AddrFabricated — the
// proxyless SSL-uprobe path's loopback substitution + content-matched port),
// fetchServerGreeting must fail WITHOUT dialing. A live listener on the
// address proves no connection is attempted — dialing a fabricated loopback
// address can reach an unrelated co-resident server and stitch a foreign
// greeting into the mock.
func TestFetchServerGreeting_RefusesFabricatedAddr(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	accepted := make(chan struct{}, 1)
	go func() {
		conn, aerr := ln.Accept()
		if aerr != nil {
			return
		}
		accepted <- struct{}{}
		_ = conn.Close()
	}()

	opts := models.OutgoingOptions{
		DstCfg: &models.ConditionalDstCfg{Addr: ln.Addr().String(), Port: 3306, AddrFabricated: true},
	}
	buf, err := fetchServerGreeting(context.Background(), opts)
	if err == nil {
		t.Fatal("fetchServerGreeting must refuse a fabricated destination")
	}
	if !strings.Contains(err.Error(), "refusing to dial") {
		t.Errorf("error = %v, want the explicit dial refusal", err)
	}
	if buf != nil {
		t.Errorf("buf = %v, want nil on refusal", buf)
	}
	select {
	case <-accepted:
		t.Fatal("fetchServerGreeting dialed a fabricated address")
	case <-time.After(100 * time.Millisecond):
	}
}

// TestFetchServerGreeting_ReadsRealGreeting proves the guard does not break
// the legitimate fallback (pre-warmed connections whose handshake predates
// interception): a non-fabricated destination is dialed and the greeting
// packet is returned verbatim.
func TestFetchServerGreeting_ReadsRealGreeting(t *testing.T) {
	greeting := cannedHandshakeV10(t)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	go func() {
		conn, aerr := ln.Accept()
		if aerr != nil {
			return
		}
		_, _ = conn.Write(greeting)
		_ = conn.Close()
	}()

	opts := models.OutgoingOptions{
		DstCfg: &models.ConditionalDstCfg{Addr: ln.Addr().String(), Port: 3306},
	}
	buf, err := fetchServerGreeting(context.Background(), opts)
	if err != nil {
		t.Fatalf("fetchServerGreeting: %v", err)
	}
	if !bytes.Equal(buf, greeting) {
		t.Fatalf("greeting bytes mismatch: got %d bytes, want %d", len(buf), len(greeting))
	}
}
