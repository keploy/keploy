package tls

import (
	"bytes"
	"crypto/tls"
	"io"
	"net"
	"testing"
	"time"
)

// clientHelloOf is the ClientHello a crypto/tls client sends for sni ("" for
// none), as the bytes on the wire: one TLS record.
func clientHelloOf(t *testing.T, sni string) []byte {
	t.Helper()
	app, keploy := net.Pipe()
	t.Cleanup(func() { _ = app.Close(); _ = keploy.Close() })
	go func() {
		c := tls.Client(app, &tls.Config{ServerName: sni, InsecureSkipVerify: true}) //nolint:gosec // no server answers
		_ = c.Handshake()
	}()
	header := make([]byte, 5)
	if _, err := io.ReadFull(keploy, header); err != nil {
		t.Fatal(err)
	}
	body := make([]byte, int(header[3])<<8|int(header[4]))
	if _, err := io.ReadFull(keploy, body); err != nil {
		t.Fatal(err)
	}
	return append(header, body...)
}

// ServerNameOf reads the server name a client's ClientHello asks for, one
// sent in one record or spread over two, and returns every byte it read, which
// is the ClientHello whole: handed on, it is what a server reads. A
// ClientHello with no SNI names "". Bytes that are not a ClientHello are an
// error, and are returned too.
func TestServerNameOfReadsTheClientHelloWithoutAnsweringIt(t *testing.T) {
	split := func(hello []byte) []byte {
		body := hello[5:]
		half := len(body) / 2
		record := func(p []byte) []byte {
			return append([]byte{hello[0], hello[1], hello[2], byte(len(p) >> 8), byte(len(p))}, p...)
		}
		return append(record(body[:half]), record(body[half:])...)
	}
	for _, tc := range []struct {
		name    string
		wire    func(t *testing.T) []byte
		want    string
		wantErr bool
	}{
		{"an SNI", func(t *testing.T) []byte { return clientHelloOf(t, "api.example.com") }, "api.example.com", false},
		{"no SNI", func(t *testing.T) []byte { return clientHelloOf(t, "") }, "", false},
		{"an SNI over two records", func(t *testing.T) []byte { return split(clientHelloOf(t, "api.example.com")) }, "api.example.com", false},
		{"not TLS", func(*testing.T) []byte { return []byte("GET / HTTP/1.1\r\nHost: a\r\n\r\n") }, "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			wire := tc.wire(t)
			// What follows the ClientHello is not read: the client waits
			// for its server's answer.
			r := io.MultiReader(bytes.NewReader(wire), blockingReader{})
			type result struct {
				name string
				read []byte
				err  error
			}
			done := make(chan result, 1)
			go func() {
				name, read, err := ServerNameOf(r)
				done <- result{name, read, err}
			}()
			var got result
			select {
			case got = <-done:
			case <-time.After(30 * time.Second):
				t.Fatal("ServerNameOf read past the ClientHello, or never returned")
			}
			if tc.wantErr != (got.err != nil) {
				t.Fatalf("err = %v, want an error: %v", got.err, tc.wantErr)
			}
			if got.name != tc.want {
				t.Fatalf("server name = %q, want %q", got.name, tc.want)
			}
			if !tc.wantErr && !bytes.Equal(got.read, wire) {
				t.Fatalf("read %d bytes, want the ClientHello's %d", len(got.read), len(wire))
			}
			if tc.wantErr && !bytes.HasPrefix(wire, got.read) {
				t.Fatalf("returned %q, not what it read of %q", got.read, wire)
			}
		})
	}
}

// blockingReader is a client that has sent all it sends until answered.
type blockingReader struct{}

func (blockingReader) Read([]byte) (int, error) { select {} }
