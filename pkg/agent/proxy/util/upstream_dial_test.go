package util

import (
	"context"
	"crypto/tls"
	"testing"

	"go.keploy.io/server/v3/pkg/agent/proxy/connseq"
)

// Every connection to a destination is read through a connseq.Upstream at its
// socket, under any TLS and any wrapper the proxy puts on it, so the relay can
// number what it carries by what the destination had sent (relay.New refuses
// a destination without one).
func TestEveryDialOfADestinationIsReadThroughAnUpstream(t *testing.T) {
	cert := selfSigned(t)
	l, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: []tls.Certificate{cert}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close() })
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go func() { _ = c.(*tls.Conn).Handshake() }()
			t.Cleanup(func() { _ = c.Close() })
		}
	}()
	addr := l.Addr().String()
	ctx := context.Background()

	raw, err := DialRaw(ctx, nil, "tcp", addr, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	if connseq.Of(raw) == nil {
		t.Fatalf("DialRaw returned a %T with no connseq.Upstream", raw)
	}
	tc, err := DialTLS(ctx, nil, "tcp", addr, &tls.Config{InsecureSkipVerify: true}, nil) //nolint:gosec // test server
	if err != nil {
		t.Fatal(err)
	}
	defer tc.Close()
	if connseq.Of(tc) == nil {
		t.Fatal("DialTLS's connection has no connseq.Upstream under its TLS")
	}
	// A probe's replay of what it read, and a CONNECT tunnel's buffer, wrap the
	// connection in a Conn: the Upstream is still found beneath it.
	wrapped := &Conn{Conn: raw, Reader: NewPrefixReader([]byte("greeting"), raw)}
	if connseq.Of(wrapped) != connseq.Of(raw) || connseq.Of(tls.Client(wrapped, &tls.Config{})) != connseq.Of(raw) {
		t.Fatal("the Upstream is not found beneath a Conn")
	}
}
