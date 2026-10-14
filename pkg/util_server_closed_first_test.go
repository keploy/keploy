package pkg

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

// signalOnCloseConn tells closed when the transport closes its side of the
// connection.
type signalOnCloseConn struct {
	net.Conn
	once   sync.Once
	closed chan struct{}
}

func (c *signalOnCloseConn) Close() error {
	c.once.Do(func() { close(c.closed) })
	return c.Conn.Close()
}

// postToAServerThatClosesFirst sends a POST to a server that closes each
// connection as it accepts it, as docker's proxy does at a published port no
// app listens behind, and holds the request back until the client's
// transport has read that close and closed the connection itself. The close
// then always comes before the request is registered on the connection, and
// net/http returns its errServerClosedIdle for it, undecorated, on a fresh
// connection. Which of the two shapes a real drop takes is a race between the
// transport's read loop and the request: under -race, replay's
// TestADroppedConnectionAtAnUnreachableAppPortSaysSo got this one in 94 of 500
// runs on main.
func postToAServerThatClosesFirst(t *testing.T) error {
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

	var conn *signalOnCloseConn
	transport := &http.Transport{DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
		c, err := (&net.Dialer{}).DialContext(ctx, network, addr)
		if err != nil {
			return nil, err
		}
		conn = &signalOnCloseConn{Conn: c, closed: make(chan struct{})}
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
		http.MethodPost, "http://"+ln.Addr().String()+"/echo", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	_, err = (&http.Client{Transport: transport}).Do(req)
	if !held {
		t.Fatal("the transport did not close the connection within 5s of the server's close")
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
