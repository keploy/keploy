package tls

import (
	"bytes"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"time"
)

// errHelloRead ends ServerNameOf's handshake once it has the ClientHello.
var errHelloRead = errors.New("the ClientHello is read")

// ServerNameOf reads the ClientHello a TLS client sends first off r, and
// returns the server name it asks for (its SNI, "" for none) and every byte it
// read of r, which the caller hands on ahead of the rest of r. Nothing is
// written to the client: its handshake is left whole for whatever reads it
// next, keploy's MITM or the server a pass-through reaches. A ClientHello
// spread over several TLS records is read whole, as crypto/tls reads one. err
// is a ClientHello that could not be read (r ended first, or what it sent is
// not one); the bytes read are returned all the same.
func ServerNameOf(r io.Reader) (string, []byte, error) {
	var read bytes.Buffer
	var name string
	got := false
	err := tls.Server(&helloConn{r: io.TeeReader(r, &read)}, &tls.Config{
		GetConfigForClient: func(hello *tls.ClientHelloInfo) (*tls.Config, error) {
			name, got = hello.ServerName, true
			return nil, errHelloRead
		},
	}).Handshake()
	if got {
		return name, read.Bytes(), nil
	}
	return "", read.Bytes(), err
}

// helloConn is the client's side of ServerNameOf's handshake: it reads the
// client, and drops what the handshake would write to it (the alert that ends
// it), so the client sees nothing of it.
type helloConn struct{ r io.Reader }

func (c *helloConn) Read(b []byte) (int, error)       { return c.r.Read(b) }
func (c *helloConn) Write(b []byte) (int, error)      { return len(b), nil }
func (c *helloConn) Close() error                     { return nil }
func (c *helloConn) LocalAddr() net.Addr              { return &net.TCPAddr{} }
func (c *helloConn) RemoteAddr() net.Addr             { return &net.TCPAddr{} }
func (c *helloConn) SetDeadline(time.Time) error      { return nil }
func (c *helloConn) SetReadDeadline(time.Time) error  { return nil }
func (c *helloConn) SetWriteDeadline(time.Time) error { return nil }
