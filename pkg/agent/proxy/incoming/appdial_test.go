package proxy

import (
	"context"
	"errors"
	"net"
	"strconv"
	"testing"
	"time"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/agent/proxy/internal/testnet"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// sharedAppAddr is a loopback address at a port held for the test's life
// (testnet.SharedPort): a dial there is refused until the test listens
// there, as often as it closes its listener and listens again, and no other
// socket takes the port meanwhile.
func sharedAppAddr(t *testing.T) string {
	t.Helper()
	return net.JoinHostPort("127.0.0.1", strconv.Itoa(testnet.SharedPort(t)))
}

// waitersAt is how many calls wait in addr's shared wait, and how many
// shared waits there are.
func waitersAt(pm *IngressProxyManager, addr string) (callers, waits int) {
	pm.waitsMu.Lock()
	defer pm.waitsMu.Unlock()
	if w := pm.waits[addr]; w != nil {
		callers = len(w.callers)
	}
	return callers, len(pm.waits)
}

// awaitWaiters waits, bounded, until n calls wait in addr's shared wait.
func awaitWaiters(t *testing.T, pm *IngressProxyManager, addr string, n int) {
	t.Helper()
	for end := time.Now().Add(30 * time.Second); ; time.Sleep(time.Millisecond) {
		if c, _ := waitersAt(pm, addr); c == n {
			return
		}
		if time.Now().After(end) {
			c, _ := waitersAt(pm, addr)
			t.Fatalf("%d calls wait for %s, want %d", c, addr, n)
		}
	}
}

type dialed struct {
	conn net.Conn
	err  error
}

// dialAsync runs pm.dialApp in a goroutine; its result comes on the channel.
func dialAsync(ctx context.Context, pm *IngressProxyManager, addr string, client net.Conn) <-chan dialed {
	ch := make(chan dialed, 1)
	go func() {
		c, err := pm.dialApp(ctx, addr, client)
		ch <- dialed{c, err}
	}()
	return ch
}

// result is the dial's result, bounded so a dial that never ends fails here.
func result(t *testing.T, ch <-chan dialed) dialed {
	t.Helper()
	select {
	case r := <-ch:
		return r
	case <-time.After(30 * time.Second):
		t.Fatal("the dial did not end")
		return dialed{}
	}
}

// accepted counts the connections waiting in ln's accept queue, closing
// them. A dial that returned has its connection there already.
func accepted(t *testing.T, ln net.Listener) int {
	t.Helper()
	tl := ln.(*net.TCPListener)
	n := 0
	for {
		_ = tl.SetDeadline(time.Now().Add(200 * time.Millisecond))
		c, err := tl.Accept()
		if err != nil {
			return n
		}
		n++
		_ = c.Close()
	}
}

// A dial to an app that has not listened yet waits for its listener, for as
// long as the call's context lasts: the listener appearing answers it, not a
// time. It comes here later than the 3 s the dial once gave up after.
func TestDialAppWaitsForAnAppThatHasNotListenedYet(t *testing.T) {
	addr := sharedAppAddr(t)
	pm := New(zap.NewNop(), nil, &config.Config{})
	ch := dialAsync(context.Background(), pm, addr, nil)
	time.Sleep(3*time.Second + 250*time.Millisecond)
	l := testnet.ListenShared(t, "tcp4", addr)
	r := result(t, ch)
	if r.err != nil {
		t.Fatalf("dialApp returned error: %v", r.err)
	}
	_ = r.conn.Close()
	if n := accepted(t, l); n != 1 {
		t.Fatalf("the app saw %d connections, want the call's one", n)
	}
}

// An app that restarts within a recording (a watcher's rebuild, a
// supervisor, a container restart) refuses calls until it listens again: one
// that comes in the gap waits for it and connects, though the app listened
// at the address before. Only the stop of the recording makes a refusal
// final.
func TestDialAppWaitsForAnAppThatRestarts(t *testing.T) {
	addr := sharedAppAddr(t)
	pm := New(zap.NewNop(), nil, &config.Config{})
	first := testnet.ListenShared(t, "tcp4", addr)
	c, err := pm.dialApp(context.Background(), addr, nil)
	if err != nil {
		t.Fatalf("dialApp to the listening app: %v", err)
	}
	_ = c.Close()
	_ = first.Close()

	ch := dialAsync(context.Background(), pm, addr, nil)
	time.Sleep(300 * time.Millisecond) // the restart's gap
	again := testnet.ListenShared(t, "tcp4", addr)
	r := result(t, ch)
	if r.err != nil {
		t.Fatalf("a call in the app's restart gap: %v, want it connected once the app listened again", r.err)
	}
	_ = r.conn.Close()
	if n := accepted(t, again); n != 1 {
		t.Fatalf("the restarted app saw %d connections, want the call's one", n)
	}
}

// While the recording is stopping, the stop took the app's listener: a
// refusal then is final, and the dial returns it at once, as the refusal it
// is. It used to retry it for 3 s and make its last try past that deadline on
// 1 ms, so a loaded host got "i/o timeout" back, an ERROR in the stop.
func TestDialAppReturnsARefusalAtOnceWhileStopping(t *testing.T) {
	addr := sharedAppAddr(t)
	pm := New(zap.NewNop(), nil, &config.Config{})
	pm.SetStopping(true)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := pm.dialApp(ctx, addr, nil); !isConnRefused(err) {
		t.Fatalf("dialApp to an app the stop took: %v, want its refusal", err)
	}
	if ctx.Err() != nil {
		t.Fatal("dialApp waited on a refusal while stopping until its context ended")
	}
	if _, waits := waitersAt(pm, addr); waits != 0 {
		t.Fatalf("a refusal while stopping left %d waits", waits)
	}
}

// A call that waits for the app ends its wait as the stop begins, with the
// refusal: the stop is what keeps the app from listening.
func TestAWaitForTheAppEndsWithTheRefusalWhenTheStopBegins(t *testing.T) {
	addr := sharedAppAddr(t)
	pm := New(zap.NewNop(), nil, &config.Config{})
	ch := dialAsync(context.Background(), pm, addr, nil)
	awaitWaiters(t, pm, addr, 1)
	pm.SetStopping(true)
	if r := result(t, ch); !isConnRefused(r.err) {
		t.Fatalf("a wait the stop ended returned %v, want the refusal", r.err)
	}
	if _, waits := waitersAt(pm, addr); waits != 0 {
		t.Fatalf("the stop left %d waits", waits)
	}
}

// A context that ends while its call waits for the app (the forwarder or the
// agent stopping: keploy's own end) ends the dial with the context's end,
// wherever it lands: a refused loopback dial takes microseconds, so a cancel
// almost always lands between tries, and the dial once returned the refusal
// then, an ERROR outside a stop. It is said at DEBUG.
func TestAContextThatEndsDuringTheWaitEndsTheDialAsKeploysOwnEnd(t *testing.T) {
	addr := sharedAppAddr(t)
	core, logs := observer.New(zapcore.DebugLevel)
	pm := New(zap.New(core), nil, &config.Config{})
	ctx, cancel := context.WithCancel(context.Background())
	ch := dialAsync(ctx, pm, addr, nil)
	awaitWaiters(t, pm, addr, 1)
	time.Sleep(2*ingressTargetPollInterval + 10*time.Millisecond) // between the wait's tries
	cancel()
	r := result(t, ch)
	if !errors.Is(r.err, context.Canceled) || isConnRefused(r.err) {
		t.Fatalf("a dial whose context ended in its wait returned %v, want the context's end", r.err)
	}
	pm.logAppDialFailure(pm.logger, "Failed to connect to upstream application.", r.err)
	if all := logs.All(); len(all) != 1 || all[0].Level != zapcore.DebugLevel {
		t.Fatalf("a dial keploy ended in its wait was logged as %v, want one DEBUG", all)
	}
	if _, waits := waitersAt(pm, addr); waits != 0 {
		t.Fatalf("a wait whose only call left is still there (%d)", waits)
	}
}

// Every call that finds an address refusing waits in one wait, which dials
// for the listener on behalf of all of them. The connection that finds it is
// a call's: the app sees one connection per call, none that keploy opened
// for itself.
func TestCallsThatFindAnAddressRefusingShareOneWait(t *testing.T) {
	addr := sharedAppAddr(t)
	pm := New(zap.NewNop(), nil, &config.Config{})
	const n = 5
	chs := make([]<-chan dialed, n)
	for i := range chs {
		chs[i] = dialAsync(context.Background(), pm, addr, nil)
	}
	awaitWaiters(t, pm, addr, n)
	if _, waits := waitersAt(pm, addr); waits != 1 {
		t.Fatalf("%d calls to one address wait in %d waits, want one", n, waits)
	}
	l := testnet.ListenShared(t, "tcp4", addr)
	for _, ch := range chs {
		r := result(t, ch)
		if r.err != nil {
			t.Fatalf("a call that waited: %v", r.err)
		}
		defer func() { _ = r.conn.Close() }()
	}
	if got := accepted(t, l); got != n {
		t.Fatalf("the app saw %d connections for %d calls", got, n)
	}
	if _, waits := waitersAt(pm, addr); waits != 0 {
		t.Fatalf("the wait outlived its calls (%d)", waits)
	}
}
