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

	"go.keploy.io/server/v3/pkg/agent"
	"go.keploy.io/server/v3/pkg/models"
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
	served, err := p.serveTreeListener(context.Background(), srv, dest, fmt.Sprintf("127.0.0.1:%d", port), "1")
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
		served, _ := p.serveTreeListener(context.Background(), srv, dest, fmt.Sprintf("127.0.0.1:%d", port), "1")
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
	served, err := p.serveTreeListener(context.Background(), srv, dest, fmt.Sprintf("127.0.0.1:%d", port), "1")
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
			served, err := p.serveTreeListener(context.Background(), srv, dest, fmt.Sprintf("127.0.0.1:%d", port), "1")
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
