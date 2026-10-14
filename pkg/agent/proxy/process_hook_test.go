package proxy

import (
	"context"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.uber.org/zap"
)

// Under the proven switch, seeProcess tells OnProcess about each real process,
// and nothing about pid 0 (a connection the kernel could not attribute); with
// no OnProcess set, or the switch off, it does nothing.
func TestSeeProcessTellsTheBuildEachProcess(t *testing.T) {
	prev := OnProcess
	t.Cleanup(func() { OnProcess = prev })
	t.Setenv(provenswitch.Env, "1")

	OnProcess = nil
	seeProcess(7)

	var seen []uint32
	OnProcess = func(pid uint32) { seen = append(seen, pid) }
	seeProcess(0)
	seeProcess(42)
	seeProcess(42)
	if len(seen) != 2 || seen[0] != 42 || seen[1] != 42 {
		t.Fatalf("OnProcess saw %v", seen)
	}

	t.Setenv(provenswitch.Env, "")
	seeProcess(43)
	if len(seen) != 2 {
		t.Fatalf("with the switch off OnProcess saw %v", seen)
	}
}

// Under the proven switch, handleConnection tells OnProcess the kernel PID of
// each connection it accepts, before it serves it.
func TestHandleConnectionTellsTheBuildTheCaller(t *testing.T) {
	prev := OnProcess
	t.Cleanup(func() { OnProcess = prev })
	t.Setenv(provenswitch.Env, "1")
	seen := make(chan uint32, 1)
	OnProcess = func(pid uint32) { seen <- pid }

	upstream, accepted, _ := echoUpstream(t)
	app, src := acceptedConn(t)
	dest := ipv4Dest(t, upstream)
	dest.KernelPid = 4242
	p := &Proxy{logger: zap.NewNop(), DestInfo: fakeDestInfo{dest: dest}, agentMode: models.MODE_RECORD}
	predialedTo(t, p, src, upstream)
	<-accepted

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- p.handleConnection(ctx, src) }()
	if pid := awaitEvent(t, seen, "OnProcess being told the caller"); pid != 4242 {
		t.Fatalf("OnProcess was told %d, want the connection's kernel PID 4242", pid)
	}
	_ = app.Close()
	cancel()
	awaitEvent(t, done, "handleConnection returning")
}
