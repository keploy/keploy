package pkg

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

// closedFirstConn is the transport's connection to a server that closed it
// first. Its Close only tells closed: the socket stays open until the caller
// closes it, once the request is done. net/http's read loop reads the
// server's close and closes the connection, but its writer may already have
// taken the request (a select inside net/http decides). On a closed socket
// that write fails, and roundTrip may return the write's error instead
// (postWrittenAfterTheTransportClosed); on the open one it goes through, as a
// write made before the transport read the close does, and the request always
// gets the answer the transport made on reading the close.
type closedFirstConn struct {
	net.Conn
	once   sync.Once
	closed chan struct{}
}

func (c *closedFirstConn) Close() error {
	c.once.Do(func() { close(c.closed) })
	return nil
}

// serveClosingEachConn listens on loopback and closes each connection as it
// accepts it, as docker's proxy does at a published port no app listens
// behind.
func serveClosingEachConn(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			_ = c.Close()
		}
	}()
	return ln.Addr().String()
}

// postToAServerThatClosesFirst sends a POST to a server that closes each
// connection as it accepts it (serveClosingEachConn), and holds the request
// back until the client's transport has read that close and closed the
// connection itself. The close then always comes before the request is
// registered on the connection, and net/http returns its errServerClosedIdle
// for it, undecorated, on a fresh connection. A real drop races the
// transport's read loop against the request: under -race, replay's
// TestADroppedConnectionAtAnUnreachableAppPortSaysSo got this shape, not
// io.EOF, in 94 of 500 runs on main. When the read loop wins, the request can
// still get its write to the closed connection instead
// (postWrittenAfterTheTransportClosed):
// TestIsTransportConnResetClassifiesAServerThatClosedTheConnectionFirst got
// that in 6 of 10,000 runs under load, before closedFirstConn kept the socket
// open.
func postToAServerThatClosesFirst(t *testing.T) error {
	t.Helper()
	addr := serveClosingEachConn(t)

	var conn *closedFirstConn
	transport := &http.Transport{DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
		c, err := (&net.Dialer{}).DialContext(ctx, network, addr)
		if err != nil {
			return nil, err
		}
		conn = &closedFirstConn{Conn: c, closed: make(chan struct{})}
		return conn, nil
	}}
	t.Cleanup(transport.CloseIdleConnections)
	held := false
	trace := &httptrace.ClientTrace{GotConn: func(httptrace.GotConnInfo) {
		select {
		case <-conn.closed:
			held = true
		case <-time.After(5 * time.Second):
		}
	}}
	req, err := http.NewRequestWithContext(httptrace.WithClientTrace(context.Background(), trace),
		http.MethodPost, "http://"+addr+"/echo", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	_, err = (&http.Client{Transport: transport}).Do(req)
	if conn != nil {
		_ = conn.Conn.Close()
	}
	if !held {
		t.Fatal("the transport did not close the connection within 5s of the server's close")
	}
	return err
}

// writeAfterCloseTransport makes, on a connection dial makes, the calls
// net/http's transport makes when its writer took the request before it saw
// the transport's own close: the read loop reads the server's close and
// closes the connection, and the writer then writes the request to it.
type writeAfterCloseTransport struct {
	dial func(ctx context.Context) (net.Conn, error)
}

func (tr writeAfterCloseTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	c, err := tr.dial(req.Context())
	if err != nil {
		_ = req.Body.Close()
		return nil, err
	}
	if n, err := c.Read(make([]byte, 1)); n != 0 || err != io.EOF {
		_ = c.Close()
		_ = req.Body.Close()
		return nil, fmt.Errorf("read %d bytes and %v; want the server's close (io.EOF) first", n, err)
	}
	_ = c.Close()
	if err := req.Write(c); err != nil {
		return nil, err
	}
	return nil, errors.New("a request was written whole to a connection closed before it")
}

// postWrittenAfterTheTransportClosed sends a POST through an http.Client to a
// server that closes each connection as it accepts it (over TLS, once the
// handshake is done), written as net/http's transport writes it when the drop
// ends the other way than postToAServerThatClosesFirst's: its read loop read
// the server's close and closed the connection, its writer had taken the
// request before it saw that, and roundTrip's select picked the write's error
// over the close. A select inside net/http decides that, which no hook
// outside it orders, so writeAfterCloseTransport makes the same calls in that
// order. Over TCP the write's error is a *net.OpError; over TLS it is
// net.ErrClosed itself, which a *tls.Conn's Write returns after its Close.
func postWrittenAfterTheTransportClosed(t *testing.T, overTLS bool) error {
	t.Helper()
	tr := writeAfterCloseTransport{}
	var target string
	if !overTLS {
		addr := serveClosingEachConn(t)
		target = "http://" + addr + "/echo"
		tr.dial = func(ctx context.Context) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "tcp", addr)
		}
	} else {
		// httptest's certificate, for 127.0.0.1, on a listener of our own
		// that closes each connection once its handshake is done.
		cert := httptest.NewUnstartedServer(nil)
		cert.StartTLS()
		cfg := cert.TLS.Clone()
		roots := x509.NewCertPool()
		roots.AddCert(cert.Certificate())
		cert.Close()
		ln, err := tls.Listen("tcp", "127.0.0.1:0", cfg)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = ln.Close() })
		go func() {
			for {
				c, err := ln.Accept()
				if err != nil {
					return
				}
				_ = c.(*tls.Conn).Handshake()
				_ = c.Close()
			}
		}()
		target = "https://" + ln.Addr().String() + "/echo"
		tr.dial = func(ctx context.Context) (net.Conn, error) {
			return (&tls.Dialer{Config: &tls.Config{RootCAs: roots, ServerName: "127.0.0.1"}}).DialContext(ctx, "tcp", ln.Addr().String())
		}
	}
	req, err := http.NewRequest(http.MethodPost, target, strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := (&http.Client{Transport: tr}).Do(req)
	if err == nil {
		_ = resp.Body.Close()
		t.Fatal("a request written to a closed connection got an answer")
	}
	return err
}

// A connection the server closes before the request is on it is the same
// drop as one it closes after: no answer, and nothing of the request read.
// net/http reports the first as errServerClosedIdle, not as io.EOF, and
// IsTransportConnReset classifies both, so the reset re-send and the
// unreachable-app-port check see either shape.
func TestIsTransportConnResetClassifiesAServerThatClosedTheConnectionFirst(t *testing.T) {
	err := postToAServerThatClosesFirst(t)
	if err == nil {
		t.Fatal("a POST to a server that closes every connection succeeded")
	}
	var ue *url.Error
	if !errors.As(err, &ue) || ue.Err.Error() != netHTTPServerClosedIdle {
		t.Fatalf("got %v; want net/http's %q, which this test provokes", err, netHTTPServerClosedIdle)
	}
	if !IsTransportConnReset(err) {
		t.Fatalf("not classified as a transport reset: %v", err)
	}
}
