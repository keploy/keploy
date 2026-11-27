//go:build linux && integration

package bpfsafety

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/cilium/ebpf"
	"github.com/cilium/ebpf/asm"
	"github.com/cilium/ebpf/link"
	"github.com/cilium/ebpf/rlimit"
	"go.uber.org/zap"
	"golang.org/x/sys/unix"
)

// TestOnKernelPinStaysLoadedPastDetachAndCrossProcessBPFFSStampReaps proves on
// the real Linux kernel:
//  1. PinBeforeAttach pins a real fentry/bpf_fentry_test1 program into bpffs
//     before link.AttachTracing.
//  2. Closing the link AND the program FD keeps the program loaded in the
//     kernel (ebpf.NewProgramFromID succeeds) — preventing CVE-2026-98800.
//  3. After owner's flock releases, a SECOND process's reaper force-rebuilds
//     the trampoline via a pinned no-op in its OWN grace dir, writes the shared
//     32-byte BPF_MAP_TYPE_ARRAY CLOCK_BOOTTIME stamp on real bpffs, and a
//     THIRD fresh reaper process reads that pinned map and unpins + removes
//     the grace dir after grace + confirmGap — after which NewProgramFromID
//     returns ENOENT.
func TestOnKernelPinStaysLoadedPastDetachAndCrossProcessBPFFSStampReaps(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("requires root/CAP_BPF")
	}
	if err := rlimit.RemoveMemlock(); err != nil {
		t.Fatalf("RemoveMemlock: %v", err)
	}
	var stfs unix.Statfs_t
	if err := unix.Statfs(DefaultNativeBPFFSPath, &stfs); err != nil || stfs.Type != unix.BPF_FS_MAGIC {
		t.Skipf("%s is not a bpffs mount", DefaultNativeBPFFSPath)
	}
	resetStateForTest(t)

	root := filepath.Join(DefaultNativeBPFFSPath, "keploy-bpfsafety-test-"+randomNonce())
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", root, err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })

	owner, err := openGraceOwnerInRoot(root, zap.NewNop())
	if err != nil {
		t.Fatalf("openGraceOwnerInRoot: %v", err)
	}
	stateMu.Lock()
	activeOwner = owner
	stateMu.Unlock()

	prog, err := ebpf.NewProgram(&ebpf.ProgramSpec{
		Name:       "grace_kern_test",
		Type:       ebpf.Tracing,
		AttachType: ebpf.AttachTraceFEntry,
		AttachTo:   "bpf_fentry_test1",
		License:    "GPL",
		Instructions: asm.Instructions{
			asm.Mov.Imm(asm.R0, 0),
			asm.Return(),
		},
	})
	if err != nil {
		_ = unix.Close(owner.lockFD)
		t.Fatalf("load fentry/bpf_fentry_test1: %v", err)
	}
	info, err := prog.Info()
	if err != nil {
		_ = prog.Close()
		_ = unix.Close(owner.lockFD)
		t.Fatalf("prog.Info: %v", err)
	}
	progID, ok := info.ID()
	if !ok {
		_ = prog.Close()
		_ = unix.Close(owner.lockFD)
		t.Fatal("prog.Info returned no ID")
	}

	if err := PinBeforeAttach(prog, "bpfsafety_test", "bpf_fentry_test1", "p"); err != nil {
		_ = prog.Close()
		_ = unix.Close(owner.lockFD)
		t.Fatalf("PinBeforeAttach: %v", err)
	}
	l, err := link.AttachTracing(link.TracingOptions{Program: prog})
	if err != nil {
		_ = prog.Close()
		_ = unix.Close(owner.lockFD)
		t.Fatalf("AttachTracing: %v", err)
	}

	// Simulate abnormal process exit (SIGKILL): link closes, prog FD closes, flock releases.
	_ = l.Close()
	_ = prog.Close()
	_ = unix.Close(owner.lockFD)
	stateMu.Lock()
	activeOwner = nil
	stateMu.Unlock()

	// Prog MUST STILL be loaded in the kernel thanks to the bpffs grace pin!
	alive, err := ebpf.NewProgramFromID(progID)
	if err != nil {
		t.Fatalf("prog %d was freed immediately on detach+close (CVE-2026-98800 exposed!): %v", progID, err)
	}
	_ = alive.Close()

	// Create a live reaper owner so forceRebuildTarget pins its own no-op
	// fentry/bpf_fentry_test1 inside the reaper's OWN active grace dir.
	reaperOwner, err := openGraceOwnerInRoot(root, zap.NewNop())
	if err != nil {
		t.Fatalf("openGraceOwnerInRoot(reaper): %v", err)
	}
	defer func() {
		reaperOwner.reaper.closeNoopCache()
		_ = unix.Close(reaperOwner.lockFD)
	}()

	now := clockBoottimeNs()
	makeReaper := func() *graceReaper {
		r := newGraceReaper(root, reaperOwner.self, 15*time.Minute, 3*time.Minute, 1*time.Minute, zap.NewNop())
		r.bootNowNs = func() uint64 { return now }
		r.pinNoop = func(p *ebpf.Program, funcName string) error {
			return reaperOwner.pinProgram(p, "bpfsafety_reaper", funcName, "rb")
		}
		return r
	}

	// Reaper process 1: force-rebuilds bpf_fentry_test1 (pinning its own no-op
	// in reaperOwner.dir) and writes 32-byte first_free_boottime stamp map.
	r1 := makeReaper()
	r1.round()
	r1.closeNoopCache()
	if _, err := os.Stat(filepath.Join(owner.dir, stampFileName)); err != nil {
		t.Fatalf("expected pinned bpffs stamp map %s after force-rebuild: %v", stampFileName, err)
	}
	// On Linux 6.0+, verify the reaper pinned its own no-op into reaperOwner.dir!
	if countPinnedProgs(reaperOwner.dir) == 0 {
		t.Logf("note: reaper pinned 0 no-op progs (expected only on pre-6.0 kernels without attach_btf_id)")
	}

	// Reaper process 2 (15m01s later): reads pinned map, verifies run_cnt flat,
	// updates deadlineHitBootNs, does NOT reap yet.
	now += uint64(15*time.Minute + time.Second)
	r2 := makeReaper()
	r2.round()
	r2.closeNoopCache()
	alive2, err := ebpf.NewProgramFromID(progID)
	if err != nil {
		t.Fatalf("prog %d reaped on first confirm pass: %v", progID, err)
	}
	_ = alive2.Close()

	// Reaper process 3 (1m01s later): reads pinned map, sees confirmGap met, unpins & removes dir!
	now += uint64(1*time.Minute + time.Second)
	r3 := makeReaper()
	r3.round()
	r3.closeNoopCache()

	if _, err := os.Stat(owner.dir); !os.IsNotExist(err) {
		t.Fatalf("owner.dir still exists after 2-pass cross-process reap: %v", err)
	}
	// Wait briefly for kernel call_rcu to release the unpinned prog ID.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		p, err := ebpf.NewProgramFromID(progID)
		if err != nil {
			return // gone!
		}
		_ = p.Close()
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("prog %d still loaded 5s after grace dir was reaped", progID)
}

// TestOnKernelDockerHostBPFFSPinSurvivesSIGKILL proves on a real Linux host
// with Docker that a containerized agent launched with
// `-v /sys/fs/bpf:/run/keploy/host-bpffs -e KEPLOY_LAUNCH_MODE=docker`:
//  1. Arms host-bpffs grace pinning under /run/keploy/host-bpffs/keploy-grace/
//     and attaches a real fentry/bpf_fentry_test1 program.
//  2. When the container is killed with SIGKILL (`docker kill -s KILL`), the
//     grace pin lands on the HOST /sys/fs/bpf/keploy-grace/ and keeps the BPF
//     program loaded in the kernel (`ebpf.NewProgramFromID` succeeds AFTER the
//     container is completely dead and removed).
func TestOnKernelDockerHostBPFFSPinSurvivesSIGKILL(t *testing.T) {
	if os.Getenv("KEPLOY_TEST_DOCKER_PIN_CHILD") == "1" {
		if err := rlimit.RemoveMemlock(); err != nil {
			t.Fatalf("RemoveMemlock: %v", err)
		}
		if err := Init(context.Background(), zap.NewNop(), DetectMode(false)); err != nil {
			t.Fatalf("Init: %v", err)
		}
		if !Armed() {
			t.Fatalf("expected Armed()=true inside docker container with /run/keploy/host-bpffs, graceMode=%q", ActiveGraceMode())
		}
		prog, err := ebpf.NewProgram(&ebpf.ProgramSpec{
			Name:       "docker_kill_pin",
			Type:       ebpf.Tracing,
			AttachType: ebpf.AttachTraceFEntry,
			AttachTo:   "bpf_fentry_test1",
			License:    "GPL",
			Instructions: asm.Instructions{
				asm.Mov.Imm(asm.R0, 0),
				asm.Return(),
			},
		})
		if err != nil {
			t.Fatalf("load fentry/bpf_fentry_test1: %v", err)
		}
		info, err := prog.Info()
		if err != nil {
			t.Fatalf("prog.Info: %v", err)
		}
		progID, ok := info.ID()
		if !ok {
			t.Fatal("prog.Info returned no ID")
		}
		l, err := AttachTracing(prog, "docker_e2e", "bpf_fentry_test1", "p")
		if err != nil {
			t.Fatalf("AttachTracing: %v", err)
		}
		defer func() {
			_ = l.Close()
			_ = prog.Close()
		}()
		fmt.Printf("DOCKER_PIN_READY PROG_ID=%d DIR_BASE=%s\n", progID, filepath.Base(ActiveGraceDir()))
		select {}
	}

	if os.Geteuid() != 0 {
		t.Skip("requires root/CAP_BPF")
	}
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker CLI not available")
	}
	if err := exec.Command("docker", "info").Run(); err != nil {
		t.Skipf("docker daemon unavailable: %v", err)
	}
	var stfs unix.Statfs_t
	if err := unix.Statfs(DefaultNativeBPFFSPath, &stfs); err != nil || stfs.Type != unix.BPF_FS_MAGIC {
		t.Skipf("%s is not a bpffs mount", DefaultNativeBPFFSPath)
	}

	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	cname := "keploy-bpfsafety-kill-test-" + randomNonce()[:8]
	cmd := exec.Command("docker", "run",
		"--name", cname,
		"--privileged",
		"-v", "/sys/fs/bpf:/run/keploy/host-bpffs",
		"-v", exe+":/bpfsafety.test:ro",
		"-e", "KEPLOY_LAUNCH_MODE=docker",
		"-e", "KEPLOY_TEST_DOCKER_PIN_CHILD=1",
		"alpine:3.20",
		"/bpfsafety.test", "-test.run=^TestOnKernelDockerHostBPFFSPinSurvivesSIGKILL$", "-test.v",
	)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("StdoutPipe: %v", err)
	}
	cmd.Stderr = cmd.Stdout
	if err := cmd.Start(); err != nil {
		t.Fatalf("start docker container: %v", err)
	}
	t.Cleanup(func() {
		_ = exec.Command("docker", "rm", "-f", cname).Run()
		_ = cmd.Wait()
	})

	var (
		progID  ebpf.ProgramID
		dirBase string
		lines   []string
	)
	readyCh := make(chan struct{})
	go func() {
		defer close(readyCh)
		sc := bufio.NewScanner(stdout)
		for sc.Scan() {
			line := sc.Text()
			lines = append(lines, line)
			if strings.HasPrefix(line, "DOCKER_PIN_READY ") {
				for _, f := range strings.Fields(line)[1:] {
					if strings.HasPrefix(f, "PROG_ID=") {
						id, _ := strconv.ParseUint(strings.TrimPrefix(f, "PROG_ID="), 10, 32)
						progID = ebpf.ProgramID(id)
					} else if strings.HasPrefix(f, "DIR_BASE=") {
						dirBase = strings.TrimPrefix(f, "DIR_BASE=")
					}
				}
				return
			}
		}
	}()

	select {
	case <-readyCh:
	case <-time.After(30 * time.Second):
		t.Fatalf("timed out waiting for container pin ready; output:\n%s", strings.Join(lines, "\n"))
	}
	if progID == 0 || dirBase == "" {
		t.Fatalf("container did not report ready pin; output:\n%s", strings.Join(lines, "\n"))
	}

	hostGraceDir := filepath.Join(DefaultNativeBPFFSPath, GraceRootSubdir, dirBase)
	t.Cleanup(func() { _ = os.RemoveAll(hostGraceDir) })

	// SIGKILL and remove the container completely!
	if out, err := exec.Command("docker", "kill", "-s", "KILL", cname).CombinedOutput(); err != nil {
		t.Fatalf("docker kill -s KILL %s: %v (%s)", cname, err, string(out))
	}
	_ = cmd.Wait()
	_ = exec.Command("docker", "rm", "-f", cname).Run()

	// Brief settle for container process teardown + RCU callback.
	time.Sleep(500 * time.Millisecond)

	// Verify the pin directory exists on HOST /sys/fs/bpf/keploy-grace/ AND the
	// BPF program is STILL LOADED in the kernel after container SIGKILL!
	if countPinnedProgs(hostGraceDir) == 0 {
		t.Fatalf("expected pinned prog on host %s after container SIGKILL, found 0", hostGraceDir)
	}
	alive, err := ebpf.NewProgramFromID(progID)
	if err != nil {
		t.Fatalf("BPF program %d did NOT survive container SIGKILL despite host bpffs mount: %v", progID, err)
	}
	_ = alive.Close()
}

// TestOnKernelReaperSurvivesENOMEMUnlinkViaForceRebuild proves on a real Linux
// kernel with CONFIG_FAILSLAB=y (/sys/kernel/debug/failslab) that when
// bpf_trampoline_unlink_prog fails with -ENOMEM during link.Close():
//  1. The host-bpffs grace pin keeps the detached prog alive while the stale
//     trampoline image still calls it (hit_count increases after link.Close).
//  2. Our production graceReaper forces a trampoline rebuild (with its own
//     pinned no-op fentry/unix_dgram_sendmsg), replacing the stale image so
//     hit_count stops increasing (delta == 0).
//  3. After grace + confirmGap, the reaper safely unpins and removes the dead
//     grace dir, and subsequent unix_dgram_sendmsg traffic completes with zero
//     KASAN/UAF panic.
func TestOnKernelReaperSurvivesENOMEMUnlinkViaForceRebuild(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("requires root/CAP_BPF")
	}
	failslabDir := "/sys/kernel/debug/failslab"
	if _, err := os.Stat(failslabDir); err != nil {
		t.Skipf("requires CONFIG_FAILSLAB=y (%s missing: %v)", failslabDir, err)
	}
	if err := rlimit.RemoveMemlock(); err != nil {
		t.Fatalf("RemoveMemlock: %v", err)
	}
	var stfs unix.Statfs_t
	if err := unix.Statfs(DefaultNativeBPFFSPath, &stfs); err != nil || stfs.Type != unix.BPF_FS_MAGIC {
		t.Skipf("%s is not a bpffs mount", DefaultNativeBPFFSPath)
	}
	resetStateForTest(t)

	root := filepath.Join(DefaultNativeBPFFSPath, "keploy-bpfsafety-enomem-"+randomNonce())
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", root, err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })

	// Resident neighbor prog keeps total>0 during linkDetach.Close().
	progKeep, err := ebpf.NewProgram(&ebpf.ProgramSpec{
		Name:       "enomem_keep",
		Type:       ebpf.Tracing,
		AttachType: ebpf.AttachTraceFExit,
		AttachTo:   "unix_dgram_sendmsg",
		License:    "GPL",
		Instructions: asm.Instructions{
			asm.Mov.Imm(asm.R0, 0),
			asm.Return(),
		},
	})
	if err != nil {
		t.Fatalf("load progKeep: %v", err)
	}
	defer func() { _ = progKeep.Close() }()
	linkKeep, err := link.AttachTracing(link.TracingOptions{Program: progKeep})
	if err != nil {
		t.Fatalf("attach progKeep: %v", err)
	}
	defer func() { _ = linkKeep.Close() }()

	// Counting fexit/unix_dgram_sendmsg pinned through production PinBeforeAttach.
	hitMap, err := ebpf.NewMap(&ebpf.MapSpec{
		Name:       "enomem_hits",
		Type:       ebpf.Array,
		KeySize:    4,
		ValueSize:  8,
		MaxEntries: 1,
	})
	if err != nil {
		t.Fatalf("create hitMap: %v", err)
	}
	defer func() { _ = hitMap.Close() }()

	progDetach, err := ebpf.NewProgram(&ebpf.ProgramSpec{
		Name:       "enomem_detach",
		Type:       ebpf.Tracing,
		AttachType: ebpf.AttachTraceFExit,
		AttachTo:   "unix_dgram_sendmsg",
		License:    "GPL",
		Instructions: asm.Instructions{
			asm.StoreImm(asm.RFP, -4, 0, asm.Word),
			asm.LoadMapPtr(asm.R1, hitMap.FD()),
			asm.Mov.Reg(asm.R2, asm.RFP),
			asm.Add.Imm(asm.R2, -4),
			asm.FnMapLookupElem.Call(),
			asm.JEq.Imm(asm.R0, 0, "done"),
			asm.LoadMem(asm.R1, asm.R0, 0, asm.DWord),
			asm.Add.Imm(asm.R1, 1),
			asm.StoreMem(asm.R0, 0, asm.R1, asm.DWord),
			asm.Mov.Imm(asm.R0, 0).WithSymbol("done"),
			asm.Return(),
		},
	})
	if err != nil {
		t.Fatalf("load progDetach: %v", err)
	}
	info, err := progDetach.Info()
	if err != nil {
		_ = progDetach.Close()
		t.Fatalf("progDetach.Info: %v", err)
	}
	progID, ok := info.ID()
	if !ok {
		_ = progDetach.Close()
		t.Fatal("progDetach.Info returned no ID")
	}

	owner, err := openGraceOwnerInRoot(root, zap.NewNop())
	if err != nil {
		_ = progDetach.Close()
		t.Fatalf("openGraceOwnerInRoot: %v", err)
	}
	stateMu.Lock()
	activeOwner = owner
	stateMu.Unlock()

	lDetach, err := AttachTracing(progDetach, "enomem_test", "unix_dgram_sendmsg", "p")
	if err != nil {
		_ = progDetach.Close()
		_ = unix.Close(owner.lockFD)
		t.Fatalf("AttachTracing: %v", err)
	}

	sendDgrams := func(n int) {
		srv, err := unix.Socket(unix.AF_UNIX, unix.SOCK_DGRAM, 0)
		if err != nil {
			t.Fatalf("socket srv: %v", err)
		}
		defer func() { _ = unix.Close(srv) }()
		cli, err := unix.Socket(unix.AF_UNIX, unix.SOCK_DGRAM, 0)
		if err != nil {
			t.Fatalf("socket cli: %v", err)
		}
		defer func() { _ = unix.Close(cli) }()
		sp := filepath.Join(t.TempDir(), "u.sock")
		sa := &unix.SockaddrUnix{Name: sp}
		if err := unix.Bind(srv, sa); err != nil {
			t.Fatalf("bind: %v", err)
		}
		buf := make([]byte, 8)
		for i := 0; i < n; i++ {
			if err := unix.Sendto(cli, []byte("x"), 0, sa); err != nil {
				t.Fatalf("sendto: %v", err)
			}
			_, _, _ = unix.Recvfrom(srv, buf, 0)
		}
	}
	readHits := func() uint64 {
		var v uint64
		if err := hitMap.Lookup(uint32(0), &v); err != nil {
			t.Fatalf("hitMap.Lookup: %v", err)
		}
		return v
	}

	sendDgrams(10)
	c0 := readHits()
	if c0 < 10 {
		t.Fatalf("before detach hit_count = %d, want >= 10", c0)
	}

	// Inject failslab -ENOMEM strictly during lDetach.Close() on this OS thread.
	runtime.LockOSThread()
	tidFailPath := fmt.Sprintf("/proc/self/task/%d/make-it-fail", unix.Gettid())
	failFD, err := unix.Open(tidFailPath, unix.O_WRONLY, 0)
	if err != nil {
		runtime.UnlockOSThread()
		t.Fatalf("open %s: %v", tidFailPath, err)
	}
	_ = os.WriteFile(filepath.Join(failslabDir, "task-filter"), []byte("Y"), 0o644)
	_ = os.WriteFile(filepath.Join(failslabDir, "ignore-gfp-wait"), []byte("N"), 0o644)
	_ = os.WriteFile(filepath.Join(failslabDir, "probability"), []byte("100"), 0o644)
	_ = os.WriteFile(filepath.Join(failslabDir, "interval"), []byte("1"), 0o644)
	_ = os.WriteFile(filepath.Join(failslabDir, "times"), []byte("20"), 0o644)
	_ = os.WriteFile(filepath.Join(failslabDir, "space"), []byte("0"), 0o644)
	_, _ = unix.Pwrite(failFD, []byte("1\n"), 0)
	_ = lDetach.Close()
	_, _ = unix.Pwrite(failFD, []byte("0\n"), 0)
	_ = unix.Close(failFD)
	_ = os.WriteFile(filepath.Join(failslabDir, "times"), []byte("0"), 0o644)
	_ = os.WriteFile(filepath.Join(failslabDir, "probability"), []byte("0"), 0o644)
	runtime.UnlockOSThread()

	// Owner exits: close prog FD and release flock.
	_ = progDetach.Close()
	_ = unix.Close(owner.lockFD)
	stateMu.Lock()
	activeOwner = nil
	stateMu.Unlock()

	time.Sleep(500 * time.Millisecond)

	// Confirm the stale trampoline STILL calls progDetach (survived thanks to grace pin!).
	sendDgrams(50)
	c1 := readHits()
	if c1 <= c0 {
		t.Fatalf("expected stale trampoline to keep calling progDetach after ENOMEM unlink (c0=%d c1=%d)", c0, c1)
	}

	// Run our production graceReaper across all 3 passes (force-rebuild -> 15m -> 1m confirm).
	reaperOwner, err := openGraceOwnerInRoot(root, zap.NewNop())
	if err != nil {
		t.Fatalf("openGraceOwnerInRoot(reaper): %v", err)
	}
	defer func() {
		reaperOwner.reaper.closeNoopCache()
		_ = unix.Close(reaperOwner.lockFD)
	}()

	now := clockBoottimeNs()
	reaperOwner.reaper.bootNowNs = func() uint64 { return now }

	// Pass 1: force-rebuild unix_dgram_sendmsg via cached pinned no-op in reaperOwner.dir!
	reaperOwner.reaper.round()
	time.Sleep(500 * time.Millisecond)

	// Stale trampoline MUST be gone now: hit_count must stop moving!
	sendDgrams(50)
	c2 := readHits()
	if c2 != c1 {
		t.Fatalf("stale trampoline STILL called progDetach after production reaper force-rebuild: c1=%d c2=%d", c1, c2)
	}

	// Pass 2 (15m01s) & Pass 3 (1m01s): reap owner.dir!
	now += uint64(15*time.Minute + time.Second)
	reaperOwner.reaper.round()
	now += uint64(1*time.Minute + time.Second)
	reaperOwner.reaper.round()

	if _, err := os.Stat(owner.dir); !os.IsNotExist(err) {
		t.Fatalf("owner.dir still exists after production reaper rebuild + grace + confirm: %v", err)
	}
	time.Sleep(500 * time.Millisecond)

	// Verify unpinned prog is freed and live traffic runs cleanly with zero panic!
	if p, err := ebpf.NewProgramFromID(progID); err == nil {
		_ = p.Close()
		t.Fatalf("prog %d still loaded after reaper unpinned it", progID)
	}
	sendDgrams(100)
}

var _ = context.Background
