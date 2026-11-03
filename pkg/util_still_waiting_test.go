package pkg

import (
	"context"
	"net"
	"net/http"
	"os"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.keploy.io/server/v3/utils"
	"go.keploy.io/server/v3/utils/stall"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// A wait that only the application's exit or keploy's stop ends is never
// silent: it says, on a timer, that it still waits, on which address and for
// how long so far, and it stops saying so once the address accepts.
func TestAWaitForAcceptSaysItStillWaitsNamingTheAddressAndTheTimeSoFar(t *testing.T) {
	every := stillWaitingEvery
	stillWaitingEvery = 50 * time.Millisecond
	t.Cleanup(func() { stillWaitingEvery = every })

	addr := refusingAddr(t)
	core, logs := observer.New(zapcore.InfoLevel)
	done := make(chan error, 1)
	go func() { done <- WaitForAccept(context.Background(), zap.New(core), addr, nil) }()

	still := func() []observer.LoggedEntry {
		return logs.FilterMessageSnippet("still waiting for the application to accept a connection on " + addr).All()
	}
	deadline := time.Now().Add(10 * time.Second)
	for len(still()) < 2 {
		if time.Now().After(deadline) {
			t.Fatalf("said %d times in 10 s that the wait on %s goes on, every %s; want it said on the timer", len(still()), addr, stillWaitingEvery)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if line := still()[0].Message; !strings.Contains(line, "so far") {
		t.Errorf("the line does not say how long the wait has lasted: %q", line)
	}

	serve(t, addr)
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("the wait ended with %v once %s accepted, want nil", err, addr)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the wait did not end once its address accepted")
	}
	n := len(still())
	time.Sleep(10 * stillWaitingEvery)
	if after := len(still()); after != n {
		t.Errorf("said %d more times that it waits after the address accepted", after-n)
	}
}

// When the application keploy started listens, but not on the port the wait
// is on, the line names the ports it listens on: a wrong port is plain.
func TestTheStillWaitingLineNamesThePortsTheAppListensOnInstead(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the ports a process listens on are read from Linux's /proc")
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	other := strconv.Itoa(ln.Addr().(*net.TCPAddr).Port)
	ctx := utils.WithAppPID(context.Background())
	utils.NoteAppPID(ctx, os.Getpid()) // the "application" is this test's process
	addr := refusingAddr(t)
	_, port, _ := net.SplitHostPort(addr)

	line := stillWaitingLine(ctx, "accept a connection", addr, 90*time.Second)
	for _, want := range []string{"on " + addr, "1m30s so far", "listens on ", other, "not on " + port} {
		if !strings.Contains(line, want) {
			t.Errorf("the line lacks %q: %q", want, line)
		}
	}
	if got := otherListenPorts(ctx, net.JoinHostPort("127.0.0.1", other)); got != "" {
		t.Errorf("named other ports while the application listens on the waited one: %q", got)
	}
	if got := otherListenPorts(ctx, "no port"); got != "" {
		t.Errorf("named other ports for an address with no port: %q", got)
	}
	if got := otherListenPorts(context.Background(), addr); got != "" {
		t.Errorf("named ports with no application pid known: %q", got)
	}
	if got := stillWaitingLine(context.Background(), "accept a connection", addr, time.Second); strings.Contains(got, "listens on") {
		t.Errorf("named ports with no application pid known: %q", got)
	}
}

// Under the proven switch, the answer stall rule ends an exchange keploy
// waits on the application for a grace with no progress. A refused request
// waiting for its address to accept is keploy's wait, ended by an event, not
// the application's stall: it is not cut, however long, and the stall starts
// again once the wait ends.
func TestARefusedRequestsWaitIsNotTheAppsStallUnderTheProvenSwitch(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	clk := &stall.ManualClock{}
	w := newAnswerWatch(SimulationConfig{Unbounded: true, StallGrace: 10 * time.Second, StallClock: clk})
	defer w.stop()
	defer w.waiting()()
	addr := refusingAddr(t)
	ctx := w.bind(EndsWithApp(context.Background()))
	done := sendInBackground(ctx, zap.NewNop(), addr)

	held := func() bool {
		w.mu.Lock()
		defer w.mu.Unlock()
		return w.held > 0
	}
	deadline := time.Now().Add(10 * time.Second)
	for !held() {
		if time.Now().After(deadline) {
			t.Fatal("the refused request did not begin its wait for the address")
		}
		time.Sleep(time.Millisecond)
	}
	for i := 0; i < 5; i++ {
		waitArmed(t, clk, w)
		clk.Advance(9 * time.Second)
	}
	waitArmed(t, clk, w)
	if w.didStall() {
		t.Fatal("the stall rule cut a request that waited for its address to accept")
	}

	serve(t, addr)
	select {
	case r := <-done:
		if r.err != nil || r.resp.StatusCode != http.StatusOK {
			t.Fatalf("want the application's 200 once it accepted, got (%v, %v)", r.resp, r.err)
		}
		_ = r.resp.Body.Close()
	case <-time.After(10 * time.Second):
		t.Fatal("the request was not sent again once its address accepted")
	}
	waitArmed(t, clk, w)
	clk.Advance(time.Second)
	waitArmed(t, clk, w)
	if w.didStall() {
		t.Fatal("the wait's time was counted toward the stall once it ended")
	}
	// The rule is back once the wait ends: a grace with no progress after
	// it cuts the exchange.
	clk.Advance(10 * time.Second)
	deadline = time.Now().Add(10 * time.Second)
	for !w.didStall() {
		if time.Now().After(deadline) {
			t.Fatal("the stall rule did not come back once the wait ended")
		}
		time.Sleep(time.Millisecond)
	}
}

// A wait on a context that carries no answer watch (no stall rule is on)
// holds nothing and releases nothing: holding the stall there is a no-op,
// not a crash.
func TestHoldingTheStallWithoutAWatchIsANoOp(t *testing.T) {
	defer func() {
		if p := recover(); p != nil {
			t.Fatalf("holding the stall on a context with no answer watch panicked: %v", p)
		}
	}()
	release := holdAnswerStall(context.Background())
	release()
	release()
}
