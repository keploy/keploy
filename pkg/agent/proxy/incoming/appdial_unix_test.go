//go:build linux || darwin

package proxy

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/agent/proxy/internal/testnet"
	"go.keploy.io/server/v3/pkg/agent/proxy/util"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// A call whose client goes while it waits for the app to listen ends its
// wait, and nothing is sent on to the app for it once the app listens; a call
// beside it in the same wait, whose client stays, is served. The end is said
// at DEBUG: the client left, the app did not fail.
func TestAWaitEndsForACallWhoseClientWent(t *testing.T) {
	addr := sharedAppAddr(t)
	core, logs := observer.New(zapcore.DebugLevel)
	pm := New(zap.New(core), nil, &config.Config{})
	goes, goesProxied := tcpPairForWatch(t)
	_, staysProxied := tcpPairForWatch(t)
	gone := dialAsync(context.Background(), pm, addr, newReplayConn([]byte("GET / HTTP/1.1\r\n"), goesProxied))
	stays := dialAsync(context.Background(), pm, addr, staysProxied)
	awaitWaiters(t, pm, addr, 2)

	_ = goes.Close()
	if r := result(t, gone); !errors.Is(r.err, errClientLeft) {
		t.Fatalf("a call whose client went in its wait: %v, want errClientLeft", r.err)
	}
	if callers, waits := waitersAt(pm, addr); callers != 1 || waits != 1 {
		t.Fatalf("after one client went: %d calls in %d waits, want the other one in one", callers, waits)
	}
	ln := testnet.ListenShared(t, "tcp4", addr)
	r := result(t, stays)
	if r.err != nil {
		t.Fatalf("the call whose client stayed: %v", r.err)
	}
	_ = r.conn.Close()
	if n := accepted(t, ln); n != 1 {
		t.Fatalf("the app saw %d connections, want only the staying call's", n)
	}
	pm.logAppDialFailure(pm.logger, "Failed to connect to upstream application.", errClientLeft)
	if all := logs.All(); len(all) != 1 || all[0].Level != zapcore.DebugLevel {
		t.Fatalf("a call whose client went was logged as %v, want one DEBUG", all)
	}
}

// A call refused while its client has gone already does not wait at all.
func TestARefusedCallWhoseClientHasGoneDoesNotWait(t *testing.T) {
	addr := sharedAppAddr(t)
	pm := New(zap.NewNop(), nil, &config.Config{})
	client, proxied := tcpPairForWatch(t)
	_ = client.Close()
	for end := time.Now().Add(10 * time.Second); ; time.Sleep(time.Millisecond) {
		if gone, _ := util.PeerGone(proxied); gone {
			break
		}
		if time.Now().After(end) {
			t.Fatal("precondition: the client's close never reached its socket")
		}
	}
	if _, err := pm.dialApp(context.Background(), addr, proxied); !errors.Is(err, errClientLeft) {
		t.Fatalf("a refused call whose client had gone: %v, want errClientLeft", err)
	}
	if _, waits := waitersAt(pm, addr); waits != 0 {
		t.Fatalf("it left %d waits", waits)
	}
}

// A redial (the app's connection ended with no answer, and the request is
// sent again on a new one) is the first dial's: when the app restarts there,
// its refusal in the restart's gap is waited for, not a failure, and the call
// is answered by the restarted app. The redial used to dial once, on a 3 s
// budget, so the gap failed the call.
func TestARedialWaitsForAnAppThatRestarts(t *testing.T) {
	addr := sharedAppAddr(t)
	first := testnet.ListenShared(t, "tcp4", addr)
	restarted := make(chan struct{})
	go func() {
		// The app's first connection reads the request's head; the app
		// then goes (its listener and the connection) and listens again
		// 300 ms later.
		c, err := first.Accept()
		if err != nil {
			return
		}
		br := bufio.NewReader(c)
		for {
			if line, err := br.ReadString('\n'); err != nil || line == "\r\n" {
				break
			}
		}
		_ = first.Close()
		_ = c.Close()
		time.Sleep(300 * time.Millisecond)
		again, err := testnet.OpenShared("tcp4", addr)
		if err != nil {
			return
		}
		close(restarted)
		serveApp(t, again, func(w http.ResponseWriter, _ *http.Request) {
			_, _ = io.WriteString(w, "RESTARTED")
		})
		t.Cleanup(func() { _ = again.Close() })
	}()
	pm, client, served, _ := serveWatched(t, false, first, nil)
	if _, err := client.Write([]byte("GET /jobs HTTP/1.1\r\nHost: api\r\n\r\n")); err != nil {
		t.Fatal(err)
	}
	_ = client.SetReadDeadline(time.Now().Add(30 * time.Second))
	resp, err := http.ReadResponse(bufio.NewReader(client), nil)
	if err != nil {
		t.Fatalf("the call whose app restarted before it answered: %v, want the restarted app's answer", err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK || string(body) != "RESTARTED" {
		t.Fatalf("the call got %s %q, want the restarted app's answer", resp.Status, body)
	}
	select {
	case <-restarted:
	default:
		t.Fatal("precondition: the app did not restart")
	}
	_ = client.Close()
	select {
	case <-served:
	case <-never(t):
		t.Fatal("never: the handler ended")
	}
	pm.captures.Wait() // the handler started its capture before it ended
	if _, waits := waitersAt(pm, addr); waits != 0 {
		t.Fatalf("the redial left %d waits", waits)
	}
}

// atOrAbove is the entries logged at level l or above.
func atOrAbove(logs *observer.ObservedLogs, l zapcore.Level) []observer.LoggedEntry {
	var out []observer.LoggedEntry
	for _, e := range logs.All() {
		if e.Level >= l {
			out = append(out, e)
		}
	}
	return out
}

// A client that leaves while its call waits for the app to listen ends the
// call through each handler (HTTP/1, gRPC's preface, and the relay of a TLS
// connection), nothing is sent on to the app once it listens, and nothing is
// said above DEBUG.
func TestAClientThatLeavesWhileItsCallWaitsEndsTheHandler(t *testing.T) {
	for _, proto := range []string{"HTTP/1", "gRPC", "TLS"} {
		t.Run(proto, func(t *testing.T) {
			addr := sharedAppAddr(t)
			core, logs := observer.New(zapcore.DebugLevel)
			pm := New(zap.New(core), nil, &config.Config{})
			client, proxied := tcpPairForWatch(t)
			payload := "GET / HTTP/1.1\r\nHost: api\r\n\r\n"
			switch proto {
			case "gRPC":
				payload = clientPreface
			case "TLS":
				// A ClientHello's record header and the start of its
				// body: as much as the ingress reads to tell a call's kind.
				payload = "\x16\x03\x01\x00\xc8\x01\x00\x00\xc4\x03\x03" + strings.Repeat("\x00", len(clientPreface))
			}
			if _, err := client.Write([]byte(payload)); err != nil {
				t.Fatal(err)
			}
			done := make(chan struct{})
			go func() {
				defer close(done)
				pm.handleConnection(context.Background(), proxied, addr, pm.logger, make(chan *models.TestCase, 1), make(chan struct{}, 1), 8080)
				pm.captures.Wait()
			}()
			awaitWaiters(t, pm, addr, 1)
			_ = client.Close()
			select {
			case <-done:
			case <-never(t):
				t.Fatal("the handler still waits after its client left")
			}
			ln := testnet.ListenShared(t, "tcp4", addr)
			if n := accepted(t, ln); n != 0 {
				t.Errorf("the app saw %d connections for a client that left", n)
			}
			if e := atOrAbove(logs, zapcore.InfoLevel); len(e) != 0 {
				t.Errorf("logged at INFO or above: %v", e)
			}
		})
	}
}

// ServeLive (a caller that hands the ingress a live connection) passes its
// connection as the call's client: a client that leaves while its call
// waits for the app ends the call, and the app sees nothing of it.
func TestServeLiveEndsForAClientThatLeavesWhileItWaits(t *testing.T) {
	addr := sharedAppAddr(t)
	pm := New(zap.NewNop(), nil, &config.Config{})
	client, proxied := tcpPairForWatch(t)
	done := make(chan struct{})
	go func() {
		defer close(done)
		pm.ServeLive(context.Background(), proxied, addr, 8080)
		pm.captures.Wait()
	}()
	awaitWaiters(t, pm, addr, 1)
	_ = client.Close()
	select {
	case <-done:
	case <-never(t):
		t.Fatal("ServeLive still waits after its client left")
	}
	ln := testnet.ListenShared(t, "tcp4", addr)
	if n := accepted(t, ln); n != 0 {
		t.Errorf("the app saw %d connections for a client that left", n)
	}
}

// oldDialBudget is how long the ingress once let a call wait for the app to
// listen before it failed the call.
const oldDialBudget = 3 * time.Second

// firstDial is one of the handlers that dial the app for a new connection:
// handleConnection's three (an HTTP/1 call, gRPC's preface, the relay of a
// TLS connection) and ServeLive's passthrough, with requests off.
type firstDial struct {
	name string
	// open is what the client sends first: as much as tells the handler the
	// call's kind (the passthrough reads nothing before its dial).
	open string
	// serve has the app serve on ln, which it listens at late.
	serve func(t *testing.T, ln net.Listener)
	// call makes the client's call over conn: nil once the app answered.
	call func(conn net.Conn) error
}

func firstDials(t *testing.T) []firstDial {
	trust := startTLSApp(t) // the late TLS app's certificate is the same
	return []firstDial{
		{
			name: "HTTP/1",
			open: "GET /late HTTP/1.1\r\nHost: api\r\n\r\n",
			serve: func(t *testing.T, ln net.Listener) {
				serveApp(t, ln, func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, "late "+r.URL.Path) })
			},
			call: func(conn net.Conn) error {
				if _, err := io.WriteString(conn, "GET /late HTTP/1.1\r\nHost: api\r\n\r\n"); err != nil {
					return err
				}
				resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
				if err != nil {
					return fmt.Errorf("the app's answer: %w", err)
				}
				body, err := io.ReadAll(resp.Body)
				_ = resp.Body.Close()
				if err != nil || string(body) != "late /late" {
					return fmt.Errorf("the app's answer: %s %q (%v), want %q", resp.Status, body, err, "late /late")
				}
				return nil
			},
		},
		{
			name:  "gRPC",
			open:  clientPreface,
			serve: func(t *testing.T, ln net.Listener) { serveRaw(ln, clientPreface, emptySettingsFrame) },
			call:  func(conn net.Conn) error { return rawCall(conn, clientPreface, emptySettingsFrame) },
		},
		{
			name: "TLS",
			// A ClientHello's record header and the start of its body.
			open:  "\x16\x03\x01\x00\xc8\x01\x00\x00\xc4\x03\x03" + strings.Repeat("\x00", len(clientPreface)),
			serve: func(t *testing.T, ln net.Listener) { startTLSAppOn(t, ln) },
			call: func(conn net.Conn) error {
				return tlsCall(conn, clientTLSConfig(trust), trust.Certificate(), "/late")
			},
		},
		{
			name:  "passthrough",
			serve: func(t *testing.T, ln net.Listener) { serveRaw(ln, "ping", "pong") },
			call:  func(conn net.Conn) error { return rawCall(conn, "ping", "pong") },
		},
	}
}

// emptySettingsFrame is an HTTP/2 SETTINGS frame with no settings: a gRPC
// server's first bytes.
const emptySettingsFrame = "\x00\x00\x00\x04\x00\x00\x00\x00\x00"

// serveRaw is an app that takes one connection on ln, reads want from it,
// answers it with answer and closes it.
func serveRaw(ln net.Listener, want, answer string) {
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer func() { _ = c.Close() }()
		got := make([]byte, len(want))
		if _, err := io.ReadFull(c, got); err == nil && string(got) == want {
			_, _ = io.WriteString(c, answer)
		}
	}()
}

// rawCall sends want over conn and reads the app's answer: nil when it is
// answer.
func rawCall(conn net.Conn, want, answer string) error {
	if _, err := io.WriteString(conn, want); err != nil {
		return err
	}
	got := make([]byte, len(answer))
	if n, err := io.ReadFull(conn, got); err != nil {
		return fmt.Errorf("the app's answer: %q (%v), want %q", got[:n], err, answer)
	}
	if string(got) != answer {
		return fmt.Errorf("the app's answer: %q, want %q", got, answer)
	}
	return nil
}

// serveFirstDial runs d's handler, on ctx, for a new client's connection to
// the app at addr. served is closed once the handler ends and its captures
// with it.
func serveFirstDial(t *testing.T, ctx context.Context, d firstDial, addr string) (pm *IngressProxyManager, client net.Conn, served <-chan struct{}, logs *observer.ObservedLogs) {
	t.Helper()
	core, logs := observer.New(zapcore.DebugLevel)
	cfg := &config.Config{}
	cfg.Agent.RecordRequests = d.name != "passthrough"
	pm = New(zap.New(core), nil, cfg)
	client, proxied := tcpPairForWatch(t)
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer func() { _ = proxied.Close() }() // as ServeLive's caller does
		if d.name == "passthrough" {
			pm.ServeLive(ctx, proxied, addr, 8080)
		} else {
			pm.handleConnection(ctx, proxied, addr, pm.logger, make(chan *models.TestCase, 4), make(chan struct{}, 1), 8080)
		}
		pm.captures.Wait()
	}()
	return pm, client, done, logs
}

// Every first dial to the app waits for an app that listens only after the
// 3 s the ingress once gave it, and the call is answered by the app: its
// listener coming ends the wait, not a time. The app here listens once the
// call has waited past that budget (the wait's own start is the mark); a
// budget put back around any of the dials fails that call.
func TestEveryFirstDialWaitsPastTheOldBudgetForTheApp(t *testing.T) {
	for _, d := range firstDials(t) {
		t.Run(d.name, func(t *testing.T) {
			t.Parallel()
			addr := sharedAppAddr(t)
			pm, client, served, logs := serveFirstDial(t, context.Background(), d, addr)
			called := make(chan error, 1)
			go func() { called <- d.call(client) }()
			awaitWaiters(t, pm, addr, 1)
			time.Sleep(oldDialBudget + time.Second)
			d.serve(t, testnet.ListenShared(t, "tcp4", addr))
			select {
			case err := <-called:
				if err != nil {
					t.Fatalf("the call that waited past %v for the app: %v", oldDialBudget, err)
				}
			case <-never(t):
				t.Fatal("the call that waited for the app did not end")
			}
			_ = client.Close()
			<-served
			if e := atOrAbove(logs, zapcore.ErrorLevel); len(e) != 0 {
				t.Errorf("logged at ERROR: %v", e)
			}
		})
	}
}

// Every first dial ends its wait for the app when the call's context ends
// (keploy's own end: the forwarder's, the agent's), and its handler with it:
// the app, listening after, sees nothing of the call, no wait is left, and
// nothing is said above DEBUG.
func TestEveryFirstDialEndsItsWaitWithTheCallsContext(t *testing.T) {
	for _, d := range firstDials(t) {
		t.Run(d.name, func(t *testing.T) {
			t.Parallel()
			addr := sharedAppAddr(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			pm, client, served, logs := serveFirstDial(t, ctx, d, addr)
			if _, err := io.WriteString(client, d.open); err != nil {
				t.Fatal(err)
			}
			awaitWaiters(t, pm, addr, 1)
			cancel()
			select {
			case <-served:
			case <-never(t):
				t.Fatal("the handler still waits for the app after its context ended")
			}
			if callers, waits := waitersAt(pm, addr); callers != 0 || waits != 0 {
				t.Errorf("after the call's context ended: %d calls in %d waits, want none", callers, waits)
			}
			ln := testnet.ListenShared(t, "tcp4", addr)
			if n := accepted(t, ln); n != 0 {
				t.Errorf("the app saw %d connections for a call whose context ended", n)
			}
			if e := atOrAbove(logs, zapcore.InfoLevel); len(e) != 0 {
				t.Errorf("logged at INFO or above: %v", e)
			}
		})
	}
}

// The last call to leave its wait returns only once the wait's poll dials
// the app no more. The end of the wait's context cannot stop a dial already
// past its dialer's look at the context, and that dial's connect reaches an
// app that listens as the call ends: a connection for a call that has ended.
// The poll's dial is held here in that gap (pollControl) until the call is
// seen waiting for the poll; then the dial goes on, refused, and the app,
// listening once the call has returned, sees nothing. The dial held is the
// poll's first, or a later look's (its third): the call waits for the whole
// poll, not for its first look only.
func TestTheLastCallToLeaveItsWaitReturnsOnlyOnceThePollDialsNoMore(t *testing.T) {
	for _, look := range []struct {
		name string
		dial int32 // the poll's dial held, counted from 1
	}{
		{"the poll's first look", 1},
		{"a later look of the poll's", 3},
	} {
		t.Run(look.name, func(t *testing.T) {
			t.Parallel()
			addr := sharedAppAddr(t)
			pm := New(zap.NewNop(), nil, &config.Config{})
			inDial, release := make(chan struct{}), make(chan struct{})
			var dials atomic.Int32
			pm.pollControl = func(string, string, syscall.RawConn) error {
				if dials.Add(1) == look.dial {
					close(inDial)
					<-release
				}
				return nil
			}
			ctx, cancel := context.WithCancel(context.Background())
			ch := make(chan dialed, 1)
			caller := make(chan string, 1) // the call's goroutine, as its stack names it
			go func() {
				caller <- thisGoroutine()
				c, err := pm.dialApp(ctx, addr, nil)
				ch <- dialed{c, err}
			}()
			g := <-caller
			end := never(t)
			select {
			case <-inDial:
			case <-end:
				t.Fatalf("the wait's poll did not dial the app %d times", look.dial)
			}
			cancel()
			for !receivingIn(g, "(*IngressProxyManager).leaveWait") {
				select {
				case r := <-ch:
					close(release)
					t.Fatalf("the call's dial returned (%v) while its wait's own dial to the app was still to connect: that connect reaches an app that listens now, for a call that has ended", r.err)
				case <-end:
					t.Fatal("the call neither returned nor waited for its wait's poll to stop")
				case <-time.After(time.Millisecond):
				}
			}
			close(release)
			if r := result(t, ch); !errors.Is(r.err, context.Canceled) {
				t.Fatalf("a call whose context ended in its wait returned %v, want the context's end", r.err)
			}
			if n := accepted(t, testnet.ListenShared(t, "tcp4", addr)); n != 0 {
				t.Fatalf("the app, listening once the call had returned, saw %d connections keploy opened for it", n)
			}
		})
	}
}

// thisGoroutine is the calling goroutine's stack header up to its state:
// "goroutine 42 [".
func thisGoroutine() string {
	buf := make([]byte, 64)
	header, _, _ := strings.Cut(string(buf[:runtime.Stack(buf, false)]), "[")
	return header + "["
}

// receivingIn reports whether goroutine g (thisGoroutine's) is blocked
// receiving from a channel in fn itself (its stack's top frame).
func receivingIn(g, fn string) bool {
	buf := make([]byte, 1<<20)
	for {
		n := runtime.Stack(buf, true)
		if n < len(buf) {
			buf = buf[:n]
			break
		}
		buf = make([]byte, 2*len(buf))
	}
	for _, stack := range strings.Split(string(buf), "\n\n") {
		header, frames, _ := strings.Cut(stack, "\n")
		top, _, _ := strings.Cut(frames, "\n")
		if strings.HasPrefix(header, g+"chan receive") && strings.Contains(top, fn+"(") {
			return true
		}
	}
	return false
}

// A redial of a stale pooled connection waits for an app that restarts for
// longer than the 3 s the redial once had, and the call is answered by the
// restarted app.
func TestARedialWaitsForAnAppThatRestartsForLongerThanTheOldBudget(t *testing.T) {
	addr := sharedAppAddr(t)
	first := testnet.ListenShared(t, "tcp4", addr)
	go func() {
		c, err := first.Accept()
		if err != nil {
			return
		}
		br := bufio.NewReader(c)
		for {
			if line, err := br.ReadString('\n'); err != nil || line == "\r\n" {
				break
			}
		}
		_ = first.Close()
		_ = c.Close()
		time.Sleep(3500 * time.Millisecond)
		again, err := testnet.OpenShared("tcp4", addr)
		if err != nil {
			return
		}
		serveApp(t, again, func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "RESTARTED") })
		t.Cleanup(func() { _ = again.Close() })
	}()
	pm, client, served, _ := serveWatched(t, false, first, nil)
	if _, err := client.Write([]byte("GET /jobs HTTP/1.1\r\nHost: api\r\n\r\n")); err != nil {
		t.Fatal(err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(client), nil)
	if err != nil {
		t.Fatalf("the call over a redial to an app restarting for 3.5 s: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if string(body) != "RESTARTED" {
		t.Fatalf("got %s %q, want the restarted app's answer", resp.Status, body)
	}
	_ = client.Close()
	<-served
	pm.captures.Wait() // the handler started its capture before it ended
}

// A connection the poll hands to a call whose context ends at that moment
// goes to no one: leaveWait closes it, so the app sees it end.
func TestAConnectionHandedToACallThatHasEndedIsClosed(t *testing.T) {
	addr := sharedAppAddr(t)
	ln := testnet.ListenShared(t, "tcp4", addr)
	pm := New(zap.NewNop(), nil, &config.Config{})
	conn, err := net.Dial("tcp4", addr)
	if err != nil {
		t.Fatal(err)
	}
	app, err := ln.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = app.Close() }()
	wait := &appWait{addr: addr, callers: map[*appWaiter]struct{}{}}
	wait.ctx, wait.cancel = context.WithCancel(context.Background())
	w := &appWaiter{done: make(chan struct{})}
	wait.callers[w] = struct{}{}
	pm.waitsMu.Lock()
	endWaiterLocked(wait, w, conn, addr, nil)
	pm.waitsMu.Unlock()

	pm.leaveWait(wait, w)
	_ = app.SetReadDeadline(time.Now().Add(30 * time.Second))
	if n, err := app.Read(make([]byte, 1)); n != 0 || !errors.Is(err, io.EOF) {
		t.Fatalf("the app's end of the handed connection read %d, %v; want EOF: it was not closed", n, err)
	}
}

// eofCountingConn counts the reads of its connection's EOF that the preface
// read (util.AppendRequiredBytes) makes, apart from the handler's reads after
// it.
type eofCountingConn struct {
	net.Conn
	prefaceEOFs atomic.Int32
}

func (c *eofCountingConn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	if errors.Is(err, io.EOF) {
		pcs := make([]uintptr, 32)
		frames := runtime.CallersFrames(pcs[:runtime.Callers(2, pcs)])
		for f, more := frames.Next(); ; f, more = frames.Next() {
			if strings.HasSuffix(f.Function, "/pkg/agent/proxy/util.AppendRequiredBytes") {
				c.prefaceEOFs.Add(1)
				break
			}
			if !more {
				break
			}
		}
	}
	return n, err
}

// A port probe (`echo >/dev/tcp/host/port`: one byte, then its close) is
// handed on to the HTTP/1 handler, which dials the app, at the first EOF its
// connection reads. The preface read used to read that EOF five times, 100
// ms apart, so the probe's dial came 400 ms after its close: in CI, after the
// recording had stopped the app.
func TestAPortProbeIsDialedThroughAtItsConnectionsFirstEOF(t *testing.T) {
	app, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = app.Close() }()
	pm := New(zap.NewNop(), nil, &config.Config{})
	client, proxied := tcpPairForWatch(t)
	if _, err := client.Write([]byte("\n")); err != nil {
		t.Fatal(err)
	}
	_ = client.Close()
	counted := &eofCountingConn{Conn: proxied}
	done := make(chan struct{})
	go func() {
		defer close(done)
		pm.handleConnection(context.Background(), counted, app.Addr().String(), pm.logger, make(chan *models.TestCase, 1), make(chan struct{}, 1), 8080)
		pm.captures.Wait()
	}()
	accepted := make(chan net.Conn, 1)
	go func() {
		if c, err := app.Accept(); err == nil {
			accepted <- c
		}
	}()
	select {
	case c := <-accepted:
		_ = c.Close()
	case <-never(t):
		t.Fatal("the app never saw the probe's connection")
	}
	select {
	case <-done:
	case <-never(t):
		t.Fatal("the handler did not end after the probe and the app closed")
	}
	if n := counted.prefaceEOFs.Load(); n != 1 {
		t.Fatalf("the preface read read the probe's EOF %d times, want once: it hands the probe on at the first", n)
	}
}

// A test's call to its app, handed to the live handler (ServeLive) with the
// destination the test dialed, is answered at every address that the proxy's
// ownIP hands it and a connect to which stays on the host: the loopback's, the
// unspecified address (0.0.0.0 or ::, an IPv4-mapped one too: the kernel
// delivers a connect there to the loopback), and an interface's address. It
// is, with requests recorded and without (the relay). A form the app does
// not answer a direct request at here either is skipped: the host, not
// keploy, keeps it from the app.
func TestServeLiveAnswersTheAppAtEveryAddressThatStaysOnTheHost(t *testing.T) {
	ln, err := net.Listen("tcp", "[::]:0") // dual-stack: every form reaches it
	if err != nil {
		if ln, err = net.Listen("tcp4", "0.0.0.0:0"); err != nil {
			t.Fatal(err)
		}
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "pong") })}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	port := ln.Addr().(*net.TCPAddr).Port
	hosts := []string{"127.0.0.1", "::1", "0.0.0.0", "::", "::ffff:0.0.0.0", "::ffff:127.0.0.1"}
	if ip := interfaceIPv4(t); ip != "" {
		hosts = append(hosts, ip)
	}
	direct := &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{DisableKeepAlives: true}}
	for _, recorded := range []bool{false, true} {
		for _, h := range hosts {
			upstream := net.JoinHostPort(h, strconv.Itoa(port))
			t.Run(fmt.Sprintf("recorded=%v/%s", recorded, upstream), func(t *testing.T) {
				if resp, err := direct.Get("http://" + upstream + "/ping"); err != nil {
					t.Skipf("the app does not answer a direct request at %s here either: %v", upstream, err)
				} else {
					_ = resp.Body.Close()
				}
				cfg := &config.Config{}
				cfg.Agent.MockMode = recorded
				cfg.Agent.RecordRequests = recorded
				core, logs := observer.New(zapcore.DebugLevel)
				pm := New(zap.New(core), nil, cfg)
				client, proxied := tcpPairForWatch(t)
				ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
				defer cancel()
				done := make(chan struct{})
				go func() {
					defer close(done)
					pm.ServeLive(ctx, proxied, upstream, uint16(port))
					_ = proxied.Close()
				}()
				if _, err := io.WriteString(client, "GET /ping HTTP/1.1\r\nHost: app\r\nConnection: close\r\n\r\n"); err != nil {
					t.Fatal(err)
				}
				_ = client.SetReadDeadline(time.Now().Add(15 * time.Second))
				resp, err := http.ReadResponse(bufio.NewReader(client), nil)
				if err != nil {
					t.Errorf("ServeLive(%s): no answer from the app a direct request reaches: %v", upstream, err)
				} else {
					b, _ := io.ReadAll(resp.Body)
					_ = resp.Body.Close()
					if string(b) != "pong" {
						t.Errorf("ServeLive(%s) answered %q, want the app's pong", upstream, b)
					}
				}
				_ = client.Close()
				cancel()
				<-done
				pm.captures.Wait()
				if t.Failed() {
					for _, e := range logs.All() {
						t.Logf("logged %s %q %v", e.Level, e.Message, e.ContextMap())
					}
				}
			})
		}
	}
}

// interfaceIPv4 is an IPv4 address of an interface that is up and is not
// the loopback, or "" when there is none.
func interfaceIPv4(t *testing.T) string {
	t.Helper()
	ifs, err := net.Interfaces()
	if err != nil {
		t.Fatal(err)
	}
	for _, ifc := range ifs {
		if ifc.Flags&net.FlagUp == 0 || ifc.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, _ := ifc.Addrs()
		for _, a := range addrs {
			if n, ok := a.(*net.IPNet); ok && n.IP.To4() != nil {
				return n.IP.String()
			}
		}
	}
	return ""
}
