//go:build linux

package proxy

import (
	"context"
	"io"
	"net"
	"os"
	"syscall"
	"testing"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/agent"
	"go.keploy.io/server/v3/pkg/agent/containerkey"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/agent/realdeps"
	"go.keploy.io/server/v3/pkg/agent/starts"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// instanceParser is a parser the test reaches through handleConnection, as
// production reaches one: for each connection it is handed, it says the
// keploy instance a mock recorded on that connection would carry
// (starts.StampInstance, by the connection's ID).
type instanceParser struct {
	saw chan uint32
}

func (instanceParser) MatchType(context.Context, []byte) bool { return true }
func (instanceParser) RecordOutgoing(context.Context, *integrations.RecordSession) error {
	return nil
}
func (w instanceParser) MockOutgoing(ctx context.Context, _ net.Conn, _ *models.ConditionalDstCfg, _ integrations.MockMemDb, _ models.OutgoingOptions) error {
	id, _ := ctx.Value(models.ClientConnectionIDKey).(string)
	m := &models.Mock{ConnectionID: id}
	starts.Default.StampInstance(m)
	w.saw <- m.Instance
	return nil
}

// instanceCounter is fakeCounter whose kernel stamped the application's
// n-th connection with the keploy instance inst[n] (none where it has none)
// beside its destination.
type instanceCounter struct {
	fakeCounter
	inst map[uint64]uint32
}

func (f *instanceCounter) ProxyConnPair(rc syscall.RawConn) (agent.ProxyConnPairing, error) {
	p, err := f.fakeCounter.ProxyConnPair(rc)
	if err == nil && p.Dst != nil {
		d := *p.Dst
		d.Instance = f.inst[p.AppSeq]
		p.Dst = &d
	}
	return p, err
}

// instanceProxy is a proven proxy in test mode whose one parser is w, with
// its destinations by source port from byPort and, when c is set, its
// listener counted by c.
func instanceProxy(t *testing.T, byPort *agent.NetworkAddress, c agent.ListenerCounter) (*Proxy, instanceParser) {
	t.Helper()
	saved := containerkey.Default
	containerkey.Default = containerkey.NewKeyer(containerAt{addr: "none"})
	t.Cleanup(func() { containerkey.Default = saved })
	cfg := config.New()
	cfg.ProxyPort = 0
	cfg.Agent.Mode = models.MODE_TEST
	p := New(zap.NewNop(), fakeDestInfo{dest: byPort}, cfg)
	w := instanceParser{saw: make(chan uint32, 4)}
	p.Integrations = map[integrations.IntegrationType]integrations.Integrations{integrations.HTTP: w}
	p.integrationsPriority = []ParserPriority{{Priority: 1, ParserType: integrations.HTTP}}
	if c != nil {
		p.SetListenerCounter(c)
	}
	_ = p.Mock(context.Background(), models.OutgoingOptions{DisableMysqlAutoDetect: true, Mocking: true})
	return p, w
}

// Under the proven switch, each connection's mocks are charged to the keploy
// instance (a test worker's) the kernel stamped that connection with, read
// from its own socket beside its destination (ProxyConnPairing) and handed
// through the accept loop to its own handler: three connections in turn,
// from two workers and from none, each carry their own, never the by-port
// entry's. A proxy whose listener the kernel does not count takes the
// instance with the destination its redirect entry holds.
func TestEachConnectionsMocksCarryTheWorkerInstanceTheKernelStampedItWith(t *testing.T) {
	t.Setenv("KEPLOY_PROVEN", "1")
	self := uint32(os.Getpid())
	byPort := ipv4Dest(t, "127.0.0.1:50052")
	byPort.KernelPid, byPort.Instance = self, 5
	paired := ipv4Dest(t, "127.0.0.1:50051")
	paired.KernelPid = self
	c := &instanceCounter{fakeCounter: fakeCounter{dst: paired}, inst: map[uint64]uint32{1: 7, 2: 9}}
	p, w := instanceProxy(t, byPort, c)
	ctx, cancel := context.WithCancel(context.Background())
	ready, done := make(chan error, 1), make(chan error, 1)
	go func() { done <- p.Serve(ctx, ready) }()
	t.Cleanup(func() { cancel(); <-done })
	if err := <-ready; err != nil {
		t.Fatal(err)
	}
	for i, want := range []uint32{7, 9, 0} {
		app, err := net.Dial("tcp", p.Listener.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		if _, err := io.WriteString(app, "hello\n"); err != nil {
			t.Fatal(err)
		}
		got := awaitEvent(t, w.saw, "the parser being handed the connection")
		_ = app.Close()
		if got != want {
			t.Fatalf("connection %d of 3: its mocks carry instance %d, want %d, the one the kernel stamped it with (5 is the by-port entry's)", i+1, got, want)
		}
	}

	// Not counted: the destination, and its instance, come from the
	// connection's redirect entry.
	p, w = instanceProxy(t, byPort, nil)
	app, src := acceptedConn(t)
	tc := p.inflight.track(src)
	hdone := make(chan error, 1)
	go func() { hdone <- p.handleConnection(context.Background(), tc) }()
	if _, err := io.WriteString(app, "hello\n"); err != nil {
		t.Fatal(err)
	}
	if got := awaitEvent(t, w.saw, "the parser being handed the connection"); got != 5 {
		t.Fatalf("a connection routed by its redirect entry carries instance %d, want that entry's 5", got)
	}
	// The connection kept for test windows is that worker's too: only its
	// windows cut it and wait for it (inflight.of).
	if got := tc.worker.Load(); got != 5+1 {
		t.Fatalf("the kept connection is noted as worker %d's (stored %d), want worker 5's", int64(got)-1, got)
	}
	if err := awaitEvent(t, hdone, "handleConnection returning"); err != nil {
		t.Fatalf("handleConnection: %v", err)
	}
}

// A connection handleConnection relays to a mock.real destination carries the
// keploy instance the kernel stamped it with to the REAL bookkeeping: with
// two test workers' tests open, its send is in its worker's REAL row, and
// nothing of it is the other worker's, or unattributed.
func TestARelayedRealConnectionIsTheWorkerInstanceTheKernelStampedItWith(t *testing.T) {
	t.Setenv("KEPLOY_PROVEN", "1")
	dst := realServer(t)
	dest := ipv4Dest(t, dst)
	dest.KernelPid, dest.Instance = uint32(os.Getpid()), 4
	p, _ := instanceProxy(t, dest, nil)
	list, err := realdeps.Parse([]string{dst})
	if err != nil {
		t.Fatal(err)
	}
	p.real.Store(&realState{list: list, book: &realdeps.Book{}})
	twoWorkers(p)
	app, src := acceptedConn(t)
	hdone := make(chan error, 1)
	go func() { hdone <- p.handleConnection(context.Background(), p.inflight.track(src)) }()
	if got := sendAndRead(t, app, "hello"); got != "real:hello" {
		t.Fatalf("the app read %q, want the real destination's answer", got)
	}
	if err := awaitEvent(t, hdone, "handleConnection returning"); err != nil {
		t.Fatalf("handleConnection: %v", err)
	}
	if got := endWorkers(t, p).Uses; len(got) != 1 || got[0] != (models.RealUse{Dest: dst, Conns: 1}) {
		t.Fatalf("worker 4's REAL row: %+v, want its one relayed connection to %s", got, dst)
	}
}
