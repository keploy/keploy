//go:build linux

package proxy

import (
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"testing"
	"time"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/agent"
	incoming "go.keploy.io/server/v3/pkg/agent/proxy/incoming"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/neterr"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// recordedPlainDep is a mock whose recorded destination is 127.0.0.1:port, so
// recorded.add indexes that port as a plain (non-child) outside dependency.
func recordedPlainDep(port uint32) *models.Mock {
	return &models.Mock{
		Kind: models.HTTP,
		Spec: models.MockSpec{Metadata: map[string]string{"destAddr": fmt.Sprintf("127.0.0.1:%d", port)}},
	}
}

func selfCallProxy() *Proxy {
	return &Proxy{mockMode: true, appPID: uint32(os.Getpid()), logger: zap.NewNop()}
}

// A port the recording called as an outside dependency stays on the mock path
// even when a process in the run's own tree now listens on it — otherwise the
// test set's recorded mock for that dependency would be skipped for whatever is
// live there. (The hardening guard in serveTreeListener.)
func TestServeTreeListenerKeepsRecordedDepOnMockPath(t *testing.T) {
	recorded.reset()
	defer recorded.reset()

	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	port := uint32(l.Addr().(*net.TCPAddr).Port)

	recorded.add([]*models.Mock{recordedPlainDep(port)})
	if !RecordedPort(port) {
		t.Fatalf("precondition: port %d should be a recorded outside dependency", port)
	}

	p := selfCallProxy()
	dest := &agent.NetworkAddress{Version: 4, Port: port, KernelPid: uint32(os.Getpid())}
	_, srv := net.Pipe()
	defer srv.Close()
	served, _, err := p.serveTreeListener(context.Background(), srv, dest, fmt.Sprintf("127.0.0.1:%d", port), "1")
	if served || err != nil {
		t.Fatalf("a recorded outside dependency must stay on the mock path (served=false), got served=%v err=%v", served, err)
	}
}

// A self-call — a listener owned by the run's own process tree that the
// recording did not call as a dependency — is passed through to the real
// handler, so a broken self-handler fails the run instead of being served a
// stale mock.
func TestServeTreeListenerPassesSelfCallToRealHandler(t *testing.T) {
	recorded.reset()
	defer recorded.reset()

	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	port := uint32(l.Addr().(*net.TCPAddr).Port)

	const live = "LIVE-HANDLER-RESPONSE"
	go func() {
		c, err := l.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		_, _ = c.Write([]byte(live))
	}()

	p := selfCallProxy()
	dest := &agent.NetworkAddress{Version: 4, Port: port, KernelPid: uint32(os.Getpid())}
	cli, srv := net.Pipe()
	defer cli.Close()

	servedCh := make(chan bool, 1)
	go func() {
		served, _, _ := p.serveTreeListener(context.Background(), srv, dest, fmt.Sprintf("127.0.0.1:%d", port), "1")
		servedCh <- served
	}()

	// The real handler's response must reach the caller, relayed over srcConn —
	// proving the live handler answered, not a mock.
	buf := make([]byte, len(live))
	_ = cli.SetReadDeadline(time.Now().Add(3 * time.Second))
	if _, err := io.ReadFull(cli, buf); err != nil || string(buf) != live {
		t.Fatalf("the real handler's response must reach the caller: got %q (%v), want %q", string(buf), err, live)
	}
	_ = cli.Close() // let the relay's other direction finish so serveTreeListener returns

	select {
	case served := <-servedCh:
		if !served {
			t.Fatal("a self-call must be served by passing it through to the real handler")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("serveTreeListener did not return after the relay finished")
	}
}

// A loopback port with nothing listening that the recording did not call as a
// dependency is closed (the real connect would have been refused), not served a
// stale mock.
func TestServeTreeListenerClosesDownUnrecordedLocalPort(t *testing.T) {
	recorded.reset()
	defer recorded.reset()

	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := uint32(l.Addr().(*net.TCPAddr).Port)
	_ = l.Close() // nothing listens there now

	p := selfCallProxy()
	dest := &agent.NetworkAddress{Version: 4, Port: port, KernelPid: uint32(os.Getpid())}
	_, srv := net.Pipe()
	defer srv.Close()
	served, _, err := p.serveTreeListener(context.Background(), srv, dest, fmt.Sprintf("127.0.0.1:%d", port), "1")
	if !served || err != nil {
		t.Fatalf("a down, unrecorded local port is closed (served=true, nil err), got served=%v err=%v", served, err)
	}
}

// A runner that calls its own in-process server makes a self-call per
// connection — supertest's request(app) starts a server on a new port for
// each request. The run's first is logged at INFO, the rest at Debug, and
// their count when the run ends (SetGracefulShutdown, which `keploy mock`
// calls once its run is over and which may be called again: the count is
// logged again only if it moved). A run with one self-call has no count: its
// INFO line said it all.
func TestServeTreeListenerSaysTheFirstAndCountsTheRest(t *testing.T) {
	recorded.reset()
	defer recorded.reset()

	core, logs := observer.New(zap.DebugLevel)
	p := selfCallProxy()
	p.logger = zap.New(core)
	selfCall := func() {
		// A new server on a new port, as supertest has it.
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		defer l.Close()
		go func() {
			if c, err := l.Accept(); err == nil {
				_ = c.Close()
			}
		}()
		port := uint32(l.Addr().(*net.TCPAddr).Port)
		dest := &agent.NetworkAddress{Version: 4, Port: port, KernelPid: uint32(os.Getpid())}
		cli, srv := net.Pipe()
		defer srv.Close()
		_ = cli.Close()
		done := make(chan error, 1)
		go func() {
			served, _, err := p.serveTreeListener(context.Background(), srv, dest, fmt.Sprintf("127.0.0.1:%d", port), "1")
			if err == nil && !served {
				err = fmt.Errorf("not passed through")
			}
			done <- err
		}()
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("a self-call must be passed through: %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("serveTreeListener did not return")
		}
	}
	selfCall()
	_ = p.SetGracefulShutdown(context.Background())
	if got := logs.FilterMessageSnippet("self-calls passed through").All(); len(got) != 0 {
		t.Fatalf("a run with one self-call logged a count: %+v", got)
	}
	const calls = 20
	for range calls - 1 {
		selfCall()
	}

	said := func(level zapcore.Level) int {
		n := 0
		for _, e := range logs.FilterMessageSnippet("self-call passed through").All() {
			if e.Level == level {
				n++
			}
		}
		return n
	}
	if got := said(zapcore.InfoLevel); got != 1 {
		t.Fatalf("%d self-calls said so at INFO %d times, want once", calls, got)
	}
	if got := said(zapcore.DebugLevel); got != calls-1 {
		t.Fatalf("the other self-calls were logged at Debug %d times, want %d", got, calls-1)
	}

	counts := func() []observer.LoggedEntry {
		return logs.FilterMessageSnippet("self-calls passed through").All()
	}
	_ = p.SetGracefulShutdown(context.Background())
	_ = p.SetGracefulShutdown(context.Background())
	if got := counts(); len(got) != 1 || got[0].Level != zapcore.InfoLevel ||
		got[0].ContextMap()["calls"] != int64(calls) || got[0].ContextMap()["processes"] != int64(1) {
		t.Fatalf("want one INFO count of %d self-calls from 1 process when the run ends, said once however often it is asked, got %+v", calls, got)
	}
	selfCall()
	_ = p.SetGracefulShutdown(context.Background())
	if got := counts(); len(got) != 2 || got[1].ContextMap()["calls"] != int64(calls+1) {
		t.Fatalf("a count that moved is said again: want a second line with %d, got %+v", calls+1, got)
	}
}

// Under the proven switch, serveTreeListener tells OnProcess the owner of a
// listener of the run's own tree a connection is addressed to.
func TestServeTreeListenerTellsTheBuildTheOwner(t *testing.T) {
	recorded.reset()
	defer recorded.reset()
	prev := OnProcess
	t.Cleanup(func() { OnProcess = prev })
	t.Setenv("KEPLOY_PROVEN", "1")
	seen := make(chan uint32, 4)
	OnProcess = func(pid uint32) { seen <- pid }

	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	port := uint32(l.Addr().(*net.TCPAddr).Port)
	go func() {
		c, err := l.Accept()
		if err != nil {
			return
		}
		_ = c.Close()
	}()

	p := selfCallProxy()
	dest := &agent.NetworkAddress{Version: 4, Port: port, KernelPid: uint32(os.Getpid())}
	cli, srv := net.Pipe()
	defer cli.Close()
	done := make(chan struct{})
	go func() {
		_, _, _ = p.serveTreeListener(context.Background(), srv, dest, fmt.Sprintf("127.0.0.1:%d", port), "1")
		close(done)
	}()
	if pid := awaitEvent(t, seen, "OnProcess being told the listener's owner"); pid != uint32(os.Getpid()) {
		t.Fatalf("OnProcess was told %d, want the listener's owner %d", pid, os.Getpid())
	}
	_ = cli.Close()
	<-done
}

// A destination that a listener of the run's own tree serves is relayed by
// serveTreeListener, and when it resets, the app reads a reset, as it would
// with no proxy in between: the relay passes the reset on beneath the
// wrappers of the app's connection (their SetLinger), and the caller's close
// of it sends the reset. Without that the close sends a FIN, and the app
// takes a reset upstream for a clean end. Under the proven switch, with the
// topology sink agent.New always installs, a call from another process of
// the tree is wrapped in a liveReader first. Five calls: a mock.real
// destination's from each caller, and an unlisted one's from the two
// callers that are another process:
//   - from the listener's own process: treeRealConn alone (mock.real);
//   - from another process of the tree (a child here): the tree relay, a
//     liveReader, and on a mock.real destination treeRealConn over it; the
//     usual such listener is a database the test command started;
//   - from a process the listener's owner descends from (this process's
//     parent here, as a runner calls the app it started): the live
//     handler's raw relay, incoming.ServeLive, over the same wrappers.
func TestATreeDestinationThatResetsResetsTheApp(t *testing.T) {
	for _, tc := range []struct {
		name   string
		listed bool
		caller string // "own", "child" or "parent"
	}{
		{"a mock.real destination, called from the listener's own process", true, "own"},
		{"a mock.real destination, called from another process of the tree", true, "child"},
		{"an unlisted destination, called from another process of the tree", false, "child"},
		{"a mock.real destination, called by the live handler's raw relay", true, "parent"},
		{"an unlisted destination, called by the live handler's raw relay", false, "parent"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			recorded.reset()
			defer recorded.reset()

			l, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer l.Close()
			dst := l.Addr().String()
			port := uint32(l.Addr().(*net.TCPAddr).Port)
			// The destination reads the request, then resets.
			go func() {
				c, err := l.Accept()
				if err != nil {
					return
				}
				_, _ = io.ReadFull(c, make([]byte, len("hello")))
				_ = c.(*net.TCPConn).SetLinger(0)
				_ = c.Close()
			}()

			var listed []string
			if tc.listed {
				listed = []string{dst}
			}
			p := realProxy(t, listed, nil) // the proven switch
			p.mockMode, p.appPID = true, uint32(os.Getpid())
			// The open test's mock.real uses: only treeRealConn's reads add
			// the caller's connection to one, so they say it wrapped it.
			p.realTracker.BeginTest()
			sink := &rowSink{}
			liveCalled := make(chan struct{}, 1)
			caller := os.Getpid()
			switch tc.caller {
			case "child":
				p.SetTopologySink(sink)
				caller = hold(t).Process.Pid
			case "parent":
				p.SetTopologySink(sink)
				caller = os.Getppid()
				ip := incoming.New(zap.NewNop(), nil, &config.Config{}) // RecordRequests off: the raw relay
				p.live = func(ctx context.Context, conn net.Conn, upstream string, port uint16) {
					liveCalled <- struct{}{}
					ip.ServeLive(ctx, conn, upstream, port)
				}
			}
			app, accepted := realConnPair(t)
			dest := &agent.NetworkAddress{Version: 4, Port: port, KernelPid: uint32(caller)}
			servedCh := make(chan bool, 1)
			go func() {
				served, _, _ := p.serveTreeListener(context.Background(), accepted, dest, dst, "1")
				_ = accepted.Close() // the caller closes the real connection once it returns
				servedCh <- served
			}()

			if _, err := app.Write([]byte("hello")); err != nil {
				t.Fatalf("write: %v", err)
			}
			if !<-servedCh {
				t.Fatal("precondition: the connection was not served on the tree path")
			}
			select {
			case <-liveCalled:
				if tc.caller != "parent" {
					t.Fatal("precondition: the live handler served a call it was not to")
				}
			default:
				if tc.caller == "parent" {
					t.Fatal("precondition: the live handler did not serve the call")
				}
			}
			uses, _ := p.realTracker.EndTest()
			if wrapped := len(uses) == 1 && uses[0].Conns == 1; wrapped != tc.listed {
				t.Fatalf("precondition: treeRealConn is to wrap the caller's connection on a mock.real "+
					"destination only (mock.real %v, wrapped %v, uses %+v)", tc.listed, wrapped, uses)
			}
			run := p.RealRun()
			if tc.listed && tc.caller != "parent" && (len(run.Reached) != 1 || run.Reached[0] != dst) {
				t.Fatalf("precondition: the relay did not count %s reached as mock.real (reached %v)", dst, run.Reached)
			}
			if !tc.listed && len(run.Reached) != 0 {
				t.Fatalf("precondition: an unlisted destination was counted as mock.real (reached %v)", run.Reached)
			}
			if live, _ := sink.rows(); tc.caller != "own" && len(live) == 0 {
				t.Fatal("precondition: the call was not counted live, so no liveReader wrapped it")
			}
			if _, err := app.Read(make([]byte, 1)); !neterr.IsConnReset(err) {
				t.Fatalf("the app read %v; want a reset. Its destination reset, and a FIN "+
					"tells the app the destination ended cleanly", err)
			}
		})
	}
}
