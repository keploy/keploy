//go:build linux && pidmap_integration

// Root-only, like pidmaps_integration_linux_test.go: it loads keploy's BPF
// objects (the main one and unixconn) into the running kernel.

package linux

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/cilium/ebpf"
	"github.com/cilium/ebpf/link"
	"github.com/cilium/ebpf/rlimit"
	"go.uber.org/zap"
	"golang.org/x/sys/unix"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/internal/testnet"
	"go.keploy.io/server/v3/pkg/agent/bpfsafety"
	"go.keploy.io/server/v3/pkg/agent/hooks/structs"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
)

// unixHelperEnv makes a re-executed test binary an AF_UNIX helper: "listen
// PATH" binds and listens on PATH, "connect PATH" connects to it; either says
// "ready" (or "helper: <error>") and lives until its stdin closes.
const unixHelperEnv = "KEPLOY_UNIX_TEST_HELPER"

func runUnixHelper(step string) int {
	f := strings.Fields(step)
	if len(f) != 2 {
		fmt.Println("helper: bad step", step)
		return 2
	}
	fd, err := unix.Socket(unix.AF_UNIX, unix.SOCK_STREAM, 0)
	if err == nil {
		switch f[0] {
		case "listen":
			if err = unix.Bind(fd, &unix.SockaddrUnix{Name: f[1]}); err == nil {
				err = unix.Listen(fd, 8)
			}
		case "connect":
			err = unix.Connect(fd, &unix.SockaddrUnix{Name: f[1]})
		default:
			err = fmt.Errorf("unknown step %s", f[0])
		}
	}
	if err != nil {
		fmt.Println("helper:", err)
		return 3
	}
	fmt.Println("ready")
	_, _ = bufio.NewReader(os.Stdin).ReadString(0)
	return 0
}

func startUnixHelper(t *testing.T, step string) {
	t.Helper()
	cmd := exec.Command(appExe(), "-test.run=^$")
	cmd.Env = append(os.Environ(), unixHelperEnv+"="+step)
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
		t.Fatalf("unix helper (%s): got %q (%v), want ready", step, line, err)
	}
}

// The agent side of the AF_UNIX report, against keploy's own objects: an
// application process that connects to a Unix socket no member owns is a
// unix_socket capture loss in Hooks.CaptureLosses (what Agent.CaptureLosses,
// and so every test after it under the proven switch, is charged with); one
// to a member's socket is not. Run in a PID namespace of its own, so that no
// host process becomes a member while it runs.
func TestAnAppConnectToAUnixSocketNoMemberOwnsIsACaptureLoss(t *testing.T) {
	if !inOwnPIDNamespace(t) {
		return
	}
	// The syscall tracepoints, as on a kernel without BPF trampolines,
	// when asked (the test binary is re-run in the namespace with it).
	if os.Getenv("KEPLOY_UNIX_TEST_FALLBACK") == "1" {
		unixFallbackOnly = true
		t.Cleanup(func() { unixFallbackOnly = false })
	}
	if err := rlimit.RemoveMemlock(); err != nil {
		t.Fatal(err)
	}
	dir := testnet.SocketDir(t)
	outside, own := filepath.Join(dir, "outside.sock"), filepath.Join(dir, "member.sock")
	// Listening before the agent is registered: never a member.
	startUnixHelper(t, "listen "+outside)

	h := unixReportAgent(t)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	// The report runs under the proven switch only.
	t.Setenv(provenswitch.Env, "1")
	bpfsafety.AllowShortGraceForTest(t)
	bpfsafety.ResetShutdownForTest(t)
	h.startUnixConn(ctx)
	h.unixMu.Lock()
	loaded := h.unix != nil
	fallback := h.unix != nil && h.unix.fallback
	h.unixMu.Unlock()
	if !loaded {
		t.Fatal("the AF_UNIX report did not load (the agent's WARN says why)")
	}
	if !fallback {
		graceDir := bpfsafety.ActiveGraceDir()
		if graceDir == "" {
			t.Fatal("unixconn attached fexit trampolines without an active bpfsafety grace directory")
		}
		entries, err := os.ReadDir(graceDir)
		if err != nil {
			t.Fatalf("read unixconn grace dir %s: %v", graceDir, err)
		}
		pinned := 0
		for _, e := range entries {
			if strings.Contains(e.Name(), "-unixconn-") {
				pinned++
			}
		}
		if !strings.Contains(graceDir, "/"+bpfsafety.GraceRootSubdir+"/") {
			t.Fatalf("unixconn grace dir %s must live under /%s/", graceDir, bpfsafety.GraceRootSubdir)
		}
		if pinned < len(unixFexitPrograms) {
			t.Fatalf("expected >= %d pinned unixconn fexit progs in %s, got %d", len(unixFexitPrograms), graceDir, pinned)
		}
		t.Cleanup(func() {
			bpfsafety.GracefulShutdown(context.Background(), zap.NewNop(), 0)
			_ = os.RemoveAll(graceDir)
		})
	}

	// A member's own socket first, then the outsider's: the reports come in
	// that order, so when the outsider's has been counted, the member's would
	// have been too.
	startUnixHelper(t, "listen "+own)
	startUnixHelper(t, "connect "+own)
	startUnixHelper(t, "connect "+outside)
	// One read, after every report the kernel made is handled.
	if err := h.DrainCaptureLosses(context.Background()); err != nil {
		t.Fatal(err)
	}
	got := h.CaptureLosses()
	if len(got) != 1 || got[0].What != outside || got[0].Kind != models.CaptureLossUnixSocket || got[0].Count != 1 {
		t.Fatalf("capture losses %+v, want one unix_socket loss, the outsider's socket (the member's own not counted)", got)
	}
}

// unixReportAgent loads keploy's main object as the agent does, registers this
// process as the agent (in a PID namespace of its own: every process there
// that calls socket() is a member), and returns the hooks, the AF_UNIX report
// not yet started.
func unixReportAgent(t *testing.T) *Hooks {
	t.Helper()
	spec, err := loadBpf()
	if err != nil {
		t.Fatal(err)
	}
	if p := spec.Programs["k_sockops"]; p != nil {
		p.Type = ebpf.SockOps
		p.AttachType = ebpf.AttachCGroupSockOps
	}
	h := NewHooks(zap.NewNop(), &config.Config{})
	if err := spec.LoadAndAssign(&h.objects, nil); err != nil {
		t.Fatalf("load keploy's BPF object: %v", err)
	}
	t.Cleanup(func() { _ = h.objects.Close() })
	h.agentRegistrationMap = h.objects.KeployAgentRegistrationMap
	sock, err := link.Tracepoint("syscalls", "sys_enter_socket", h.objects.SyscallProbeEntrySocket, nil)
	if err != nil {
		t.Fatalf("attach sys_enter_socket: %v", err)
	}
	t.Cleanup(func() { _ = sock.Close() })
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
	// This process is the agent, recognised by its own socket().
	fd, err := unix.Socket(unix.AF_INET, unix.SOCK_DGRAM, 0)
	if err != nil {
		t.Fatal(err)
	}
	_ = unix.Close(fd)
	return h
}

// A read of the capture losses waits for every report the kernel made before
// it: a member's connect whose report waits in the ring while the watch is
// not reading (a starved goroutine, a loaded host) is in the read that
// follows the connect, not a later one. The read is blocked in the drain
// before the watch starts; without the drain it returns without the call.
func TestAReadWaitsForTheUnixReportsTheKernelMadeBeforeIt(t *testing.T) {
	if !inOwnPIDNamespace(t) {
		return
	}
	if err := rlimit.RemoveMemlock(); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(testnet.SocketDir(t), "outside.sock")
	startUnixHelper(t, "listen "+outside) // before the agent: never a member
	h := unixReportAgent(t)
	t.Setenv(provenswitch.Env, "1")
	u, err := loadUnixConn(h.logger, &h.objects)
	if err != nil {
		t.Fatalf("the AF_UNIX report did not load: %v", err)
	}
	t.Cleanup(u.close)
	h.unixMu.Lock()
	h.unix = u
	h.unixMu.Unlock()

	// The watch is not reading: the connect's report waits in the ring.
	startUnixHelper(t, "connect "+outside)
	type read struct {
		losses models.CaptureLosses
		err    error
	}
	done := make(chan read, 1)
	go func() {
		err := h.DrainCaptureLosses(context.Background())
		done <- read{h.CaptureLosses(), err}
	}()
	for {
		select {
		case r := <-done:
			t.Fatalf("the read returned %+v (%v) before the watch handled the report the kernel made before it", r.losses, r.err)
		default:
		}
		u.drain.mu.Lock()
		w := u.drain.waiting
		u.drain.mu.Unlock()
		if w > 0 {
			break
		}
		runtime.Gosched()
	}
	go u.watch(zap.NewNop(), h.isMember)
	r := <-done
	if r.err != nil {
		t.Fatal(r.err)
	}
	if len(r.losses) != 1 || r.losses[0].Kind != models.CaptureLossUnixSocket || r.losses[0].What != outside || r.losses[0].Count != 1 {
		t.Fatalf("the read after the connect: %+v, want the outsider's socket, once", r.losses)
	}
}

// TestUnixConnNoHostBPFFSKeepsFexitWithInProcessHold proves on a real kernel
// inside a container PID namespace that when host bpffs is unavailable (e.g.
// DinD or a plain classic container without /run/keploy/host-bpffs), unixconn
// keeps fexit (with its exact in-kernel peer filter — zero capture regression
// on member-owned sockets) via detached-holder prog-FD retention instead of
// degrading to syscall tracepoints, returns from GracefulShutdown immediately
// without blocking exit, and keeps the programs loaded until the detached
// holder's wait expires.
func TestUnixConnNoHostBPFFSKeepsFexitWithInProcessHold(t *testing.T) {
	if !inOwnPIDNamespace(t) {
		return
	}
	if err := rlimit.RemoveMemlock(); err != nil {
		t.Fatal(err)
	}
	// Point host bpffs at a non-bpffs temp dir so bpfsafety.Init returns ErrEnvironmentUnsupported.
	t.Setenv(bpfsafety.EnvHostBPFFS, t.TempDir())
	t.Setenv(bpfsafety.EnvAllowUnprotectedTrampolines, "")
	t.Setenv(provenswitch.Env, "1")
	bpfsafety.AllowShortGraceForTest(t)
	bpfsafety.ResetShutdownForTest(t)

	dir := testnet.SocketDir(t)
	outside, own := filepath.Join(dir, "outside.sock"), filepath.Join(dir, "member.sock")
	startUnixHelper(t, "listen "+outside)

	h := unixReportAgent(t)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	h.startUnixConn(ctx)

	h.unixMu.Lock()
	u := h.unix
	h.unixMu.Unlock()
	if u == nil {
		t.Fatal("unixconn failed to load without host bpffs")
	}
	if u.fallback {
		t.Fatalf("unixconn wrongly degraded to tracepoint fallback without host bpffs (why=%q); want fexit with detached holder", u.why)
	}
	if !u.inProcessHold {
		t.Fatal("expected u.inProcessHold=true when host bpffs is unavailable")
	}
	if strings.Contains(u.why, bpfsafety.EnvAllowUnprotectedTrampolines) {
		t.Fatalf("OSS unixconn hold detail should not mention %s: %q", bpfsafety.EnvAllowUnprotectedTrampolines, u.why)
	}

	// Capture kernel program IDs of every unixconn fexit program BEFORE shutdown
	// so we can prove on the real kernel that they remain loaded AFTER
	// GracefulShutdown closes all links/FDs and returns immediately, and are
	// freed AFTER the detached holder process exits.
	var progIDs []ebpf.ProgramID
	for _, name := range unixFexitPrograms {
		p := u.coll.Programs[name]
		if p == nil {
			t.Fatalf("missing fexit program %s in unixconn collection", name)
		}
		info, err := p.Info()
		if err != nil {
			t.Fatalf("prog %s info: %v", name, err)
		}
		id, ok := info.ID()
		if !ok {
			t.Fatalf("prog %s has no kernel ID", name)
		}
		progIDs = append(progIDs, id)
	}

	startUnixHelper(t, "listen "+own)
	startUnixHelper(t, "connect "+own)
	startUnixHelper(t, "connect "+outside)
	if err := h.DrainCaptureLosses(context.Background()); err != nil {
		t.Fatal(err)
	}
	got := h.CaptureLosses()
	if len(got) != 1 || got[0].What != outside || got[0].Kind != models.CaptureLossUnixSocket || got[0].Count != 1 {
		t.Fatalf("capture losses %+v, want only outsider socket (in-kernel peer filter intact in PID namespace)", got)
	}

	detached := false
	bpfsafety.RegisterDetachHook(func() {
		detached = true
	})

	// GracefulShutdown MUST return immediately (handing prog FDs to detached 2s
	// holder process rather than blocking the caller for 2s).
	start := time.Now()
	bpfsafety.GracefulShutdown(context.Background(), zap.NewNop(), 2*time.Second)
	elapsed := time.Since(start)
	if !detached {
		t.Fatal("GracefulShutdown did not run detach hooks synchronously")
	}
	if elapsed >= 1*time.Second {
		t.Fatalf("GracefulShutdown blocked for %v (>= 1s); must hand off to detached holder immediately", elapsed)
	}

	// Every fexit program MUST STILL be loaded in the kernel at W-0.5s (1.5s of
	// the 2s hold) after GracefulShutdown returned!
	if rem := time.Until(start.Add(1500 * time.Millisecond)); rem > 0 {
		time.Sleep(rem)
	}
	for _, id := range progIDs {
		alive, err := ebpf.NewProgramFromID(id)
		if err != nil {
			t.Fatalf("detached holder failed: fexit prog %d disappeared before W-0.5s (1.5s of 2s hold): %v", id, err)
		}
		_ = alive.Close()
	}

	// Once W + 3s (5s) elapses, the detached 2s holder MUST have exited and
	// every prog ID MUST be gone.
	deadline := start.Add(5 * time.Second)
	for _, id := range progIDs {
		gone := false
		for time.Now().Before(deadline) {
			p, err := ebpf.NewProgramFromID(id)
			if err != nil {
				gone = true
				break
			}
			_ = p.Close()
			time.Sleep(100 * time.Millisecond)
		}
		if !gone {
			t.Fatalf("fexit prog %d still loaded at W+3s (5s) after detached 2s holder was spawned", id)
		}
	}
}
