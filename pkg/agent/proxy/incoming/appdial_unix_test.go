//go:build linux || darwin

package proxy

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
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
// call through each handler (HTTP/1 and gRPC's preface), nothing is sent on
// to the app once it listens, and nothing is said above DEBUG.
func TestAClientThatLeavesWhileItsCallWaitsEndsTheHandler(t *testing.T) {
	for _, proto := range []string{"HTTP/1", "gRPC"} {
		t.Run(proto, func(t *testing.T) {
			addr := sharedAppAddr(t)
			core, logs := observer.New(zapcore.DebugLevel)
			pm := New(zap.New(core), nil, &config.Config{})
			client, proxied := tcpPairForWatch(t)
			payload := "GET / HTTP/1.1\r\nHost: api\r\n\r\n"
			if proto == "gRPC" {
				payload = clientPreface
			}
			if _, err := client.Write([]byte(payload)); err != nil {
				t.Fatal(err)
			}
			done := make(chan struct{})
			go func() {
				defer close(done)
				pm.handleConnection(context.Background(), proxied, addr, pm.logger, make(chan *models.TestCase, 1), make(chan struct{}, 1), 8080)
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
	_, client, served, _ := serveWatched(t, false, first, nil)
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
	wait := &appWait{addr: addr, network: "tcp4", callers: map[*appWaiter]struct{}{}}
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
