//go:build linux

package proxy

import (
	"context"
	"crypto/tls"
	"net"
	"sync"
	"testing"
	"time"

	"go.uber.org/zap"

	"go.keploy.io/server/v3/pkg/agent/containerkey"
	pTls "go.keploy.io/server/v3/pkg/agent/proxy/tls"
	"go.keploy.io/server/v3/pkg/agent/proxy/util"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
)

// starvedConn is the proxy's side of a connection whose handler a loaded host
// does not run once its read has taken the client's hang-up from the socket:
// a read that returns nothing and an error (the end of the stream, or a
// reset: the client may close with keploy's flight unread) holds it until run
// is closed,
// as a goroutine whose system call is done and that the scheduler has not
// run since. eof is closed when it has.
type starvedConn struct {
	*net.TCPConn
	eof, run chan struct{}
	once     sync.Once
}

func (c *starvedConn) Read(b []byte) (int, error) {
	n, err := c.TCPConn.Read(b)
	if n == 0 && err != nil {
		c.once.Do(func() { close(c.eof) })
		<-c.run
	}
	return n, err
}

// A client's hang-up on a TLS handshake whose handler the host has not run
// since it took the hang-up from the socket is charged to the test it
// happened in: the test's end waits for it to be told, in a record as in a
// replay, and only then reads what the test lost and ends its scope. Before,
// the FIN left nothing untaken and the handler was counted as waiting for
// the client, so the end read the counts and closed the scope first; the
// handler, run later, found itself outside the scope the handshake began in
// and dropped the abandon: the test was judged on a call keploy lost.
//
// The handshake is handed the tracked connection itself, and, as the proxy
// hands it (handleConnection, the CONNECT path, opportunistic TLS), in its
// util.Conn wrapper: the hold is found through the wrapper's NetConn.
func TestAHangUpItsStarvedHandlerHasNotToldIsChargedToItsTest(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	hands := []struct {
		name string
		conn func(tracked net.Conn) net.Conn
	}{
		{"", func(c net.Conn) net.Conn { return c }},
		{", handed in the proxy's util.Conn wrapper", func(c net.Conn) net.Conn {
			return &util.Conn{Conn: c, Reader: c, Logger: zap.NewNop()}
		}},
	}
	for _, tc := range []struct {
		name string
		// hangUp is the client: it hangs up without a word.
		hangUp func(t *testing.T, app *net.TCPConn)
	}{
		{"once keploy sent its certificate, during the handshake", func(t *testing.T, app *net.TCPConn) {
			if _, err := app.Write(rawHello(t, "dep.example")); err != nil {
				t.Error(err)
			}
			// keploy's first flight, its certificate in it.
			if _, err := app.Read(make([]byte, 1)); err != nil {
				t.Error(err)
			}
		}},
		{"as soon as the handshake was done, having sent nothing", func(t *testing.T, app *net.TCPConn) {
			c := tls.Client(app, &tls.Config{InsecureSkipVerify: true, ServerName: "dep.example", MaxVersion: tls.VersionTLS12})
			if err := c.Handshake(); err != nil {
				t.Errorf("the client's handshake: %v", err)
			}
		}},
	} {
		for _, answers := range []bool{false, true} {
			for _, hand := range hands {
				name := tc.name + map[bool]string{false: ", in a record", true: ", in a replay"}[answers] + hand.name
				t.Run(name, func(t *testing.T) {
					ck := containerkey.Default
					ck.Reset()
					t.Cleanup(ck.Reset)
					p := &Proxy{logger: zap.NewNop()}
					app, side := tcpPair(t)
					starved := &starvedConn{TCPConn: side, eof: make(chan struct{}), run: make(chan struct{})}
					var runOnce sync.Once
					run := func() { runOnce.Do(func() { close(starved.run) }) }
					t.Cleanup(run)
					tracked := p.inflight.track(starved)

					ck.BeginTest() // the test the client's call is made in
					handled := make(chan struct{})
					go func() {
						defer close(handled)
						defer p.inflight.forget(tracked)
						defer func() { _ = tracked.Close() }()
						c, _, err := pTls.HandleTLSConnection(p.withClientTLS(context.Background(), "10.0.0.1:443"), zap.NewNop(), hand.conn(tracked), time.Time{})
						if err == nil {
							_, _ = c.Read(make([]byte, 1)) // the client's first request
						}
					}()
					tc.hangUp(t, app)
					_ = app.Close() // no alert, no close_notify
					<-starved.eof   // the handler's read took the hang-up; it is not run since

					// The test's end, as the agent's (endTest, closeOpen):
					// its calls settled, by events, then what it lost read,
					// then its scope closed.
					waiting := make(chan struct{})
					lost := make(chan uint64, 1)
					go func() {
						first := true
						for {
							changed := p.inflight.Changed()
							if p.inflight.Pending(answers) == 0 {
								break
							}
							if first {
								first = false
								close(waiting)
							}
							<-changed
						}
						lost <- lossesOf(p, models.CaptureLossTLSAbandoned)
						ck.EndTest()
					}()
					select {
					case <-waiting:
					case n := <-lost:
						t.Fatalf("the test's end did not wait for a hang-up its handler had not told: it read %d abandoned, and closed the test's scope", n)
					}
					run() // the host runs the handler at last
					if n := <-lost; n != 1 {
						t.Fatalf("the test's end read %d tls_handshake_abandoned, want 1: the hang-up is the test's", n)
					}
					<-handled
					if n := lossesOf(p, models.CaptureLossTLSAbandoned); n != 1 {
						t.Fatalf("%d tls_handshake_abandoned in all, want 1", n)
					}
				})
			}
		}
	}
}

// A hold on a client's hang-up holds no work while the client is still
// there: a spare whose handshake is done and that has sent nothing yet never
// keeps a test's end waiting. Its first byte releases the hold: a later
// hang-up is an ordinary close, not a silent one.
func TestAHangUpHoldHoldsNoWorkWhileItsClientIsThere(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	p := &Proxy{logger: zap.NewNop()}
	app, side := tcpPair(t)
	tracked := p.inflight.track(side)
	handshook, read := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(read)
		defer p.inflight.forget(tracked)
		defer func() { _ = tracked.Close() }()
		c, _, err := pTls.HandleTLSConnection(p.withClientTLS(context.Background(), "10.0.0.1:443"), zap.NewNop(), tracked, time.Time{})
		close(handshook)
		if err == nil {
			_, _ = c.Read(make([]byte, 1))
		}
	}()
	c := tls.Client(app, &tls.Config{InsecureSkipVerify: true, ServerName: "dep.example"})
	if err := c.Handshake(); err != nil {
		t.Fatalf("the client's handshake: %v", err)
	}
	<-handshook
	if n := tracked.hangUps.Load(); n != 1 {
		t.Fatalf("%d holds on the hang-up after the handshake, want 1 until the client's first byte", n)
	}
	// A record's end reads only untaken bytes and hang-ups (a replay's
	// also waits for a handler not yet reading): the client sent nothing
	// since its Finished, which the handshake read, and is still there.
	if n := p.inflight.Pending(false); n != 0 {
		t.Fatal("a connection whose client is still there, silent since its handshake, held work")
	}
	if _, err := c.Write([]byte("x")); err != nil {
		t.Fatal(err)
	}
	<-read
	if n := tracked.hangUps.Load(); n != 0 {
		t.Fatalf("%d holds on the hang-up after the client's first byte, want 0", n)
	}
}
