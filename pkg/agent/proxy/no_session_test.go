package proxy

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"syscall"
	"testing"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/agent"
	syncMock "go.keploy.io/server/v3/pkg/agent/proxy/syncMock"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// Global passthrough, or an agent started to record, passes a connection
// through while it has no session; every other case, an unknown mode
// included, closes it.
func TestNoSessionActionFor(t *testing.T) {
	for _, tc := range []struct {
		mode   models.Mode
		global bool
		want   noSessionAction
	}{
		{models.MODE_RECORD, false, noSessionPassThrough},
		{models.MODE_TEST, false, noSessionClose},
		{models.MODE_OFF, false, noSessionClose},
		{"", false, noSessionClose},
		{"replay", false, noSessionClose},
		{models.MODE_RECORD, true, noSessionPassThrough},
		{models.MODE_TEST, true, noSessionPassThrough},
		{"", true, noSessionPassThrough},
	} {
		if got := noSessionActionFor(tc.mode, tc.global); got != tc.want {
			t.Errorf("noSessionActionFor(%q, global passthrough %v) = %v, want %v", tc.mode, tc.global, got, tc.want)
		}
	}
}

// echoUpstream is a destination that echoes what it reads until the proxy
// ends its side, then closes. accepted delivers each connection it takes,
// and ended the error its echo ended with (nil: a clean end of stream).
func echoUpstream(t *testing.T) (addr string, accepted <-chan net.Conn, ended <-chan error) {
	t.Helper()
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	acc := make(chan net.Conn, 1)
	end := make(chan error, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer func() { _ = c.Close() }()
		acc <- c
		_, err = io.Copy(c, c)
		end <- err
	}()
	return ln.Addr().String(), acc, end
}

// readN reads exactly n bytes from c in the background and delivers them, or
// nothing if the connection ends first.
func readN(c net.Conn, n int) <-chan string {
	got := make(chan string, 1)
	go func() {
		buf := make([]byte, n)
		if _, err := io.ReadFull(c, buf); err == nil {
			got <- string(buf)
		}
	}()
	return got
}

// readErr reads from c in the background until it fails, and delivers the
// error: io.EOF for a clean end of stream.
func readErr(c net.Conn) <-chan error {
	got := make(chan error, 1)
	go func() {
		_, err := io.Copy(io.Discard, c)
		if err == nil {
			err = io.EOF
		}
		got <- err
	}()
	return got
}

// While the agent has no session, a connection is passed through to its
// destination, as if keploy were not there, when the agent was started to
// record or told to pass everything through; both of its connections are
// closed once the exchange is over.
func TestHandleConnectionWithoutASessionPassesThrough(t *testing.T) {
	for _, tc := range []struct {
		mode   models.Mode
		global bool
	}{
		{models.MODE_RECORD, false},
		{models.MODE_TEST, true},
		{"", true},
	} {
		t.Run(fmt.Sprintf("mode=%q global_passthrough=%v", tc.mode, tc.global), func(t *testing.T) {
			upstream, _, upstreamEnded := echoUpstream(t)
			app, src := acceptedConn(t)
			p := &Proxy{logger: zap.NewNop(), DestInfo: fakeDestInfo{dest: ipv4Dest(t, upstream)}, agentMode: tc.mode, GlobalPassthrough: tc.global}
			dst := predialedTo(t, p, src, upstream)

			done := make(chan error, 1)
			go func() { done <- p.handleConnection(context.Background(), src) }()

			if _, err := app.Write([]byte("ping")); err != nil {
				t.Fatal(err)
			}
			// The echo can only come back through the passthrough, and the
			// passthrough cannot end before the application ends its side
			// below.
			select {
			case err := <-done:
				t.Fatalf("handleConnection returned (%v) without passing the connection through", err)
			case got := <-readN(app, 4):
				if got != "ping" {
					t.Fatalf("the application read %q through the passthrough, want %q", got, "ping")
				}
			}

			if err := app.CloseWrite(); err != nil {
				t.Fatal(err)
			}
			if err := awaitEvent(t, done, "handleConnection returning once both sides ended"); err != nil {
				t.Fatalf("handleConnection: %v", err)
			}
			if src.closes.Load() == 0 {
				t.Fatal("handleConnection returned without closing the application's connection")
			}
			if dst.closes.Load() == 0 {
				t.Fatal("handleConnection returned without closing its connection to the destination")
			}
			if rest, err := io.ReadAll(app); err != nil || len(rest) != 0 {
				t.Fatalf("the application's end read %q, %v after the exchange; want a clean end of stream", rest, err)
			}
			if err := awaitEvent(t, upstreamEnded, "the destination seeing its connection end"); err != nil {
				t.Fatalf("the destination's connection did not end cleanly: %v", err)
			}
		})
	}
}

// A reset on either side of a passthrough reaches the other side at once, as
// it would without keploy in between: the application sees the destination's
// reset as a reset, not a clean end of stream after a wait, and the
// destination sees the application's.
func TestHandleConnectionWithoutASessionPassesOnAResetFromEitherSide(t *testing.T) {
	t.Run("the destination resets", func(t *testing.T) {
		ln, err := net.Listen("tcp4", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = ln.Close() })
		accepted := make(chan struct{}, 1)
		go func() {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			accepted <- struct{}{}
			buf := make([]byte, 4)
			_, _ = io.ReadFull(c, buf)
			_ = c.(*net.TCPConn).SetLinger(0)
			_ = c.Close()
		}()
		app, src := acceptedConn(t)
		p := &Proxy{logger: zap.NewNop(), DestInfo: fakeDestInfo{dest: ipv4Dest(t, ln.Addr().String())}, agentMode: models.MODE_RECORD}
		done := make(chan error, 1)
		go func() { done <- p.handleConnection(context.Background(), src) }()
		requireDialled(t, accepted, done)

		if _, err := app.Write([]byte("ping")); err != nil {
			t.Fatal(err)
		}
		if err := awaitEvent(t, readErr(app), "the application's read ending"); !errors.Is(err, syscall.ECONNRESET) {
			t.Fatalf("the application's read ended with %v; want the destination's reset", err)
		}
		if err := awaitEvent(t, done, "handleConnection returning after the reset"); err != nil {
			t.Fatalf("handleConnection: %v", err)
		}
	})

	t.Run("the application resets", func(t *testing.T) {
		ln, err := net.Listen("tcp4", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = ln.Close() })
		accepted := make(chan struct{}, 1)
		got := make(chan struct{})
		upstreamRead := make(chan error, 1)
		go func() {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			defer func() { _ = c.Close() }()
			accepted <- struct{}{}
			buf := make([]byte, 4)
			if _, err := io.ReadFull(c, buf); err != nil {
				upstreamRead <- err
				return
			}
			close(got)
			upstreamRead <- <-readErr(c)
		}()
		app, src := acceptedConn(t)
		p := &Proxy{logger: zap.NewNop(), DestInfo: fakeDestInfo{dest: ipv4Dest(t, ln.Addr().String())}, agentMode: models.MODE_RECORD}
		done := make(chan error, 1)
		go func() { done <- p.handleConnection(context.Background(), src) }()
		requireDialled(t, accepted, done)

		if _, err := app.Write([]byte("ping")); err != nil {
			t.Fatal(err)
		}
		awaitEvent(t, got, "the destination receiving the application's bytes")
		_ = app.SetLinger(0)
		_ = app.Close()
		if err := awaitEvent(t, upstreamRead, "the destination's read ending"); !errors.Is(err, syscall.ECONNRESET) {
			t.Fatalf("the destination's read ended with %v; want the application's reset", err)
		}
		if err := awaitEvent(t, done, "handleConnection returning after the reset"); err != nil {
			t.Fatalf("handleConnection: %v", err)
		}
	})
}

// requireDialled fails the test unless handleConnection dialled the
// destination before returning. Called before the application sends
// anything, while a passthrough cannot have ended yet: done is ready first
// only when handleConnection returned without passing the connection through.
func requireDialled(t *testing.T, accepted <-chan struct{}, done <-chan error) {
	t.Helper()
	select {
	case <-accepted:
	case err := <-done:
		t.Fatalf("handleConnection returned (%v) without passing the connection through", err)
	}
}

// A passthrough made without a session ends when the proxy shuts down,
// resetting both ends: the relay itself does not watch the context, a
// passthrough to a destination that keeps its connection open would otherwise
// outlive the proxy, and an exchange cut off mid-message must not look
// complete to either peer.
func TestHandleConnectionWithoutASessionResetsBothEndsOnShutdown(t *testing.T) {
	upstream, accepted, upstreamEnded := echoUpstream(t)
	app, src := acceptedConn(t)
	p := &Proxy{logger: zap.NewNop(), DestInfo: fakeDestInfo{dest: ipv4Dest(t, upstream)}, agentMode: models.MODE_RECORD}
	dst := predialedTo(t, p, src, upstream)
	<-accepted

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- p.handleConnection(ctx, src) }()

	if _, err := app.Write([]byte("ping")); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		t.Fatalf("handleConnection returned (%v) without passing the connection through", err)
	case <-readN(app, 4):
	}

	cancel()
	if err := awaitEvent(t, done, "handleConnection returning after the proxy shut down"); err != nil {
		t.Fatalf("handleConnection: %v", err)
	}
	if dst.closes.Load() == 0 {
		t.Fatal("the passthrough's connection to the destination was left open")
	}
	if src.closes.Load() == 0 {
		t.Fatal("handleConnection returned without closing the application's connection")
	}
	requireReset(t, app, upstreamEnded, "the proxy shut down")
}

// requireReset fails the test unless both peers of a passthrough saw it end
// in a reset: app's next read, and the destination's read, reported in ended.
func requireReset(t *testing.T, app net.Conn, ended <-chan error, why string) {
	t.Helper()
	if err := awaitEvent(t, readErr(app), "the application's read ending"); !errors.Is(err, syscall.ECONNRESET) {
		t.Fatalf("after %s the application's read ended with %v; want a reset, so a cut-off message cannot look complete", why, err)
	}
	if err := awaitEvent(t, ended, "the destination's read ending"); !errors.Is(err, syscall.ECONNRESET) {
		t.Fatalf("after %s the destination's read ended with %v; want a reset, so a cut-off message cannot look complete", why, err)
	}
}

// keepAliveHTTPUpstream is a destination that answers every request with its
// path and keeps the connection open for the next one, as a pooled
// dependency does.
func keepAliveHTTPUpstream(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, r.URL.Path)
	}))
	t.Cleanup(srv.Close)
	return strings.TrimPrefix(srv.URL, "http://")
}

// get sends a GET for path on c and delivers the response body, or the error
// that ended the exchange.
func get(c net.Conn, br *bufio.Reader, path string, closeAfter bool) <-chan string {
	got := make(chan string, 1)
	go func() {
		req := "GET " + path + " HTTP/1.1\r\nHost: dependency\r\n"
		if closeAfter {
			req += "Connection: close\r\n"
		}
		if _, err := io.WriteString(c, req+"\r\n"); err != nil {
			got <- "error: " + err.Error()
			return
		}
		res, err := http.ReadResponse(br, nil)
		if err != nil {
			got <- "error: " + err.Error()
			return
		}
		defer func() { _ = res.Body.Close() }()
		body, err := io.ReadAll(res.Body)
		if err != nil {
			got <- "error: " + err.Error()
			return
		}
		got <- string(body)
	}()
	return got
}

// A connection passed through before the session existed does not outlive
// the session's start: when Record (or Mock) has set the session up, it is
// reset, so the application's pool reconnects, and the reconnection is
// recorded. Left open, everything a pooled connection carried from then on
// would escape the recording without a word.
func TestTheSessionResetsWhatWasPassedThroughBeforeIt(t *testing.T) {
	upstream := keepAliveHTTPUpstream(t)
	core, logs := observer.New(zapcore.WarnLevel)
	cfg := config.New()
	cfg.Agent.Mode = models.MODE_RECORD
	p := New(zap.New(core), fakeDestInfo{dest: ipv4Dest(t, upstream)}, cfg)
	if err := p.InitIntegrations(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { syncMock.Get().SetOutputChannel(nil) })

	// Opened, and used, while there is no session.
	app1, src1 := acceptedConn(t)
	dst1 := predialedTo(t, p, src1, upstream)
	done1 := make(chan error, 1)
	go func() { done1 <- p.handleConnection(context.Background(), src1) }()
	br1 := bufio.NewReader(app1)
	select {
	case err := <-done1:
		t.Fatalf("handleConnection returned (%v) without passing the connection through", err)
	case body := <-get(app1, br1, "/before", false):
		if body != "/before" {
			t.Fatalf("the call before the session got %q, want %q", body, "/before")
		}
	}

	mocks := make(chan *models.Mock, 16)
	if err := p.Record(context.Background(), mocks, models.OutgoingOptions{DisableMysqlAutoDetect: true}); err != nil {
		t.Fatal(err)
	}
	// Record has returned: the passthrough is already closed.
	if src1.closes.Load() == 0 || dst1.closes.Load() == 0 {
		t.Fatalf("the connection passed through before the session is still open after Record (application end closed %d times, destination end %d)",
			src1.closes.Load(), dst1.closes.Load())
	}
	if err := awaitEvent(t, done1, "handleConnection returning after the session started"); err != nil {
		t.Fatalf("handleConnection: %v", err)
	}
	// The pool's next use of it finds it reset: a cut, not a clean end of
	// stream that could pass for the end of a message.
	if err := awaitEvent(t, readErr(app1), "the pooled connection's read ending"); !errors.Is(err, syscall.ECONNRESET) {
		t.Fatalf("after Record the pooled connection's read ended with %v; want a reset", err)
	}
	warned := logs.FilterMessageSnippet("the session started").FilterField(zap.Int("connections", 1)).Len()
	if warned != 1 {
		t.Fatalf("%d WARN lines saying the session closed one passed-through connection, want 1: %v", warned, logs.All())
	}

	// The application reconnects, and that connection is recorded.
	app2, src2 := acceptedConn(t)
	done2 := make(chan error, 1)
	go func() { done2 <- p.handleConnection(context.Background(), src2) }()
	if body := awaitEvent(t, get(app2, bufio.NewReader(app2), "/after", true), "the reconnection's call"); body != "/after" {
		t.Fatalf("the reconnection's call got %q, want %q", body, "/after")
	}
	// It asked for the connection to be closed, and closes its end, as an
	// HTTP client does.
	_ = app2.Close()
	recorded := make(chan string, 1)
	go func() {
		for m := range mocks {
			if m.Spec.HTTPReq != nil {
				recorded <- m.Spec.HTTPReq.URL
				return
			}
		}
	}()
	if url := awaitEvent(t, recorded, "a mock for the reconnection's call"); !strings.HasSuffix(url, "/after") {
		t.Fatalf("the first mock recorded after the session started is for %q, want the reconnection's /after", url)
	}
	awaitEvent(t, done2, "the reconnection's handleConnection returning")
}

// An agent started to record whose first session is a replay resets what it
// passed through too: from Mock on, a connection's calls are served from
// mocks, never from the live dependency.
func TestAReplaySessionResetsWhatARecordingAgentPassedThrough(t *testing.T) {
	upstream, _, upstreamEnded := echoUpstream(t)
	cfg := config.New()
	cfg.Agent.Mode = models.MODE_RECORD
	p := New(zap.NewNop(), fakeDestInfo{dest: ipv4Dest(t, upstream)}, cfg)
	// Preseeded so Mock() leaves the host's /etc/nsswitch.conf alone (see
	// testProxy).
	p.nsswitchData = []byte("preseeded-by-test")

	app, src := acceptedConn(t)
	dst := predialedTo(t, p, src, upstream)
	done := make(chan error, 1)
	go func() { done <- p.handleConnection(context.Background(), src) }()
	if _, err := app.Write([]byte("ping")); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		t.Fatalf("handleConnection returned (%v) without passing the connection through", err)
	case <-readN(app, 4):
	}

	if err := p.Mock(context.Background(), models.OutgoingOptions{Mocking: true}); err != nil {
		t.Fatal(err)
	}
	if src.closes.Load() == 0 || dst.closes.Load() == 0 {
		t.Fatalf("the connection passed through before the replay session is still open after Mock (application end closed %d times, destination end %d)",
			src.closes.Load(), dst.closes.Load())
	}
	if err := awaitEvent(t, done, "handleConnection returning after the session started"); err != nil {
		t.Fatalf("handleConnection: %v", err)
	}
	requireReset(t, app, upstreamEnded, "Mock returned")
}

// Once a session is set up, a passthrough it would not have made is reset and
// none opens, even one whose decision was made before it; one made under
// global passthrough survives a session that passes connections through too,
// and is reset by one that intercepts TLS instead.
func TestNoSessionPassthroughsAfterTheSessionStarts(t *testing.T) {
	newPT := func(global bool) (*noSessionPassthrough, *closeCountingConn, *closeCountingConn) {
		_, src := acceptedConn(t)
		_, dst := acceptedConn(t)
		return &noSessionPassthrough{src: src, dst: dst, dest: "127.0.0.1:1", global: global}, src, dst
	}
	closed := func(c ...*closeCountingConn) bool {
		for _, x := range c {
			if x.closes.Load() == 0 {
				return false
			}
		}
		return true
	}

	t.Run("not global", func(t *testing.T) {
		var s noSessionPassthroughs
		pt, src, dst := newPT(false)
		if !s.add(pt) {
			t.Fatal("a passthrough was refused before the session started")
		}
		ended, kept := s.sessionStarted(true)
		if len(ended) != 1 || ended[0] != pt || kept != 0 || !closed(src, dst) {
			t.Fatalf("sessionStarted ended %v and kept %d; want the one passthrough reset", ended, kept)
		}
		late, _, _ := newPT(false)
		if s.add(late) {
			t.Fatal("a passthrough opened after the session started")
		}
	})

	t.Run("global, the session passes through too", func(t *testing.T) {
		var s noSessionPassthroughs
		pt, src, dst := newPT(true)
		s.add(pt)
		if ended, kept := s.sessionStarted(true); len(ended) != 0 || kept != 1 || src.closes.Load() != 0 || dst.closes.Load() != 0 {
			t.Fatalf("sessionStarted ended %v and kept %d; want the global passthrough kept open", ended, kept)
		}
		late, _, _ := newPT(true)
		if !s.add(late) {
			t.Fatal("a global passthrough was refused under a session that passes connections through too")
		}
		// A later session that intercepts TLS resets them both.
		if ended, kept := s.sessionStarted(false); len(ended) != 2 || kept != 0 || !closed(src, dst) {
			t.Fatalf("a session intercepting TLS ended %v and kept %d; want both global passthroughs reset", ended, kept)
		}
	})

	t.Run("global, the session intercepts TLS", func(t *testing.T) {
		var s noSessionPassthroughs
		pt, src, dst := newPT(true)
		s.add(pt)
		if ended, kept := s.sessionStarted(false); len(ended) != 1 || kept != 0 || !closed(src, dst) {
			t.Fatalf("sessionStarted ended %v and kept %d; want the global passthrough reset", ended, kept)
		}
		late, _, _ := newPT(true)
		if s.add(late) {
			t.Fatal("a global passthrough opened under a session that intercepts TLS")
		}
	})
}

// A connection whose destination was still being dialled when the session was
// set up does not become a passthrough the session cannot see: it is reset,
// and both of its connections are closed.
func TestHandleConnectionResetsAPassthroughTheSessionOvertookWhileDialling(t *testing.T) {
	upstream, accepted, upstreamEnded := echoUpstream(t)
	app, src := acceptedConn(t)
	p := &Proxy{logger: zap.NewNop(), DestInfo: fakeDestInfo{dest: ipv4Dest(t, upstream)}, agentMode: models.MODE_RECORD}
	dst := predialedTo(t, p, src, upstream)
	<-accepted
	// The session is set up after handleConnection decided there was none,
	// while it dials: the order a session overtaking the dial leaves.
	p.endNoSessionPassthroughs(&agent.Session{Mode: models.MODE_RECORD})

	if err := p.handleConnection(context.Background(), src); err != nil {
		t.Fatalf("handleConnection: %v", err)
	}
	if src.closes.Load() == 0 || dst.closes.Load() == 0 {
		t.Fatalf("a connection overtaken by the session while dialling was left open (application end closed %d times, destination end %d)",
			src.closes.Load(), dst.closes.Load())
	}
	requireReset(t, app, upstreamEnded, "the session overtook the dial")
}

// Under global passthrough a session passes connections through as well, so a
// passthrough made before it stays open across its start: closing it would
// only cost a reconnect, and fail a call for a pool that does not check its
// connections. A session that intercepts TLS opportunistically does not pass
// connections through as they are, so it resets them.
func TestAGlobalPassthroughOutlivesOnlyASessionThatPassesThroughToo(t *testing.T) {
	open := func(t *testing.T, mode models.Mode) (*Proxy, *net.TCPConn, *closeCountingConn, *closeCountingConn, chan error, <-chan error) {
		upstream, _, upstreamEnded := echoUpstream(t)
		cfg := config.New()
		cfg.Agent.Mode = mode
		cfg.Agent.GlobalPassthrough = true
		p := New(zap.NewNop(), fakeDestInfo{dest: ipv4Dest(t, upstream)}, cfg)
		p.nsswitchData = []byte("preseeded-by-test") // see testProxy
		app, src := acceptedConn(t)
		dst := predialedTo(t, p, src, upstream)
		done := make(chan error, 1)
		go func() { done <- p.handleConnection(context.Background(), src) }()
		if _, err := app.Write([]byte("ping")); err != nil {
			t.Fatal(err)
		}
		select {
		case err := <-done:
			t.Fatalf("handleConnection returned (%v) without passing the connection through", err)
		case <-readN(app, 4):
		}
		return p, app, src, dst, done, upstreamEnded
	}

	t.Run("a replay session that passes through", func(t *testing.T) {
		p, app, src, dst, done, upstreamEnded := open(t, models.MODE_TEST)
		if err := p.Mock(context.Background(), models.OutgoingOptions{Mocking: true}); err != nil {
			t.Fatal(err)
		}
		if src.closes.Load() != 0 || dst.closes.Load() != 0 {
			t.Fatal("Mock closed a connection passed through under global passthrough, which the session passes through too")
		}
		if _, err := app.Write([]byte("pong")); err != nil {
			t.Fatal(err)
		}
		select {
		case err := <-done:
			t.Fatalf("handleConnection returned (%v) after the session started", err)
		case got := <-readN(app, 4):
			if got != "pong" {
				t.Fatalf("the passthrough carried %q after the session started, want %q", got, "pong")
			}
		}
		if err := app.CloseWrite(); err != nil {
			t.Fatal(err)
		}
		if err := awaitEvent(t, done, "handleConnection returning once both sides ended"); err != nil {
			t.Fatalf("handleConnection: %v", err)
		}
		if err := awaitEvent(t, upstreamEnded, "the destination seeing its connection end"); err != nil {
			t.Fatalf("the destination's connection did not end cleanly: %v", err)
		}
	})

	t.Run("a recording session that intercepts TLS", func(t *testing.T) {
		p, app, src, dst, done, upstreamEnded := open(t, models.MODE_RECORD)
		t.Cleanup(func() { syncMock.Get().SetOutputChannel(nil) })
		if err := p.Record(context.Background(), make(chan *models.Mock, 1), models.OutgoingOptions{OpportunisticTLSIntercept: true}); err != nil {
			t.Fatal(err)
		}
		if src.closes.Load() == 0 || dst.closes.Load() == 0 {
			t.Fatal("Record left open a connection passed through under global passthrough, although the session intercepts TLS")
		}
		if err := awaitEvent(t, done, "handleConnection returning after the session started"); err != nil {
			t.Fatalf("handleConnection: %v", err)
		}
		requireReset(t, app, upstreamEnded, "Record returned")
	})
}

// In replay, and whenever the agent's mode is unknown, a connection that
// arrives before the session exists is closed at once and its destination is
// never contacted: a live call there could pass a test on behaviour the
// recording never showed. The first connection to a destination is reported
// at WARN, the rest at Debug.
func TestHandleConnectionWithoutASessionClosesWithoutDialling(t *testing.T) {
	for _, mode := range []models.Mode{models.MODE_TEST, "", models.MODE_OFF} {
		t.Run(string(mode), func(t *testing.T) {
			ln, err := net.Listen("tcp4", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = ln.Close() })
			core, logs := observer.New(zapcore.DebugLevel)
			p := &Proxy{logger: zap.New(core), DestInfo: fakeDestInfo{dest: ipv4Dest(t, ln.Addr().String())}, agentMode: mode}

			for i := 0; i < 2; i++ {
				app, src := acceptedConn(t)
				if err := p.handleConnection(context.Background(), src); err != nil {
					t.Fatalf("handleConnection: %v", err)
				}
				if src.closes.Load() == 0 {
					t.Fatal("handleConnection returned without closing the application's connection")
				}
				if rest, err := io.ReadAll(app); err != nil || len(rest) != 0 {
					t.Fatalf("the application's end read %q, %v; want a clean end of stream", rest, err)
				}
			}
			assertNothingDialled(t, ln)

			warns := logs.FilterLevelExact(zapcore.WarnLevel).FilterField(zap.String("destination", ln.Addr().String())).Len()
			if warns != 1 {
				t.Fatalf("%d WARN lines for two connections to one destination, want 1: %v", warns, logs.All())
			}
		})
	}
}

// assertNothingDialled fails the test if anything connected to ln. The check
// connects once itself and accepts until it sees its own connection: one the
// proxy made before it would come first.
func assertNothingDialled(t *testing.T, ln net.Listener) {
	t.Helper()
	probe, err := net.Dial("tcp4", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = probe.Close() }()
	for {
		c, err := ln.Accept()
		if err != nil {
			t.Fatal(err)
		}
		from := c.RemoteAddr().String()
		_ = c.Close()
		if from == probe.LocalAddr().String() {
			return
		}
		t.Fatalf("the proxy connected to the destination (from %s) although it has no session to serve the call from", from)
	}
}

// The log dedupe is bounded: once full it starts over instead of growing
// with every destination an agent without a session is asked to reach.
func TestNoSessionDestsIsBounded(t *testing.T) {
	var d noSessionDests
	if !d.first("a:1") || d.first("a:1") {
		t.Fatal("a destination must be first exactly once")
	}
	for i := 0; i < noSessionDestsMax*3; i++ {
		d.first(fmt.Sprintf("10.0.0.1:%d", i))
		if len(d.seen) > noSessionDestsMax {
			t.Fatalf("the dedupe grew to %d entries, past its bound of %d", len(d.seen), noSessionDestsMax)
		}
	}
}
