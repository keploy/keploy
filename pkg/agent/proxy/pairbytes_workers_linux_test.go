//go:build linux

package proxy

import (
	"context"
	"errors"
	"io"
	"maps"
	"net"
	"os"
	"slices"
	"testing"

	"go.keploy.io/server/v3/pkg/agent"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/mocknoise"
	"go.keploy.io/server/v3/pkg/models"
)

// TestACutEndOfAWorkersTestMarksOnlyThatWorkersConnections pins which
// connections a test worker's end cut short marks with its test
// (waitPairTargets): only those the kernel stamped with its worker's instance.
// Another worker's bytes, and those of a process in no worker, are no bytes of
// that test's: a loss of them is the test open when it is counted, never the
// ended test's (which would leave the other worker's test passing with its own
// call lost). The end of a test of a run of one test process marks every
// connection it had not seen reached, whatever its stamp.
func TestACutEndOfAWorkersTestMarksOnlyThatWorkersConnections(t *testing.T) {
	for _, tc := range []struct {
		name   string
		worker uint32
		// marked: whether each connection, stamped 1, 2 and 0, is marked.
		marked [3]bool
	}{
		{"worker 1's end", 1, [3]bool{true, false, false}},
		{"worker 2's end", 2, [3]bool{false, true, false}},
		{"a run of one test process's end", 0, [3]bool{true, true, true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, app1, side1, _ := heldPair(t, "tcp4", "127.0.0.1:0")
			app2, side2, _ := heldPairOn(t, p, 2, "tcp4", "127.0.0.1:0")
			app3, side3, _ := heldPairOn(t, p, 3, "tcp4", "127.0.0.1:0")
			sides := []*net.TCPConn{side1, side2, side3}
			p.paired.mu.Lock()
			for i, side := range sides {
				// As accept reads it from the kernel's pairing: the
				// instance the connecting process was in.
				p.paired.byConn[side].instance = []uint32{1, 2, 0}[i]
			}
			p.paired.mu.Unlock()
			for _, app := range []*net.TCPConn{app1, app2, app3} {
				queue(t, app)
			}
			ctx, cancel := context.WithCancel(agent.WithEndOfWorker(context.Background(), "test-1", tc.worker))
			cancel()
			if err := p.WaitPairedBytes(ctx, 3); !errors.Is(err, context.Canceled) {
				t.Fatalf("the end's wait = %v, want it cut short", err)
			}
			for i, side := range sides {
				p.paired.mu.Lock()
				pc := p.paired.byConn[side]
				p.paired.mu.Unlock()
				owners, open := pc.lostBy(1, 1)
				if tc.marked[i] {
					if !slices.Equal(owners, []string{"test-1"}) || open {
						t.Errorf("connection %d's first byte is owned by %q (open test: %v), want test-1's alone", i+1, owners, open)
					}
					continue
				}
				if owners != nil || !open {
					t.Errorf("connection %d's first byte is owned by %q (open test: %v), want the open test's alone", i+1, owners, open)
				}
			}
		})
	}
}

// TestWithEndOfIsTheEndOfARunOfOneTestProcess pins that WithEndOf names no
// worker: its cut wait marks every connection, as before test workers.
func TestWithEndOfIsTheEndOfARunOfOneTestProcess(t *testing.T) {
	ctx := agent.WithEndOf(context.Background(), "test-1")
	if agent.EndOf(ctx) != "test-1" || agent.EndWorkerOf(ctx) != 0 {
		t.Fatalf("WithEndOf = (%q, worker %d), want (test-1, worker 0)", agent.EndOf(ctx), agent.EndWorkerOf(ctx))
	}
	ctx = agent.WithEndOfWorker(context.Background(), "test-2", 7)
	if agent.EndOf(ctx) != "test-2" || agent.EndWorkerOf(ctx) != 7 {
		t.Fatalf("WithEndOfWorker = (%q, worker %d), want (test-2, worker 7)", agent.EndOf(ctx), agent.EndWorkerOf(ctx))
	}
}

// holdParser is a parser that holds each connection it is handed until the
// test releases them all, so what the proxy keeps of it can be read.
type holdParser struct {
	saw     chan struct{}
	release chan struct{}
}

func (holdParser) MatchType(context.Context, []byte) bool { return true }
func (holdParser) RecordOutgoing(context.Context, *integrations.RecordSession) error {
	return nil
}
func (holdParser) MockNoise() mocknoise.Adapter { return nil }
func (h holdParser) MockOutgoing(context.Context, net.Conn, *models.ConditionalDstCfg, integrations.MockMemDb, models.OutgoingOptions) error {
	h.saw <- struct{}{}
	<-h.release
	return nil
}

// Each connection the proxy keeps for the bytes waits (pairedConns) carries
// the keploy instance the kernel stamped the application's connection with,
// read with its pairing as it is accepted: a worker's end cut short reads it
// to mark only that worker's connections. One from no worker carries none.
func TestAPairedConnectionKeepsTheInstanceTheKernelStampedIt(t *testing.T) {
	t.Setenv("KEPLOY_PROVEN", "1")
	self := uint32(os.Getpid())
	byPort := ipv4Dest(t, "127.0.0.1:50052")
	byPort.KernelPid, byPort.Instance = self, 5
	paired := ipv4Dest(t, "127.0.0.1:50051")
	paired.KernelPid = self
	c := &instanceCounter{fakeCounter: fakeCounter{dst: paired}, inst: map[uint64]uint32{1: 7, 2: 9}}
	p, _ := instanceProxy(t, byPort, c)
	h := holdParser{saw: make(chan struct{}, 4), release: make(chan struct{})}
	p.Integrations = map[integrations.IntegrationType]integrations.Integrations{integrations.HTTP: h}
	ctx, cancel := context.WithCancel(context.Background())
	ready, done := make(chan error, 1), make(chan error, 1)
	go func() { done <- p.Serve(ctx, ready) }()
	t.Cleanup(func() { close(h.release); cancel(); <-done })
	if err := <-ready; err != nil {
		t.Fatal(err)
	}
	for range 3 {
		app, err := net.Dial("tcp", p.Listener.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = app.Close() })
		if _, err := io.WriteString(app, "hello\n"); err != nil {
			t.Fatal(err)
		}
		awaitEvent(t, h.saw, "the parser being handed the connection")
	}
	got := map[uint64]uint32{}
	p.paired.mu.Lock()
	for _, pc := range p.paired.byConn {
		got[pc.appSeq] = pc.instance
	}
	p.paired.mu.Unlock()
	if want := map[uint64]uint32{1: 7, 2: 9, 3: 0}; !maps.Equal(got, want) {
		t.Fatalf("the kept connections' instances by application connection = %v, want %v", got, want)
	}
}
