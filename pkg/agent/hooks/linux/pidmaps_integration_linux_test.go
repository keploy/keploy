//go:build linux && pidmap_integration

// Root-only: it loads keploy's BPF object into the running kernel and creates
// a PID namespace, so it lives behind the pidmap_integration build tag and runs
// as root in its own CI job (.github/workflows/mock_linux.yml), never in
// `go test ./...`.

package linux

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/cilium/ebpf"
	"github.com/cilium/ebpf/link"
	"github.com/cilium/ebpf/rlimit"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
	"golang.org/x/sys/unix"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/agent/hooks/structs"
)

// socketHelperEnv makes a re-executed test binary a helper process: it calls
// socket() once, says "ready", and lives until its stdin closes.
const socketHelperEnv = "KEPLOY_PIDMAP_TEST_HELPER"

// ownPIDNSEnv marks the run inOwnPIDNamespace re-executed.
const ownPIDNSEnv = "KEPLOY_PIDMAP_TEST_PIDNS"

func TestMain(m *testing.M) {
	if port := os.Getenv(udpHelperEnv); port != "" {
		os.Exit(runUDPHelper(port))
	}
	if step := os.Getenv(unixHelperEnv); step != "" {
		os.Exit(runUnixHelper(step))
	}
	if mode := os.Getenv(readyHelperEnv); mode != "" {
		os.Exit(runReadyHelper(mode))
	}
	if os.Getenv(socketHelperEnv) == "1" {
		fd, err := unix.Socket(unix.AF_INET, unix.SOCK_DGRAM, 0)
		if err != nil {
			fmt.Println("helper:", err)
			os.Exit(3)
		}
		fmt.Println("ready")
		_, _ = io.Copy(io.Discard, os.Stdin)
		_ = unix.Close(fd)
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// inOwnPIDNamespace runs the calling test again, alone, as the first process
// of a new PID namespace, and reports whether the caller is that run. The
// hooks track every process of the agent's PID namespace that calls socket();
// in one shared with a host, other processes take places in the map and are
// refused them while the test runs, so the counts it checks could not be
// exact. When the run exits the kernel kills whatever it left there.
func inOwnPIDNamespace(t *testing.T) bool {
	t.Helper()
	if os.Getenv(ownPIDNSEnv) == "1" {
		return true
	}
	if os.Geteuid() != 0 {
		t.Fatal("this test needs root (it loads BPF programs and creates a PID namespace); run the test binary with sudo")
	}
	cmd := exec.Command("/proc/self/exe", "-test.run=^"+regexp.QuoteMeta(t.Name())+"$", "-test.count=1", "-test.v")
	cmd.Env = append(os.Environ(), ownPIDNSEnv+"=1")
	cmd.SysProcAttr = &syscall.SysProcAttr{Cloneflags: syscall.CLONE_NEWPID, Pdeathsig: syscall.SIGKILL}
	out, err := cmd.CombinedOutput()
	t.Logf("the run in a PID namespace of its own:\n%s", out)
	if err != nil {
		t.Fatalf("failed in a PID namespace of its own: %v", err)
	}
	if !strings.Contains(string(out), "--- PASS: "+t.Name()) {
		t.Fatalf("the run in a PID namespace of its own did not report %s as passed", t.Name())
	}
	return false
}

// startSocketHelper starts a process that calls socket() once and stays
// alive until the test ends, and waits for its call.
func startSocketHelper(t *testing.T) {
	t.Helper()
	cmd := exec.Command("/proc/self/exe", "-test.run=^$")
	cmd.Env = append(os.Environ(), socketHelperEnv+"=1")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = stdin.Close(); _ = cmd.Wait() })
	line, err := bufio.NewReader(stdout).ReadString('\n')
	if strings.TrimSpace(line) != "ready" {
		t.Fatalf("socket helper %d: got %q (%v), want ready", cmd.Process.Pid, line, err)
	}
}

// warnsAbout counts the WARN lines about refusals into the named map.
func warnsAbout(logs *observer.ObservedLogs, name string) []observer.LoggedEntry {
	var out []observer.LoggedEntry
	for _, e := range logs.FilterLevelExact(zapcore.WarnLevel).All() {
		if e.ContextMap()["map"] == name {
			out = append(out, e)
		}
	}
	return out
}

// The keploy side of a full PID map, against keploy's own BPF object on the
// running kernel: live processes fill target_namespace_pids, the kernel
// refuses the next one, and the agent's hooks report it -- the count GET
// /agent/pidmaps answers (Hooks.PIDMapRefusals), and one WARN from the watcher
// Hooks.Load runs (watchPIDMapRefusals), at the first refusal and not again.
// Before pid_map_insert_failures there was nothing to read, and nothing was
// said: the refused process's calls simply went to the real destination.
//
// target_namespace_pids is loaded with room for two processes, so that live
// processes of the test's own fill it; the mock e2e
// (mock-pid-map-overflow-linux.sh) fills the real 10240.
func TestAProcessThePIDMapRefusesIsReported(t *testing.T) {
	if !inOwnPIDNamespace(t) {
		return
	}
	const room = 2
	if err := rlimit.RemoveMemlock(); err != nil {
		t.Fatal(err)
	}
	spec, err := loadBpf()
	if err != nil {
		t.Fatal(err)
	}
	spec.Maps["target_namespace_pids"].MaxEntries = room
	// As hooks.load does before loading.
	if p := spec.Programs["k_sockops"]; p != nil {
		p.Type = ebpf.SockOps
		p.AttachType = ebpf.AttachCGroupSockOps
	}
	core, logs := observer.New(zapcore.DebugLevel)
	h := NewHooks(zap.New(core), &config.Config{})
	if err := spec.LoadAndAssign(&h.objects, nil); err != nil {
		t.Fatalf("load keploy's BPF object: %v", err)
	}
	t.Cleanup(func() { _ = h.objects.Close() })
	h.agentRegistrationMap = h.objects.KeployAgentRegistrationMap

	// The PID tracking, attached as hooks.load attaches it.
	exit, err := link.AttachRawTracepoint(link.RawTracepointOptions{Name: "sched_process_exit", Program: h.objects.K_processExit})
	if err != nil {
		t.Fatalf("attach sched_process_exit: %v", err)
	}
	t.Cleanup(func() { _ = exit.Close() })
	sock, err := link.Tracepoint("syscalls", "sys_enter_socket", h.objects.SyscallProbeEntrySocket, nil)
	if err != nil {
		t.Fatalf("attach sys_enter_socket (is tracefs mounted?): %v", err)
	}
	t.Cleanup(func() { _ = sock.Close() })

	// This process is the agent: registered for its own PID namespace, then
	// recognised by its own socket(), so that it takes no place in the map.
	var info structs.AgentInfo
	info.KeployAgentNsPid = uint32(os.Getpid())
	if info.KeployAgentInode, info.KeployAgentDev, err = GetSelfInodeNumber(); err != nil {
		t.Fatal(err)
	}
	info.IsDocker = 1
	info.DNSPort = -1
	if err := h.SendAgentInfo(info); err != nil {
		t.Fatal(err)
	}
	fd, err := unix.Socket(unix.AF_INET, unix.SOCK_DGRAM, 0)
	if err != nil {
		t.Fatal(err)
	}
	_ = unix.Close(fd)
	var agentTGID uint32
	if err := h.objects.KeployAgentKernelPidMap.Lookup(uint32(0), &agentTGID); err != nil || agentTGID == 0 {
		t.Fatalf("the agent did not register itself: %d, %v", agentTGID, err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	watched := make(chan struct{})
	// Count the watcher's reads so settle can wait on a read happening rather
	// than on the clock: the founder rule is no wall-clock budget as a
	// correctness mechanism (mirrors fakeCounts.readsPast in the unit test).
	var reads atomic.Int64
	countingRead := func() (pidMapCounts, error) {
		c, err := h.pidMapCounts()
		reads.Add(1)
		return c, err
	}
	go func() {
		defer close(watched)
		watchPIDMapRefusals(ctx, h.logger, countingRead, 10*time.Millisecond)
	}()
	t.Cleanup(func() { cancel(); <-watched })

	refusedSoFar := func() uint64 {
		t.Helper()
		r, err := h.PIDMapRefusals(ctx)
		if err != nil {
			t.Fatalf("Hooks.PIDMapRefusals: %v", err)
		}
		if r.Excluded != 0 {
			t.Fatalf("Hooks.PIDMapRefusals counts %d refused inserts into excluded_pids, want 0", r.Excluded)
		}
		// The size a session's reason gives is the loaded map's own.
		if r.TargetCapacity != room {
			t.Fatalf("Hooks.PIDMapRefusals says target_namespace_pids holds %d, want the %d it was loaded with", r.TargetCapacity, room)
		}
		return r.Target
	}
	// settle returns once the watcher has read the counts at least twice more,
	// so the test waits on reads of the current state, not on the clock. The
	// test binary's -timeout bounds a watcher that never reads.
	settle := func() {
		t.Helper()
		start := reads.Load()
		for reads.Load() < start+2 {
			time.Sleep(time.Millisecond)
		}
	}

	for i := 0; i < room; i++ {
		startSocketHelper(t)
	}
	settle()
	if n := refusedSoFar(); n != 0 {
		t.Fatalf("with room for every process, Hooks.PIDMapRefusals counts %d refused, want 0", n)
	}
	if w := warnsAbout(logs, "target_namespace_pids"); len(w) != 0 {
		t.Fatalf("with room for every process, the agent warned: %v", w[0].Message)
	}

	// target_namespace_pids is full of live processes: the next is refused.
	startSocketHelper(t)
	if n := refusedSoFar(); n != 1 {
		t.Fatalf("one process refused a place in the full target_namespace_pids, and Hooks.PIDMapRefusals counts %d, want 1", n)
	}
	// The watcher's next read warns; waited for however long a loaded runner
	// takes to make it (the test binary's -timeout bounds a watcher that
	// never reads).
	for len(warnsAbout(logs, "target_namespace_pids")) == 0 {
		time.Sleep(10 * time.Millisecond)
	}
	w := warnsAbout(logs, "target_namespace_pids")
	if len(w) != 1 {
		t.Fatalf("the agent logged %d WARN lines for the refused process, want 1", len(w))
	}
	fields := w[0].ContextMap()
	if fields["refused_inserts"] != uint64(1) || fields["capacity"] != uint32(room) || fields["error"] != "E2BIG" || fields["tgid"] == uint32(0) {
		t.Fatalf("the WARN says %v; want refused_inserts 1, capacity %d, error E2BIG and the refused TGID", fields, room)
	}
	t.Logf("the agent warned: %s %v", w[0].Message, fields)

	// Another refused process adds to the count, and is not warned again.
	startSocketHelper(t)
	settle()
	if n := refusedSoFar(); n != 2 {
		t.Fatalf("two processes refused, and Hooks.PIDMapRefusals counts %d, want 2", n)
	}
	if w := warnsAbout(logs, "target_namespace_pids"); len(w) != 1 {
		t.Fatalf("the agent logged %d WARN lines after a second refusal, want still 1", len(w))
	}
}
