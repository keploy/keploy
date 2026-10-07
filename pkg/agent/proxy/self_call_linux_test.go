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
