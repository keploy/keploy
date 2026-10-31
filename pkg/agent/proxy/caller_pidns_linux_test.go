//go:build linux

package proxy

import (
	"context"
	"net"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"go.keploy.io/server/v3/pkg/agent"
	"go.keploy.io/server/v3/pkg/agent/proxy/util"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// noProcess is a number no process has in this /proc: pid_max, which every
// PID is below. It stands for the kernel's number of a caller that an agent
// in a PID namespace of its own (keploy in a container without the host's
// PID namespace) does not see under that number.
func noProcess(t *testing.T) uint32 {
	t.Helper()
	b, err := os.ReadFile("/proc/sys/kernel/pid_max")
	if err != nil {
		t.Fatal(err)
	}
	n, err := strconv.ParseUint(strings.TrimSpace(string(b)), 10, 32)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := utils.ReadProcStat(int(n)); ok {
		t.Fatalf("precondition: pid_max %d names a process", n)
	}
	return uint32(n)
}

// liveCounter sets p's live handler to one that counts its calls and relays
// each to the listener, as the ingress does.
func liveCounter(p *Proxy) *atomic.Int64 {
	var n atomic.Int64
	p.live = func(_ context.Context, conn net.Conn, upstream string, _ uint16) {
		n.Add(1)
		up, err := net.Dial("tcp", upstream)
		if err != nil {
			return
		}
		defer up.Close()
		util.RelayRawPassthrough(conn, up)
	}
	return &n
}

// A test's call to an app it started goes to the live handler, which records
// it as a test case, by the caller's number in the agent's PID namespace (the
// eBPF redirect's AgentNsPid), whatever the kernel numbers it. An agent in a
// PID namespace of its own got the kernel's number, which names another
// process or none in its /proc, so the call never descended from the caller
// and every test case of the run was lost, with the switch off and on.
func TestATestsCallToTheAppItStartedIsLiveByItsNumberInTheAgentsPIDNamespace(t *testing.T) {
	for _, proven := range []bool{false, true} {
		t.Run("proven="+strconv.FormatBool(proven), func(t *testing.T) {
			recorded.forget()
			t.Cleanup(recorded.forget)
			if proven {
				t.Setenv("KEPLOY_PROVEN", "1")
			}
			p := selfCallProxy()
			lives := liveCounter(p)
			port := pingServer(t)
			// The listener is this process's; its caller is this process's
			// parent, which it descends from.
			dest := &agent.NetworkAddress{Version: 4, Port: port, KernelPid: noProcess(t), AgentNsPid: uint32(os.Getppid())}
			served, got, row := callTreeAs(t, p, dest)
			if !served || got != "pong" || row != nil {
				t.Fatalf("a call to the run's own listener must be relayed: served=%v answer=%q row=%+v", served, got, row)
			}
			if n := lives.Load(); n != 1 {
				t.Fatalf("a test's call to the app it started went to the live handler %d times, want once (the caller by its number in the agent's PID namespace)", n)
			}
		})
	}
}

// Under the proven switch a process's call to its own listener (an
// in-process test server) is relayed and not listed, by the caller's number
// in the agent's PID namespace: by the kernel's, every such call of an agent
// in a PID namespace of its own was listed as a LIVE call to another process.
func TestUnderTheSwitchACallToItsOwnListenerIsNotListedWhateverTheKernelNumbersIt(t *testing.T) {
	p, s := provenTreeProxy(t)
	lives := liveCounter(p)
	port := pingServer(t)
	dest := &agent.NetworkAddress{Version: 4, Port: port, KernelPid: noProcess(t), AgentNsPid: uint32(os.Getpid())}
	if served, got, _ := callTreeAs(t, p, dest); !served || got != "pong" {
		t.Fatalf("a self-call must be relayed: served=%v answer=%q", served, got)
	}
	if live, mocked := s.rows(); len(live) != 0 || len(mocked) != 0 {
		t.Fatalf("a process's call to its own listener is not listed, got live=%+v mocked=%+v", live, mocked)
	}
	if n := lives.Load(); n != 0 {
		t.Fatalf("a process's call to its own listener went to the live handler %d times", n)
	}
}

// A call to the run's own listener whose caller the kernel named but not in
// the agent's PID namespace (AgentNsPid 0: a process outside it) may be a
// test's call to the app it started: the agent cannot tell, so it relays the
// call to the listener, records no test case, and says so: at WARN for the
// first as it comes, and the count at WARN when the run ends, never a silent
// 0 cases.
func TestACallWhoseCallerTheAgentCannotNameIsCountedAndSaid(t *testing.T) {
	recorded.forget()
	t.Cleanup(recorded.forget)
	core, logs := observer.New(zap.DebugLevel)
	p := selfCallProxy()
	p.logger = zap.New(core)
	lives := liveCounter(p)
	port := pingServer(t)
	dest := &agent.NetworkAddress{Version: 4, Port: port, KernelPid: noProcess(t)}
	const calls = 3
	for range calls {
		if served, got, row := callTreeAs(t, p, dest); !served || got != "pong" || row != nil {
			t.Fatalf("a call from a process the agent cannot name must still be relayed to the listener: served=%v answer=%q row=%+v", served, got, row)
		}
	}
	if n := lives.Load(); n != 0 {
		t.Fatalf("a call whose caller the agent cannot name went to the live handler %d times", n)
	}
	first := logs.FilterMessage(unnamedCallerWarning).All()
	if len(first) != 1 || first[0].Level != zapcore.WarnLevel || first[0].ContextMap()["caller_kernel_pid"] != dest.KernelPid {
		t.Fatalf("want the first such call said once at WARN with the caller's kernel number %d, got %+v", dest.KernelPid, first)
	}
	count := func() []observer.LoggedEntry {
		return logs.FilterMessageSnippet("came from processes keploy cannot name").All()
	}
	_ = p.SetGracefulShutdown(context.Background())
	_ = p.SetGracefulShutdown(context.Background())
	if got := count(); len(got) != 1 || got[0].Level != zapcore.WarnLevel || got[0].ContextMap()["calls"] != int64(calls) {
		t.Fatalf("want one WARN count of %d calls when the run ends, said once however often it is asked, got %+v", calls, got)
	}
}
