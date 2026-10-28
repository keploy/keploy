package proxy

import (
	"context"
	"io"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/agent/containerkey"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/matcher/learn"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// zoneParser is a parser the test reaches through handleConnection, as
// production reaches one: it reports the zone the learn step reads the
// connection's calls in (learn.Tracker.DriftsOf).
type zoneParser struct {
	p   *Proxy
	saw chan *time.Location
}

func (zoneParser) MatchType(context.Context, []byte) bool { return true }
func (zoneParser) RecordOutgoing(context.Context, *integrations.RecordSession) error {
	return nil
}
func (z zoneParser) MockOutgoing(ctx context.Context, _ net.Conn, _ *models.ConditionalDstCfg, _ integrations.MockMemDb, _ models.OutgoingOptions) error {
	z.saw <- z.p.learnTracker.DriftsOf(ctx).Zone()
	return nil
}

// zoneThrough hands a kept connection of the process pid to handleConnection,
// on a proxy that reads processes' zones under proc, and returns the zone
// the parser's learn step reads its calls in.
func zoneThrough(t *testing.T, proc string, pid uint32) *time.Location {
	t.Helper()
	saved := containerkey.Default
	containerkey.Default = containerkey.NewKeyer(containerAt{addr: "none"})
	t.Cleanup(func() { containerkey.Default = saved })
	cfg := config.New()
	cfg.Agent.Mode = models.MODE_TEST
	dest := ipv4Dest(t, "127.0.0.1:50051")
	dest.KernelPid = pid
	p := New(zap.NewNop(), fakeDestInfo{dest: dest}, cfg)
	p.procZones = learn.NewProcZones(proc)
	w := zoneParser{p: p, saw: make(chan *time.Location, 1)}
	p.Integrations = map[integrations.IntegrationType]integrations.Integrations{integrations.HTTP: w}
	p.integrationsPriority = []ParserPriority{{Priority: 1, ParserType: integrations.HTTP}}
	_ = p.Mock(context.Background(), models.OutgoingOptions{DisableMysqlAutoDetect: true, Mocking: true})
	app, src := acceptedConn(t)
	tc := p.inflight.track(src)
	done := make(chan error, 1)
	go func() { done <- p.handleConnection(context.Background(), tc) }()
	if _, err := io.WriteString(app, "hello\n"); err != nil {
		t.Fatal(err)
	}
	z := awaitEvent(t, w.saw, "the parser being handed the connection")
	if err := awaitEvent(t, done, "handleConnection returning"); err != nil {
		t.Fatalf("handleConnection: %v", err)
	}
	return z
}

// Under the proven switch, the zone of the process that made a connection,
// read by the connection's kernel PID from the process's own environment
// and files, reaches the parser's learn step through handleConnection
// (learn.WithProcZone): an application started with TZ=Asia/Kolkata beside
// an agent in UTC has its calls learned in IST, never the agent's zone. A
// connection of a process keploy cannot read is learned in the agent's own.
func TestAConnectionsCallsAreLearnedInTheZoneOfTheProcessThatMadeIt(t *testing.T) {
	kolkata, err := os.ReadFile("/usr/share/zoneinfo/Asia/Kolkata")
	if err != nil {
		t.Skipf("no host zone files: %v", err)
	}
	t.Setenv("KEPLOY_PROVEN", "1")
	t.Setenv("TZ", "UTC")
	proc := t.TempDir()
	write := func(p string, data []byte) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// keploy in the initial PID namespace, where a kernel pid names the
	// process (utils.KernelPidsLocalIn).
	if err := os.MkdirAll(filepath.Join(proc, "self", "ns"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("pid:[4026531836]", filepath.Join(proc, "self", "ns", "pid")); err != nil {
		t.Fatal(err)
	}
	write(filepath.Join(proc, "4242", "environ"), []byte("PATH=/bin\x00TZ=Asia/Kolkata\x00"))
	write(filepath.Join(proc, "4242", "stat"), []byte("4242 (app) S 1 1 1 0 -1 4194560 100 0 0 0 1 2 0 0 20 0 4 0 500 1000 100 18446744073709551615"))
	write(filepath.Join(proc, "4242", "root", "usr", "share", "zoneinfo", "Asia", "Kolkata"), kolkata)

	at := time.Date(2026, 10, 28, 7, 30, 0, 0, time.UTC)
	z := zoneThrough(t, proc, 4242)
	if z == nil {
		t.Fatalf("the learn step of a connection of a process in IST reads no zone; want IST")
	}
	if _, off := at.In(z).Zone(); off != 19800 {
		t.Errorf("the learn step of a connection of a process in IST reads %v (offset %d); want IST's 19800, never the agent's UTC", z, off)
	}
	z = zoneThrough(t, proc, 9999)
	if z == nil {
		t.Fatalf("the learn step of a connection of a process keploy cannot read reads no zone; want the agent's own")
	}
	if _, off := at.In(z).Zone(); off != 0 {
		t.Errorf("the learn step of a connection of a process keploy cannot read reads %v (offset %d); want the agent's UTC", z, off)
	}
}
