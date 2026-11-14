package util

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"

	"go.keploy.io/server/v3/internal/logleak"
)

// The proxy reads every outgoing HTTP/1 request's head with
// ReadHTTPHeadersUntilEnd, whole or split over several reads, and the agent
// logs at debug level: the head holds the request's credentials (an
// Authorization header, a cookie, an API key), so it logs the head's size
// only. It logged the whole head, Authorization and all.
func TestReadHTTPHeadersUntilEndLogsNoneOfTheHead(t *testing.T) {
	const s = logleak.Canary
	head := "GET /v1/me?access_token=" + s + " HTTP/1.1\r\nHost: api.example.com\r\nAuthorization: Bearer " + s + "\r\nCookie: session=" + s + "\r\n\r\n"
	for _, tc := range []struct {
		name  string
		parts []string
	}{
		{"in one read", []string{head}},
		{"split in a header's value", []string{head[:60], head[60:]}},
		{"split over three reads", []string{head[:20], head[20:90], head[90:]}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			logger, logs := logleak.Observe()
			app, agent := net.Pipe()
			t.Cleanup(func() { _ = app.Close(); _ = agent.Close() })
			go func() {
				for _, p := range tc.parts {
					if _, err := app.Write([]byte(p)); err != nil {
						return
					}
					time.Sleep(5 * time.Millisecond)
				}
			}()
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			got, err := ReadHTTPHeadersUntilEnd(ctx, logger, agent)
			if err != nil || string(got) != head {
				t.Fatalf("read %d bytes, %v; want the whole head", len(got), err)
			}
			logleak.Check(t, logs, s, "ReadHTTPHeadersUntilEnd")
		})
	}
}

// A relay's read that fails with bytes in hand logs their size, never them:
// they are the application's traffic, of any protocol.
func TestReadBuffConnLogsNoneOfAFailedReadsBytes(t *testing.T) {
	const s = logleak.Canary
	logger, logs := logleak.Observe()
	conn := &failingConn{data: []byte("AUTH app " + s + "\r\n"), err: errors.New("connection reset by peer")}
	bufs, errs := make(chan []byte, 4), make(chan error, 4)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		ReadBuffConn(ctx, logger, conn, bufs, errs, false)
	}()
	select {
	case <-errs:
	case <-ctx.Done():
		t.Fatal("the failed read was never reported")
	}
	cancel()
	<-done
	logleak.Check(t, logs, s, "ReadBuffConn")
	if len(logs.FilterMessage("Failed to read buffer").All()) == 0 {
		t.Fatal("precondition: the failed read's line was not written; the test drove nothing")
	}
}

// failingConn hands its data with its error on the first read, then the
// error alone.
type failingConn struct {
	net.Conn
	data []byte
	err  error
}

func (c *failingConn) Read(p []byte) (int, error) {
	n := copy(p, c.data)
	c.data = c.data[n:]
	return n, c.err
}

func (c *failingConn) SetReadDeadline(time.Time) error { return nil }
func (c *failingConn) Close() error                    { return nil }
