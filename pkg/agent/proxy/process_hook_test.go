package proxy

import (
	"context"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.uber.org/zap"
)

// Under the proven switch, seeProcess tells OnProcess about each process,
// one the agent cannot name too (Pid 0, KernelPid set: outside its PID
// namespace), and nothing about one with no number at all (a connection the
// kernel did not attribute); with no OnProcess set, or the switch off, it
// does nothing.
func TestSeeProcessTellsTheBuildEachProcess(t *testing.T) {
	prev := OnProcess
	t.Cleanup(func() { OnProcess = prev })
	t.Setenv(provenswitch.Env, "1")

	OnProcess = nil
	seeProcess(Process{Pid: 7, KernelPid: 7})

	var seen []Process
	OnProcess = func(p Process) { seen = append(seen, p) }
	seeProcess(Process{})
	seeProcess(Process{Pid: 42, KernelPid: 42})
	seeProcess(Process{Pid: 42, KernelPid: 42})
	seeProcess(Process{Pid: 5})                 // a listener's owner, from the agent's /proc
	seeProcess(Process{Pid: 0, KernelPid: 911}) // outside the agent's PID namespace
	want := []Process{{Pid: 42, KernelPid: 42}, {Pid: 42, KernelPid: 42}, {Pid: 5}, {KernelPid: 911}}
	if len(seen) != len(want) {
		t.Fatalf("OnProcess saw %+v, want %+v", seen, want)
	}
	for i := range want {
		if seen[i] != want[i] {
			t.Fatalf("OnProcess saw %+v, want %+v", seen, want)
		}
	}

	t.Setenv(provenswitch.Env, "")
	seeProcess(Process{Pid: 43, KernelPid: 43})
	if len(seen) != len(want) {
		t.Fatalf("with the switch off OnProcess saw %+v", seen)
	}
}

// Under the proven switch, handleConnection tells OnProcess the process of
// each connection it accepts, before it serves it: its number in the agent's
// PID namespace (the redirect's AgentNsPid, by which the agent reads it in its
// own /proc) beside the kernel's, or, for one the redirect could not name
// there, the kernel's alone.
func TestHandleConnectionTellsTheBuildTheCaller(t *testing.T) {
	for _, tc := range []struct {
		name       string
		agentNsPid uint32
	}{
		{"named in the agent's PID namespace", 17},
		{"outside the agent's PID namespace", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			prev := OnProcess
			t.Cleanup(func() { OnProcess = prev })
			t.Setenv(provenswitch.Env, "1")
			seen := make(chan Process, 1)
			OnProcess = func(p Process) { seen <- p }

			upstream, accepted, _ := echoUpstream(t)
			app, src := acceptedConn(t)
			dest := ipv4Dest(t, upstream)
			dest.KernelPid = 4242
			dest.AgentNsPid = tc.agentNsPid
			p := &Proxy{logger: zap.NewNop(), DestInfo: fakeDestInfo{dest: dest}, agentMode: models.MODE_RECORD}
			predialedTo(t, p, src, upstream)
			<-accepted

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan error, 1)
			go func() { done <- p.handleConnection(ctx, src) }()
			want := Process{Pid: tc.agentNsPid, KernelPid: 4242}
			if got := awaitEvent(t, seen, "OnProcess being told the caller"); got != want {
				t.Fatalf("OnProcess was told %+v, want %+v", got, want)
			}
			_ = app.Close()
			cancel()
			_ = awaitEvent(t, done, "handleConnection returning")
		})
	}
}
