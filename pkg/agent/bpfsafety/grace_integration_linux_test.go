//go:build linux && integration

package bpfsafety

import (
	"bufio"
	"context"
	"errors"
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
	"go.uber.org/zap/zaptest/observer"
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

	makeReaper := func() *graceReaper {
		r := newGraceReaper(root, reaperOwner.self, DefaultReapInterval, zap.NewNop())
		r.pinNoop = func(p *ebpf.Program, funcName string) error {
			return reaperOwner.pinProgram(p, "bpfsafety_reaper", funcName, "rb")
		}
		return r
	}

	// Pass 1 (dead owner): records stateDeadPass1 on real bpffs, does NOT reap yet.
	r1 := makeReaper()
	r1.round()
	r1.closeNoopCache()
	if _, err := os.Stat(filepath.Join(owner.dir, reaperStateFileName)); err != nil {
		t.Fatalf("expected pinned bpffs state map %s on pass 1: %v", reaperStateFileName, err)
	}
	alive2, err := ebpf.NewProgramFromID(progID)
	if err != nil {
		t.Fatalf("prog %d reaped prematurely on pass 1: %v", progID, err)
	}
	_ = alive2.Close()

	// Wait for the kernel's bpf_tramp_image_free (~1s) so kallsyms clears the
	// old trampoline image, then Pass 2+ sees image freed and reaps cleanly!
	reapDeadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(reapDeadline) {
		rN := makeReaper()
		rN.round()
		rN.closeNoopCache()
		if _, err := os.Stat(owner.dir); os.IsNotExist(err) {
			break
		}
		time.Sleep(250 * time.Millisecond)
	}

	if _, err := os.Stat(owner.dir); !os.IsNotExist(err) {
		t.Fatalf("owner.dir still exists after kallsyms image-liveness reap: %v", err)
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
	if !strings.HasPrefix(dirBase, GraceDirPrefix) {
		t.Fatalf("container grace dir %q does not use %q prefix", dirBase, GraceDirPrefix)
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

	// Verify the pin directory AND the anchor link exist on HOST /sys/fs/bpf/keploy-grace/
	// AND the BPF program is STILL LOADED in the kernel after container SIGKILL!
	if countPinnedProgs(hostGraceDir) == 0 {
		t.Fatalf("expected pinned prog on host %s after container SIGKILL, found 0", hostGraceDir)
	}
	hostAnchorLink := filepath.Join(DefaultNativeBPFFSPath, GraceRootSubdir, AnchorsSubdir, "bpf_fentry_test1"+anchorLinkSuffix)
	if _, err := os.Stat(hostAnchorLink); err != nil {
		t.Fatalf("expected anchor link %s on host bpffs to survive container SIGKILL: %v", hostAnchorLink, err)
	}
	t.Cleanup(func() {
		_ = os.RemoveAll(hostGraceDir)
		_ = CleanupAnchors(context.Background(), zap.NewNop(), DefaultNativeBPFFSPath)
	})
	alive, err := ebpf.NewProgramFromID(progID)
	if err != nil {
		t.Fatalf("BPF program %d did NOT survive container SIGKILL despite host bpffs mount: %v", progID, err)
	}
	_ = alive.Close()
}

// TestOnKernelAnchorCalmRemoval proves on a real Linux kernel:
//  1. AttachTracing creates a pinned anchor link under <root>/anchors/<func>-link
//     and <root>/anchors/<func>-prog BEFORE attaching the real program.
//  2. CleanupAnchors REFUSES to run while any live agent holds flock.
//  3. Once the live agent exits, CleanupAnchors safely reaps the dead grace dir
//     (leaving 0 leftover grace dirs on every kernel including 5.10) while
//     KEEPING the anchor -link and -prog pinned until node reboot (no Keploy
//     path ever removes an anchor's last link).
func TestOnKernelAnchorCalmRemoval(t *testing.T) {
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

	root := filepath.Join(DefaultNativeBPFFSPath, "keploy-bpfsafety-anchor-"+randomNonce())
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", root, err)
	}
	t.Cleanup(func() {
		_ = os.RemoveAll(root)
	})

	owner, err := openGraceOwnerInRoot(root, zap.NewNop())
	if err != nil {
		t.Fatalf("openGraceOwnerInRoot: %v", err)
	}
	stateMu.Lock()
	activeOwner = owner
	stateMu.Unlock()

	prog, err := ebpf.NewProgram(&ebpf.ProgramSpec{
		Name:       "anchor_test_p",
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
		t.Fatalf("load prog: %v", err)
	}
	l, err := AttachTracing(prog, "anchor_test", "bpf_fentry_test1", "p")
	if err != nil {
		_ = prog.Close()
		_ = unix.Close(owner.lockFD)
		t.Fatalf("AttachTracing: %v", err)
	}

	linkPath := filepath.Join(root, AnchorsSubdir, "bpf_fentry_test1"+anchorLinkSuffix)
	progPath := filepath.Join(root, AnchorsSubdir, "bpf_fentry_test1"+anchorProgSuffix)
	if _, err := os.Stat(linkPath); err != nil {
		t.Fatalf("expected anchor link %s created before attach: %v", linkPath, err)
	}
	if _, err := os.Stat(progPath); err != nil {
		t.Fatalf("expected anchor prog %s pinned alongside link: %v", progPath, err)
	}
	if got := CountAnchorLinks(root); got != 1 {
		t.Fatalf("CountAnchorLinks = %d, want 1", got)
	}

	// While live agent holds flock, CleanupAnchors MUST refuse!
	if err := cleanupAnchorsInRoot(context.Background(), zap.NewNop(), root, 5*time.Second, nil); err == nil {
		t.Fatal("CleanupAnchors succeeded while live agent held flock; must refuse")
	}

	// Agent exits: anchor MUST survive agent exit!
	_ = l.Close()
	_ = prog.Close()
	_ = unix.Close(owner.lockFD)
	stateMu.Lock()
	activeOwner = nil
	stateMu.Unlock()
	if got := CountAnchorLinks(root); got != 1 {
		t.Fatalf("anchor disappeared on agent exit (CountAnchorLinks=%d, want 1)", got)
	}

	// Idle cleanup MUST reap owner.dir (and on dynamic-trampoline kernels
	// 5.10.28+/5.11.11+/5.12+ with visible kallsyms leave 0 leftover grace
	// dirs; on static-trampoline kernels 5.10.0-5.10.27/5.11.0-5.11.10 at
	// most 1 mapless rebuild no-op dir may remain safely pinned) AND KEEP
	// anchor -link and -prog pinned until reboot (with
	// StaleTrampolineDetected==false)!
	if err := cleanupAnchorsInRoot(context.Background(), zap.NewNop(), root, 10*time.Second, nil); err != nil {
		t.Fatalf("idle CleanupAnchors failed: %v", err)
	}
	if StaleTrampolineDetected() {
		t.Fatal("idle CleanupAnchors wrongly raised StaleTrampolineDetected")
	}
	if _, serr := os.Stat(owner.dir); !os.IsNotExist(serr) {
		t.Fatalf("expected exited agent dir %s to be reaped, err=%v", owner.dir, serr)
	}
	if kernelHasDynamicTrampImage() && hasAnyDeadGraceDir(root) {
		ents, _ := os.ReadDir(root)
		t.Fatalf("expected 0 leftover grace dirs after idle cleanup on dynamic-trampoline kernel, found entries=%v", ents)
	}
	if _, err := os.Stat(linkPath); err != nil {
		t.Fatalf("anchor link %s was removed by CleanupAnchors (MUST stay until reboot!): %v", linkPath, err)
	}
	if _, err := os.Stat(progPath); err != nil {
		t.Fatalf("anchor prog %s was removed by CleanupAnchors (MUST stay until reboot!): %v", progPath, err)
	}
}

// TestOnKernelAnchorCleanupNeverDetachesLastLinkUnderFailslab proves on a real
// Linux debug kernel (5.10, 5.15, 5.19, 6.12) with CONFIG_FAILSLAB=y that when
// an anchor is the ONLY link on unix_dgram_sendmsg, calling cleanupAnchorsInRoot
// NEVER detaches or unpins the anchor's last link:
//  1. Zero KASAN slab-UAF / ftrace_ops_trampoline crash on 6.12 (and zero
//     static-trampoline page free on 5.10).
//  2. Returns nil with StaleTrampolineDetected()==false.
//  3. Keeps both anchor -link and -prog pinned, and 200 post-cleanup calls
//     plus a fresh attach/detach succeed with zero EBUSY.
func TestOnKernelAnchorCleanupNeverDetachesLastLinkUnderFailslab(t *testing.T) {
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

	root := filepath.Join(DefaultNativeBPFFSPath, "keploy-bpfsafety-anchor-keep-"+randomNonce())
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", root, err)
	}
	owner, err := openGraceOwnerInRoot(root, zap.NewNop())
	if err != nil {
		t.Fatalf("openGraceOwnerInRoot: %v", err)
	}
	owner.ensureAnchor("unix_dgram_sendmsg", zap.NewNop())
	_ = unix.Close(owner.lockFD)
	_ = os.Remove(owner.dir)

	linkPath := filepath.Join(root, AnchorsSubdir, "unix_dgram_sendmsg"+anchorLinkSuffix)
	progPath := filepath.Join(root, AnchorsSubdir, "unix_dgram_sendmsg"+anchorProgSuffix)
	if _, err := os.Stat(linkPath); err != nil {
		t.Fatalf("expected anchor link %s: %v", linkPath, err)
	}
	if _, err := os.Stat(progPath); err != nil {
		t.Fatalf("expected anchor prog %s: %v", progPath, err)
	}

	sendUnixDgramsHelper(t, 10)
	if err := cleanupAnchorsInRoot(context.Background(), zap.NewNop(), root, 5*time.Second, nil); err != nil {
		t.Fatalf("cleanupAnchorsInRoot = %v, want nil", err)
	}
	if StaleTrampolineDetected() {
		t.Fatal("StaleTrampolineDetected() = true after idle cleanup, want false")
	}
	if _, err := os.Stat(linkPath); err != nil {
		t.Fatalf("anchor link %s removed by cleanup (MUST stay until reboot!): %v", linkPath, err)
	}
	if _, err := os.Stat(progPath); err != nil {
		t.Fatalf("anchor prog %s removed by cleanup (MUST stay until reboot!): %v", progPath, err)
	}

	sendUnixDgramsHelper(t, 200)
	q := makeCountingFexitProg(t, "anch_reatt", "unix_dgram_sendmsg", nil)
	defer func() { _ = q.Close() }()
	lq, qerr := link.AttachTracing(link.TracingOptions{Program: q})
	if qerr != nil {
		t.Fatalf("reattach after anchor cleanup failed (EBUSY?): %v", qerr)
	}
	_ = lq.Close()
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

	// Run our production zero-timer image-liveness graceReaper:
	//   - Pass 1: marks dead owner pass-1
	//   - Pass 2: snapshots the STUCK stale trampoline address from /proc/kallsyms
	//   - Pass 3: sees the stale address STILL in /proc/kallsyms -> forces rebuild
	//     via pinned no-op fentry/unix_dgram_sendmsg in reaperOwner.dir
	//   - Pass 4+: waits until the old stale trampoline address disappears from
	//     /proc/kallsyms, THEN safely unpins and removes owner.dir!
	reaperOwner, err := openGraceOwnerInRoot(root, zap.NewNop())
	if err != nil {
		t.Fatalf("openGraceOwnerInRoot(reaper): %v", err)
	}
	defer func() {
		reaperOwner.reaper.closeNoopCache()
		_ = unix.Close(reaperOwner.lockFD)
	}()

	reaperOwner.reaper.round() // Pass 1: stateDeadPass1
	if _, err := os.Stat(owner.dir); err != nil {
		t.Fatalf("owner.dir reaped prematurely on pass 1: %v", err)
	}

	// Pass 2: snapshots stuck stale address from kallsyms AND immediately forces
	// trampoline rebuild via a pinned no-op in reaperOwner.dir (kills M3 & M5)!
	// On dynamic-trampoline kernels (5.10.28+, 5.11.11+, 5.12+), owner.dir
	// remains pinned until the old image clears from /proc/kallsyms; on
	// static-trampoline kernels (5.10.0-5.10.27, 5.11.0-5.11.10), Pass 2 forces
	// a synchronous rebuild + 2s BPF_ENABLE_STATS flat run_cnt check and may
	// already reap owner.dir.
	reaperOwner.reaper.round()
	if kernelHasDynamicTrampImage() {
		if _, err := os.Stat(owner.dir); err != nil {
			t.Fatalf("owner.dir reaped prematurely while stale trampoline image was still live: %v", err)
		}
	}
	if got := countPinnedProgs(reaperOwner.dir); got == 0 {
		t.Fatalf("expected reaper's own no-op program pinned in %s after pass-2 rebuild (kills M3!), found 0", reaperOwner.dir)
	}
	time.Sleep(500 * time.Millisecond)

	// Stale trampoline MUST be gone now: hit_count must stop moving!
	sendDgrams(50)
	c2 := readHits()
	if c2 != c1 {
		t.Fatalf("stale trampoline STILL called progDetach after production reaper force-rebuild: c1=%d c2=%d", c1, c2)
	}

	// Poll passes until old stale image address clears from /proc/kallsyms -> reaped!
	reapDeadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(reapDeadline) {
		reaperOwner.reaper.round()
		if _, err := os.Stat(owner.dir); os.IsNotExist(err) {
			break
		}
		time.Sleep(300 * time.Millisecond)
	}

	if _, err := os.Stat(owner.dir); !os.IsNotExist(err) {
		t.Fatalf("owner.dir still exists after production reaper rebuild + kallsyms clear: %v", err)
	}
	time.Sleep(500 * time.Millisecond)

	// Verify unpinned prog is freed and live traffic runs cleanly with zero panic!
	if p, err := ebpf.NewProgramFromID(progID); err == nil {
		_ = p.Close()
		t.Fatalf("prog %d still loaded after reaper unpinned it", progID)
	}
	sendDgrams(100)
}

// TestOnKernelAnchorPreventsLastLinkENOMEM proves on a real Linux debug kernel
// (5.10, 5.15, 5.19, 6.12) with CONFIG_FAILSLAB=y that when ONLY ONE real agent
// program is attached (NO auxiliary keep link — the exact production DaemonSet
// case!), the automatic host-bpffs anchor link created by PinBeforeAttach BEFORE
// first attach keeps tr->refcnt >= 1 when failslab injects -ENOMEM on l.Close():
//  1. Zero KASAN slab-UAF / ftrace_ops_trampoline crash on close(linkFD).
//  2. The host-bpffs pin keeps prog alive while the stale image still calls it.
//  3. The production reaper forces a rebuild via its own pinned no-op, replaces
//     the stale image (hit_count stops moving), waits for kallsyms clearance,
//     and unpins cleanly with zero panic on subsequent traffic.
func TestOnKernelAnchorPreventsLastLinkENOMEM(t *testing.T) {
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

	root := filepath.Join(DefaultNativeBPFFSPath, "keploy-bpfsafety-lastlink-anchor-"+randomNonce())
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", root, err)
	}
	t.Cleanup(func() {
		_ = cleanupAnchorsInRoot(context.Background(), zap.NewNop(), root, 5*time.Second, nil)
		_ = os.RemoveAll(root)
	})

	hitMap, err := ebpf.NewMap(&ebpf.MapSpec{
		Name:       "ll_enomem_hits",
		Type:       ebpf.Array,
		KeySize:    4,
		ValueSize:  8,
		MaxEntries: 1,
	})
	if err != nil {
		t.Fatalf("create hitMap: %v", err)
	}
	defer func() { _ = hitMap.Close() }()

	// ONLY ONE real agent program on unix_dgram_sendmsg (no aux progKeep!).
	progDetach, err := ebpf.NewProgram(&ebpf.ProgramSpec{
		Name:       "ll_enomem_det",
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

	lDetach, err := AttachTracing(progDetach, "ll_anchor_test", "unix_dgram_sendmsg", "p")
	if err != nil {
		_ = progDetach.Close()
		_ = unix.Close(owner.lockFD)
		t.Fatalf("AttachTracing: %v", err)
	}

	// Verify automatic anchor link was pinned BEFORE attach.
	anchorLink := filepath.Join(root, AnchorsSubdir, "unix_dgram_sendmsg"+anchorLinkSuffix)
	if _, err := os.Stat(anchorLink); err != nil {
		t.Fatalf("expected automatic anchor link %s before attach: %v", anchorLink, err)
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

	// Inject failslab -ENOMEM strictly during lDetach.Close()!
	// Because the automatic anchor link holds tr->refcnt >= 1, this MUST NOT
	// hit the last-link ftrace_ops KASAN UAF or EBUSY wedge.
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

	// Agent exits cleanly/abruptly: close prog FD and release flock.
	_ = progDetach.Close()
	_ = unix.Close(owner.lockFD)
	stateMu.Lock()
	activeOwner = nil
	stateMu.Unlock()

	time.Sleep(500 * time.Millisecond)

	// Confirm stale trampoline STILL calls progDetach (survived thanks to grace pin + anchor!).
	sendDgrams(50)
	c1 := readHits()
	if c1 <= c0 {
		t.Fatalf("expected stale trampoline to keep calling progDetach after ENOMEM unlink (c0=%d c1=%d)", c0, c1)
	}

	// Production reaper rebuilds trampoline via pinned no-op and reaps once kallsyms clears!
	reaperOwner, err := openGraceOwnerInRoot(root, zap.NewNop())
	if err != nil {
		t.Fatalf("openGraceOwnerInRoot(reaper): %v", err)
	}
	defer func() {
		reaperOwner.reaper.closeNoopCache()
		_ = unix.Close(reaperOwner.lockFD)
	}()

	reaperOwner.reaper.round() // Pass 1
	reaperOwner.reaper.round() // Pass 2: snapshot + force-rebuild (succeeds, NO EBUSY!)
	if got := countPinnedProgs(reaperOwner.dir); got == 0 {
		t.Fatalf("expected reaper no-op pinned in %s, found 0", reaperOwner.dir)
	}
	time.Sleep(500 * time.Millisecond)

	sendDgrams(50)
	c2 := readHits()
	if c2 != c1 {
		t.Fatalf("stale trampoline STILL called progDetach after anchor-protected rebuild: c1=%d c2=%d", c1, c2)
	}

	reapDeadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(reapDeadline) {
		reaperOwner.reaper.round()
		if _, err := os.Stat(owner.dir); os.IsNotExist(err) {
			break
		}
		time.Sleep(300 * time.Millisecond)
	}
	if _, err := os.Stat(owner.dir); !os.IsNotExist(err) {
		t.Fatalf("owner.dir still exists after anchor-protected rebuild + kallsyms clear: %v", err)
	}
	time.Sleep(500 * time.Millisecond)
	if p, err := ebpf.NewProgramFromID(progID); err == nil {
		_ = p.Close()
		t.Fatalf("prog %d still loaded after reaper unpinned it", progID)
	}
	sendDgrams(100)
}

func injectFailslabClose(t *testing.T, fn func()) {
	t.Helper()
	failslabDir := "/sys/kernel/debug/failslab"
	if _, err := os.Stat(failslabDir); err != nil {
		t.Skipf("requires CONFIG_FAILSLAB=y (%s missing: %v)", failslabDir, err)
	}
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	tidFailPath := fmt.Sprintf("/proc/self/task/%d/make-it-fail", unix.Gettid())
	failFD, err := unix.Open(tidFailPath, unix.O_WRONLY, 0)
	if err != nil {
		t.Fatalf("open %s: %v", tidFailPath, err)
	}
	_ = os.WriteFile(filepath.Join(failslabDir, "task-filter"), []byte("Y"), 0o644)
	_ = os.WriteFile(filepath.Join(failslabDir, "ignore-gfp-wait"), []byte("N"), 0o644)
	_ = os.WriteFile(filepath.Join(failslabDir, "probability"), []byte("100"), 0o644)
	_ = os.WriteFile(filepath.Join(failslabDir, "interval"), []byte("1"), 0o644)
	_ = os.WriteFile(filepath.Join(failslabDir, "times"), []byte("20"), 0o644)
	_ = os.WriteFile(filepath.Join(failslabDir, "space"), []byte("0"), 0o644)
	_, _ = unix.Pwrite(failFD, []byte("1\n"), 0)
	fn()
	_, _ = unix.Pwrite(failFD, []byte("0\n"), 0)
	remRaw, _ := os.ReadFile(filepath.Join(failslabDir, "times"))
	_ = unix.Close(failFD)
	_ = os.WriteFile(filepath.Join(failslabDir, "times"), []byte("0"), 0o644)
	_ = os.WriteFile(filepath.Join(failslabDir, "probability"), []byte("0"), 0o644)
	if rem, perr := strconv.Atoi(strings.TrimSpace(string(remRaw))); perr == nil && rem >= 20 {
		t.Fatalf("failslab did not inject any allocation failure during close (times remained %d)", rem)
	}
}

func makeCountingFexitProg(t *testing.T, name, target string, hitMap *ebpf.Map) *ebpf.Program {
	t.Helper()
	ins := asm.Instructions{asm.Mov.Imm(asm.R0, 0), asm.Return()}
	if hitMap != nil {
		ins = asm.Instructions{
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
		}
	}
	p, err := ebpf.NewProgram(&ebpf.ProgramSpec{
		Name: name, Type: ebpf.Tracing, AttachType: ebpf.AttachTraceFExit,
		AttachTo: target, License: "GPL", Instructions: ins,
	})
	if err != nil {
		t.Fatalf("load fexit/%s: %v", target, err)
	}
	return p
}

func sendUnixDgramsHelper(t *testing.T, n int) {
	t.Helper()
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

func releaseShutdownLockFDs() {
	stateMu.Lock()
	for _, fd := range unpinnedLockFDs {
		_ = unix.Close(fd)
	}
	unpinnedLockFDs = nil
	stateMu.Unlock()
}

// TestOnKernelCleanSnapshotRace reproduces a concurrent agent attaching on the
// same kernel function during GracefulShutdown while this agent's link close
// hits -ENOMEM. Because GracefulShutdown snapshots AFTER all detach hooks
// (union of pre and post), the newly built stale image IS captured, the reaper
// forces a rebuild instead of prematurely unpinning, and subsequent traffic
// completes with zero panic across 5.10, 5.15, 5.19, and 6.12.
func TestOnKernelCleanSnapshotRace(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("requires root/CAP_BPF")
	}
	if _, err := os.Stat("/sys/kernel/debug/failslab"); err != nil {
		t.Skip("requires CONFIG_FAILSLAB=y")
	}
	if err := rlimit.RemoveMemlock(); err != nil {
		t.Fatal(err)
	}
	var stfs unix.Statfs_t
	if err := unix.Statfs(DefaultNativeBPFFSPath, &stfs); err != nil || stfs.Type != unix.BPF_FS_MAGIC {
		t.Skipf("%s is not a bpffs mount", DefaultNativeBPFFSPath)
	}
	resetStateForTest(t)

	root := filepath.Join(DefaultNativeBPFFSPath, "keploy-bpfsafety-cleanrace-"+randomNonce())
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = cleanupAnchorsInRoot(context.Background(), zap.NewNop(), root, 5*time.Second, nil)
		_ = os.RemoveAll(root)
	})

	hitMap, err := ebpf.NewMap(&ebpf.MapSpec{Type: ebpf.Array, KeySize: 4, ValueSize: 8, MaxEntries: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = hitMap.Close() }()

	p := makeCountingFexitProg(t, "cr_prog", "unix_dgram_sendmsg", hitMap)
	owner, err := openGraceOwnerInRoot(root, zap.NewNop())
	if err != nil {
		_ = p.Close()
		t.Fatal(err)
	}
	stateMu.Lock()
	activeOwner = owner
	stateMu.Unlock()

	l, err := AttachTracing(p, "cleanrace", "unix_dgram_sendmsg", "p")
	if err != nil {
		_ = p.Close()
		_ = unix.Close(owner.lockFD)
		t.Fatal(err)
	}

	other := makeCountingFexitProg(t, "cr_other", "unix_dgram_sendmsg", nil)
	defer func() { _ = other.Close() }()
	var lo link.Link
	defer func() {
		if lo != nil {
			_ = lo.Close()
		}
	}()

	RegisterDetachHook(func() {
		// Concurrent surge agent attaches on the same function right BEFORE our
		// link close fails with -ENOMEM!
		var aerr error
		lo, aerr = link.AttachTracing(link.TracingOptions{Program: other})
		if aerr != nil {
			t.Errorf("concurrent attach: %v", aerr)
		}
		injectFailslabClose(t, func() { _ = l.Close() })
	})

	sendUnixDgramsHelper(t, 10)
	var c0 uint64
	_ = hitMap.Lookup(uint32(0), &c0)

	GracefulShutdown(context.Background(), zap.NewNop())
	_ = p.Close()
	releaseShutdownLockFDs()

	time.Sleep(300 * time.Millisecond)
	sendUnixDgramsHelper(t, 50)
	var c1 uint64
	_ = hitMap.Lookup(uint32(0), &c1)
	if c1 <= c0 {
		t.Fatalf("expected stale image to keep calling prog after ENOMEM close (c0=%d c1=%d)", c0, c1)
	}

	// Wait past the 2s clean settle window so the reaper forces a rebuild for
	// the surviving post-detach image and reaps only once the stale image clears.
	time.Sleep(2500 * time.Millisecond)
	ro, err := openGraceOwnerInRoot(root, zap.NewNop())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { ro.reaper.closeNoopCache(); _ = unix.Close(ro.lockFD) }()

	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		ro.reaper.round()
		if _, err := os.Stat(owner.dir); os.IsNotExist(err) {
			break
		}
		time.Sleep(300 * time.Millisecond)
	}
	if _, err := os.Stat(owner.dir); !os.IsNotExist(err) {
		t.Fatalf("owner.dir still exists after clean-race forced rebuild + reap")
	}
	time.Sleep(500 * time.Millisecond)
	sendUnixDgramsHelper(t, 200)
}

// TestOnKernelCleanSnapshotTwoProgsSameFunc reproduces a single agent detaching
// two programs on the same kernel function (e.g. fentry+fexit or primary+backup)
// where the first close succeeds cleanly (building a new trampoline image still
// calling prog 2) and the second close hits -ENOMEM.
func TestOnKernelCleanSnapshotTwoProgsSameFunc(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("requires root/CAP_BPF")
	}
	if _, err := os.Stat("/sys/kernel/debug/failslab"); err != nil {
		t.Skip("requires CONFIG_FAILSLAB=y")
	}
	if err := rlimit.RemoveMemlock(); err != nil {
		t.Fatal(err)
	}
	var stfs unix.Statfs_t
	if err := unix.Statfs(DefaultNativeBPFFSPath, &stfs); err != nil || stfs.Type != unix.BPF_FS_MAGIC {
		t.Skipf("%s is not a bpffs mount", DefaultNativeBPFFSPath)
	}
	resetStateForTest(t)

	root := filepath.Join(DefaultNativeBPFFSPath, "keploy-bpfsafety-twoprog-"+randomNonce())
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = cleanupAnchorsInRoot(context.Background(), zap.NewNop(), root, 5*time.Second, nil)
		_ = os.RemoveAll(root)
	})

	hitMap, err := ebpf.NewMap(&ebpf.MapSpec{Type: ebpf.Array, KeySize: 4, ValueSize: 8, MaxEntries: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = hitMap.Close() }()

	p1 := makeCountingFexitProg(t, "tp_first", "unix_dgram_sendmsg", nil)
	p2 := makeCountingFexitProg(t, "tp_second", "unix_dgram_sendmsg", hitMap)
	owner, err := openGraceOwnerInRoot(root, zap.NewNop())
	if err != nil {
		_ = p1.Close()
		_ = p2.Close()
		t.Fatal(err)
	}
	stateMu.Lock()
	activeOwner = owner
	stateMu.Unlock()

	l1, err := AttachTracing(p1, "twoprog", "unix_dgram_sendmsg", "b")
	if err != nil {
		_ = p1.Close()
		_ = p2.Close()
		_ = unix.Close(owner.lockFD)
		t.Fatal(err)
	}
	l2, err := AttachTracing(p2, "twoprog", "unix_dgram_sendmsg", "p")
	if err != nil {
		_ = l1.Close()
		_ = p1.Close()
		_ = p2.Close()
		_ = unix.Close(owner.lockFD)
		t.Fatal(err)
	}

	RegisterDetachHook(func() {
		_ = l1.Close() // clean close builds a NEW trampoline image still calling p2!
		injectFailslabClose(t, func() { _ = l2.Close() })
	})

	sendUnixDgramsHelper(t, 10)
	var c0 uint64
	_ = hitMap.Lookup(uint32(0), &c0)

	GracefulShutdown(context.Background(), zap.NewNop())
	_ = p1.Close()
	_ = p2.Close()
	releaseShutdownLockFDs()

	time.Sleep(300 * time.Millisecond)
	sendUnixDgramsHelper(t, 50)
	var c1 uint64
	_ = hitMap.Lookup(uint32(0), &c1)
	if c1 <= c0 {
		t.Fatalf("expected stale image to keep calling p2 after second-link ENOMEM (c0=%d c1=%d)", c0, c1)
	}

	time.Sleep(2500 * time.Millisecond)
	ro, err := openGraceOwnerInRoot(root, zap.NewNop())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { ro.reaper.closeNoopCache(); _ = unix.Close(ro.lockFD) }()

	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		ro.reaper.round()
		if _, err := os.Stat(owner.dir); os.IsNotExist(err) {
			break
		}
		time.Sleep(300 * time.Millisecond)
	}
	if _, err := os.Stat(owner.dir); !os.IsNotExist(err) {
		t.Fatalf("owner.dir still exists after two-prog clean-race rebuild + reap")
	}
	time.Sleep(500 * time.Millisecond)
	sendUnixDgramsHelper(t, 200)
}

// TestOnKernelUnresolvedNameFailsClosed proves BOTH layers of fail-closed
// defense on Linux 5.10..5.19 (where bpf_prog_info omits attach_btf_id):
//  1. AttachTracing REFUSES at pin time (ErrUnresolvedTarget) with 0 leaked
//     pins when caller passes a non-kernel-symbol name.
//  2. If a dead grace directory contains a manually/legacy-pinned Tracing
//     program with no per-pin target entry, the reaper FAILS CLOSED and keeps
//     the directory pinned (kills M13).
func TestOnKernelUnresolvedNameFailsClosed(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("requires root/CAP_BPF")
	}
	if err := rlimit.RemoveMemlock(); err != nil {
		t.Fatal(err)
	}
	var stfs unix.Statfs_t
	if err := unix.Statfs(DefaultNativeBPFFSPath, &stfs); err != nil || stfs.Type != unix.BPF_FS_MAGIC {
		t.Skipf("%s is not a bpffs mount", DefaultNativeBPFFSPath)
	}
	resetStateForTest(t)

	root := filepath.Join(DefaultNativeBPFFSPath, "keploy-bpfsafety-unres-"+randomNonce())
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })

	p := makeCountingFexitProg(t, "unres_p", "unix_stream_connect", nil)
	info, err := queryProgInfo(p.FD())
	if err != nil {
		_ = p.Close()
		t.Fatal(err)
	}
	if info.AttachBtfID != 0 {
		_ = p.Close()
		t.Skip("Linux 6.0+ populates attach_btf_id directly; pre-6.0 unresolved fail-closed check applies to 5.10..5.19")
	}

	owner, err := openGraceOwnerInRoot(root, zap.NewNop())
	if err != nil {
		_ = p.Close()
		t.Fatal(err)
	}
	stateMu.Lock()
	activeOwner = owner
	stateMu.Unlock()

	// Layer 1: pinProgram / AttachTracing MUST refuse with ErrUnresolvedTarget and leak 0 pins.
	if _, err := AttachTracing(p, "unres_test", "not_a_real_kernel_func_name", "p"); !errors.Is(err, ErrUnresolvedTarget) {
		_ = p.Close()
		_ = unix.Close(owner.lockFD)
		t.Fatalf("AttachTracing(non-kernel name) = %v, want ErrUnresolvedTarget", err)
	}
	if got := countPinnedProgs(owner.dir); got != 0 {
		_ = p.Close()
		_ = unix.Close(owner.lockFD)
		t.Fatalf("expected 0 leaked pins after ErrUnresolvedTarget, got %d", got)
	}

	// Layer 2: simulate a legacy/manually-pinned unresolved prog in owner.dir;
	// reaper MUST fail closed and keep owner.dir pinned.
	if err := p.Pin(filepath.Join(owner.dir, "000001-legacy-unresolved-p")); err != nil {
		_ = p.Close()
		_ = unix.Close(owner.lockFD)
		t.Fatal(err)
	}
	_ = p.Close()
	_ = unix.Close(owner.lockFD)
	stateMu.Lock()
	activeOwner = nil
	stateMu.Unlock()

	ro, err := openGraceOwnerInRoot(root, zap.NewNop())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { ro.reaper.closeNoopCache(); _ = unix.Close(ro.lockFD) }()

	for i := 0; i < 4; i++ {
		ro.reaper.round()
		time.Sleep(250 * time.Millisecond)
	}
	if _, err := os.Stat(owner.dir); err != nil {
		t.Fatalf("unresolved pre-6.0 Tracing dir was prematurely reaped (M13 fail-open!): %v", err)
	}
}

func connectUnixStreamHelper(t *testing.T, n int) {
	t.Helper()
	srv, err := unix.Socket(unix.AF_UNIX, unix.SOCK_STREAM, 0)
	if err != nil {
		t.Fatalf("socket srv: %v", err)
	}
	defer func() { _ = unix.Close(srv) }()
	sp := filepath.Join(t.TempDir(), "s.sock")
	sa := &unix.SockaddrUnix{Name: sp}
	if err := unix.Bind(srv, sa); err != nil {
		t.Fatalf("bind: %v", err)
	}
	if err := unix.Listen(srv, 64); err != nil {
		t.Fatalf("listen: %v", err)
	}
	for i := 0; i < n; i++ {
		c, cerr := unix.Socket(unix.AF_UNIX, unix.SOCK_STREAM, 0)
		if cerr != nil {
			t.Fatalf("socket cli: %v", cerr)
		}
		if err := unix.Connect(c, sa); err != nil {
			_ = unix.Close(c)
			t.Fatalf("connect: %v", err)
		}
		if a, _, aerr := unix.Accept(srv); aerr == nil {
			_ = unix.Close(a)
		}
		_ = unix.Close(c)
	}
}

// runMixedDirUnresolvedProg proves BOTH layers of per-program fail-closed
// defense on Linux 5.10..5.19:
//  1. AttachTracing refuses an unresolved name with ErrUnresolvedTarget even
//     after a resolved prog is already in the same grace dir.
//  2. If a grace dir nonetheless holds a resolved prog A (unix_dgram_sendmsg)
//     AND an unresolved/legacy prog B (unix_stream_connect whose close hits
//     -ENOMEM), the reaper fails closed PER PROGRAM and keeps the WHOLE dir
//     pinned (both dead-owner and clean-exit paths), so 500 subsequent
//     connect() calls complete with zero kernel panic!
func runMixedDirUnresolvedProg(t *testing.T, clean bool) {
	if os.Geteuid() != 0 {
		t.Skip("requires root/CAP_BPF")
	}
	if _, err := os.Stat("/sys/kernel/debug/failslab"); err != nil {
		t.Skip("requires CONFIG_FAILSLAB=y")
	}
	if err := rlimit.RemoveMemlock(); err != nil {
		t.Fatal(err)
	}
	var stfs unix.Statfs_t
	if err := unix.Statfs(DefaultNativeBPFFSPath, &stfs); err != nil || stfs.Type != unix.BPF_FS_MAGIC {
		t.Skipf("%s is not a bpffs mount", DefaultNativeBPFFSPath)
	}
	resetStateForTest(t)

	pb := makeCountingFexitProg(t, "mix_b", "unix_stream_connect", nil)
	info, err := queryProgInfo(pb.FD())
	if err != nil {
		_ = pb.Close()
		t.Fatal(err)
	}
	if info.AttachBtfID != 0 {
		_ = pb.Close()
		t.Skip("Linux 6.0+ populates attach_btf_id directly; pre-6.0 mixed-dir check applies to 5.10..5.19")
	}
	_ = pb.Close()

	root := filepath.Join(DefaultNativeBPFFSPath, fmt.Sprintf("keploy-bpfsafety-mix-%v-%s", clean, randomNonce()))
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}

	hitB, err := ebpf.NewMap(&ebpf.MapSpec{Type: ebpf.Array, KeySize: 4, ValueSize: 8, MaxEntries: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = hitB.Close() }()

	pa := makeCountingFexitProg(t, "mix_a", "unix_dgram_sendmsg", nil)
	pb = makeCountingFexitProg(t, "mix_b", "unix_stream_connect", hitB)
	pbInfo, _ := pb.Info()
	pbID, _ := pbInfo.ID()

	owner, err := openGraceOwnerInRoot(root, zap.NewNop())
	if err != nil {
		_ = pa.Close()
		_ = pb.Close()
		t.Fatal(err)
	}
	stateMu.Lock()
	activeOwner = owner
	stateMu.Unlock()

	la, err := AttachTracing(pa, "mix_test", "unix_dgram_sendmsg", "p")
	if err != nil {
		_ = pa.Close()
		_ = pb.Close()
		_ = unix.Close(owner.lockFD)
		t.Fatal(err)
	}
	// Layer 1: AttachTracing MUST refuse unresolved target on 5.x.
	if _, err := AttachTracing(pb, "mix_test", "keploy_stream_connect", "p"); !errors.Is(err, ErrUnresolvedTarget) {
		_ = la.Close()
		_ = pa.Close()
		_ = pb.Close()
		_ = unix.Close(owner.lockFD)
		t.Fatalf("AttachTracing(unresolved B alongside resolved A) = %v, want ErrUnresolvedTarget", err)
	}

	// Layer 2: simulate a legacy/manually-pinned unresolved prog B in owner.dir
	// alongside resolved prog A, with another link keeping tr->refcnt >= 1.
	other := makeCountingFexitProg(t, "mix_other", "unix_stream_connect", nil)
	defer func() { _ = other.Close() }()
	lo, err := link.AttachTracing(link.TracingOptions{Program: other})
	if err != nil {
		_ = la.Close()
		_ = pa.Close()
		_ = pb.Close()
		_ = unix.Close(owner.lockFD)
		t.Fatal(err)
	}
	defer func() { _ = lo.Close() }()

	if err := pb.Pin(filepath.Join(owner.dir, "000002-legacy-mix-b-p")); err != nil {
		_ = la.Close()
		_ = pa.Close()
		_ = pb.Close()
		_ = unix.Close(owner.lockFD)
		t.Fatal(err)
	}
	lb, err := link.AttachTracing(link.TracingOptions{Program: pb})
	if err != nil {
		_ = la.Close()
		_ = pa.Close()
		_ = pb.Close()
		_ = unix.Close(owner.lockFD)
		t.Fatal(err)
	}

	connectUnixStreamHelper(t, 10)
	var c0 uint64
	_ = hitB.Lookup(uint32(0), &c0)

	if clean {
		RegisterDetachHook(func() { _ = la.Close() })
		RegisterDetachHook(func() { injectFailslabClose(t, func() { _ = lb.Close() }) })
		GracefulShutdown(context.Background(), zap.NewNop())
		releaseShutdownLockFDs()
	} else {
		_ = la.Close()
		injectFailslabClose(t, func() { _ = lb.Close() })
		_ = unix.Close(owner.lockFD)
		stateMu.Lock()
		activeOwner = nil
		stateMu.Unlock()
	}
	_ = pa.Close()
	_ = pb.Close()

	time.Sleep(300 * time.Millisecond)
	connectUnixStreamHelper(t, 50)
	var c1 uint64
	_ = hitB.Lookup(uint32(0), &c1)
	if c1 <= c0 {
		t.Fatalf("expected stale image to keep calling B (c0=%d c1=%d)", c0, c1)
	}

	ro, err := openGraceOwnerInRoot(root, zap.NewNop())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { ro.reaper.closeNoopCache(); _ = unix.Close(ro.lockFD) }()
	if clean {
		time.Sleep(2500 * time.Millisecond)
	}
	for i := 0; i < 10; i++ {
		ro.reaper.round()
		time.Sleep(250 * time.Millisecond)
	}
	if _, err := os.Stat(owner.dir); err != nil {
		t.Fatalf("mixed dir with unresolved prog B was prematurely reaped (clean=%v): %v", clean, err)
	}
	if pAlive, aerr := ebpf.NewProgramFromID(pbID); aerr != nil {
		t.Fatalf("unresolved prog B %d was freed despite per-prog fail-closed rule: %v", pbID, aerr)
	} else {
		_ = pAlive.Close()
	}
	// 500 connect() calls MUST complete cleanly with zero kernel panic!
	connectUnixStreamHelper(t, 500)
}

func TestOnKernelMixedDirUnresolvedProgDead(t *testing.T)  { runMixedDirUnresolvedProg(t, false) }
func TestOnKernelMixedDirUnresolvedProgClean(t *testing.T) { runMixedDirUnresolvedProg(t, true) }

// TestOnKernelCleanupAnchorsIdleWithDeadDirAndOtherLink proves across all 4
// kernels (5.10, 5.15, 5.19, 6.12):
//  1. A dead dir with an -ENOMEM stale image is safely reaped by
//     cleanupAnchorsInRoot with ZERO leftover grace2-* / tmp directories
//     (including on Linux 5.10).
//  2. The anchor link & prog STAY pinned (never detached).
//  3. Even when another unrelated tracing link remains attached to the same
//     function, cleanupAnchorsInRoot returns nil and does NOT raise a false
//     StaleTrampolineDetected() alert!
func TestOnKernelCleanupAnchorsIdleWithDeadDirAndOtherLink(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("requires root/CAP_BPF")
	}
	if _, err := os.Stat("/sys/kernel/debug/failslab"); err != nil {
		t.Skip("requires CONFIG_FAILSLAB=y")
	}
	if err := rlimit.RemoveMemlock(); err != nil {
		t.Fatal(err)
	}
	var stfs unix.Statfs_t
	if err := unix.Statfs(DefaultNativeBPFFSPath, &stfs); err != nil || stfs.Type != unix.BPF_FS_MAGIC {
		t.Skipf("%s is not a bpffs mount", DefaultNativeBPFFSPath)
	}
	resetStateForTest(t)

	root := filepath.Join(DefaultNativeBPFFSPath, "keploy-bpfsafety-idle-"+randomNonce())
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}

	other := makeCountingFexitProg(t, "idle_other", "unix_dgram_sendmsg", nil)
	defer func() { _ = other.Close() }()
	lo, err := link.AttachTracing(link.TracingOptions{Program: other})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = lo.Close() }()

	hitMap, err := ebpf.NewMap(&ebpf.MapSpec{Type: ebpf.Array, KeySize: 4, ValueSize: 8, MaxEntries: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = hitMap.Close() }()

	p := makeCountingFexitProg(t, "idle_p", "unix_dgram_sendmsg", hitMap)
	owner, err := openGraceOwnerInRoot(root, zap.NewNop())
	if err != nil {
		_ = p.Close()
		t.Fatal(err)
	}
	stateMu.Lock()
	activeOwner = owner
	stateMu.Unlock()

	l, err := AttachTracing(p, "idle_test", "unix_dgram_sendmsg", "p")
	if err != nil {
		_ = p.Close()
		_ = unix.Close(owner.lockFD)
		t.Fatal(err)
	}
	injectFailslabClose(t, func() { _ = l.Close() })
	_ = p.Close()
	_ = unix.Close(owner.lockFD)
	stateMu.Lock()
	activeOwner = nil
	stateMu.Unlock()

	sendUnixDgramsHelper(t, 20)
	var c1 uint64
	_ = hitMap.Lookup(uint32(0), &c1)

	if err := cleanupAnchorsInRoot(context.Background(), zap.NewNop(), root, 15*time.Second, nil); err != nil {
		t.Fatalf("cleanupAnchorsInRoot with dead ENOMEM dir + other link = %v, want nil", err)
	}
	if StaleTrampolineDetected() {
		t.Fatal("cleanupAnchorsInRoot raised false StaleTrampolineDetected with another tracing link attached")
	}
	if _, serr := os.Stat(owner.dir); !os.IsNotExist(serr) {
		t.Fatalf("dead ENOMEM dir %s was not reaped by cleanupAnchorsInRoot: err=%v", owner.dir, serr)
	}
	if kernelHasDynamicTrampImage() && hasAnyDeadGraceDir(root) {
		ents, _ := os.ReadDir(root)
		t.Fatalf("leftover grace dir(s) remained after cleanupAnchorsInRoot on 5.12+: %v", ents)
	}
	if got := CountAnchorLinks(root); got != 1 {
		t.Fatalf("CountAnchorLinks = %d, want 1 (anchor MUST stay pinned until reboot)", got)
	}
	time.Sleep(500 * time.Millisecond)
	sendUnixDgramsHelper(t, 200)
	var c2 uint64
	_ = hitMap.Lookup(uint32(0), &c2)
	if c2 != c1 {
		t.Fatalf("stale image still called prog after idle cleanup: c1=%d c2=%d", c1, c2)
	}
}

// TestOnKernelIdleTmpNoopENOMEMNeverPanics proves across all 4 debug kernels
// (5.10, 5.15, 5.19, 6.12) that if `keploy agent cleanup`'s temporary rebuild
// no-op N hits -ENOMEM on link close while the hooked function is idle (no
// traffic during cleanup), reapDeadDirsIdle NEVER unpins N on flat run_cnt
// alone — it keeps N safely pinned and 1,000 subsequent calls complete with
// zero page fault / KASAN / panic.
func TestOnKernelIdleTmpNoopENOMEMNeverPanics(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("requires root/CAP_BPF")
	}
	if _, err := os.Stat("/sys/kernel/debug/failslab"); err != nil {
		t.Skip("requires CONFIG_FAILSLAB=y")
	}
	if err := rlimit.RemoveMemlock(); err != nil {
		t.Fatal(err)
	}
	var stfs unix.Statfs_t
	if err := unix.Statfs(DefaultNativeBPFFSPath, &stfs); err != nil || stfs.Type != unix.BPF_FS_MAGIC {
		t.Skipf("%s is not a bpffs mount", DefaultNativeBPFFSPath)
	}
	resetStateForTest(t)

	root := filepath.Join(DefaultNativeBPFFSPath, "keploy-bpfsafety-tmp-enomem-"+randomNonce())
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}

	hitMap, err := ebpf.NewMap(&ebpf.MapSpec{Type: ebpf.Array, KeySize: 4, ValueSize: 8, MaxEntries: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = hitMap.Close() }()

	p := makeCountingFexitProg(t, "tmp_enomem_p", "unix_dgram_sendmsg", hitMap)
	owner, err := openGraceOwnerInRoot(root, zap.NewNop())
	if err != nil {
		_ = p.Close()
		t.Fatal(err)
	}
	stateMu.Lock()
	activeOwner = owner
	stateMu.Unlock()

	l, err := AttachTracing(p, "tmp_enomem", "unix_dgram_sendmsg", "p")
	if err != nil {
		_ = p.Close()
		_ = unix.Close(owner.lockFD)
		t.Fatal(err)
	}
	injectFailslabClose(t, func() { _ = l.Close() })
	_ = p.Close()
	_ = unix.Close(owner.lockFD)
	stateMu.Lock()
	activeOwner = nil
	stateMu.Unlock()

	sendUnixDgramsHelper(t, 20)
	var c1 uint64
	_ = hitMap.Lookup(uint32(0), &c1)

	// Arm failslab around every rebuild no-op link close during idle cleanup,
	// with ZERO unix_dgram_sendmsg traffic while cleanup runs.
	noopCloses := 0
	hooks := &anchorCleanupHooks{
		wrapNoopClose: func(closeFn func()) {
			noopCloses++
			injectFailslabClose(t, closeFn)
		},
	}
	if err := cleanupAnchorsInRoot(context.Background(), zap.NewNop(), root, 8*time.Second, hooks); err != nil {
		t.Fatalf("cleanupAnchorsInRoot = %v, want nil", err)
	}
	if noopCloses == 0 {
		t.Fatal("expected cleanupAnchorsInRoot to run at least one rebuild no-op close")
	}
	if !hasAnyDeadGraceDir(root) {
		t.Fatal("reapDeadDirsIdle unpinned tmp rebuild no-op despite -ENOMEM on its close (would panic on next call!)")
	}

	time.Sleep(1 * time.Second)
	sendUnixDgramsHelper(t, 1000)
}

// TestOnKernelAnchorPinFailNeverClosesLastLink proves across all 4 debug
// kernels (5.10, 5.15, 5.19, 6.12) that if l.Pin(linkPath) fails with -ENOMEM
// right after ensureAnchor attaches the function's ONLY link:
//  1. ensureAnchor NEVER closes l (zero 6.12 KASAN slab-UAF in
//     ftrace_ops_trampoline and zero 5.10 trampoline page fault).
//  2. 1,000 subsequent unix_dgram_sendmsg calls complete cleanly.
//  3. Once memory pressure clears, the next reaper round retries pinning and
//     pins unix_dgram_sendmsg-link onto host bpffs!
func TestOnKernelAnchorPinFailNeverClosesLastLink(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("requires root/CAP_BPF")
	}
	failslabDir := "/sys/kernel/debug/failslab"
	if _, err := os.Stat(failslabDir); err != nil {
		t.Skipf("requires CONFIG_FAILSLAB=y (%s missing: %v)", failslabDir, err)
	}
	if err := rlimit.RemoveMemlock(); err != nil {
		t.Fatal(err)
	}
	var stfs unix.Statfs_t
	if err := unix.Statfs(DefaultNativeBPFFSPath, &stfs); err != nil || stfs.Type != unix.BPF_FS_MAGIC {
		t.Skipf("%s is not a bpffs mount", DefaultNativeBPFFSPath)
	}
	resetStateForTest(t)

	root := filepath.Join(DefaultNativeBPFFSPath, "keploy-bpfsafety-apinfail-"+randomNonce())
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	owner, err := openGraceOwnerInRoot(root, zap.NewNop())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = unix.Close(owner.lockFD) }()

	// Use bpf_fentry_test3 so no earlier test has pinned or attached a link on it
	// (guaranteeing this anchor is the trampoline's true ONLY link).
	const fn = "bpf_fentry_test3"
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

	hookRan := false
	owner.onBeforeAnchorPin = func() {
		hookRan = true
		_, _ = unix.Pwrite(failFD, []byte("1\n"), 0)
	}
	owner.ensureAnchor(fn, zap.NewNop())
	_, _ = unix.Pwrite(failFD, []byte("0\n"), 0)
	remRaw, _ := os.ReadFile(filepath.Join(failslabDir, "times"))
	_ = unix.Close(failFD)
	_ = os.WriteFile(filepath.Join(failslabDir, "times"), []byte("0"), 0o644)
	_ = os.WriteFile(filepath.Join(failslabDir, "probability"), []byte("0"), 0o644)
	runtime.UnlockOSThread()
	owner.onBeforeAnchorPin = nil

	if !hookRan {
		t.Fatal("onBeforeAnchorPin hook did not run")
	}
	if rem, perr := strconv.Atoi(strings.TrimSpace(string(remRaw))); perr == nil && rem >= 20 {
		t.Fatalf("failslab did not inject -ENOMEM during anchor link pin (times remained %d)", rem)
	}
	if got := CountAnchorLinks(root); got != 0 {
		t.Fatalf("expected 0 pinned anchor links immediately after ENOMEM pin failure, got %d", got)
	}
	owner.anchorMu.Lock()
	pending := len(owner.unpinnedAnchors)
	owner.anchorMu.Unlock()
	if pending != 1 {
		t.Fatalf("expected 1 retained unpinned anchor link FD on owner, got %d", pending)
	}

	// Next reaper round (after memory pressure passes) MUST pin the retained anchor link!
	owner.reaper.round()
	if got := CountAnchorLinks(root); got != 1 {
		t.Fatalf("expected deferred anchor link to be pinned on reaper retry, CountAnchorLinks=%d", got)
	}
	owner.anchorMu.Lock()
	pendingAfter := len(owner.unpinnedAnchors)
	owner.anchorMu.Unlock()
	if pendingAfter != 0 {
		t.Fatalf("expected 0 pending unpinned anchors after retry, got %d", pendingAfter)
	}
}

func pinFailAnchorForTest(t *testing.T, owner *graceOwner, fn string) {
	t.Helper()
	failslabDir := "/sys/kernel/debug/failslab"
	if _, err := os.Stat(failslabDir); err != nil {
		t.Skipf("requires CONFIG_FAILSLAB=y (%s missing: %v)", failslabDir, err)
	}
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	tidFailPath := fmt.Sprintf("/proc/self/task/%d/make-it-fail", unix.Gettid())
	failFD, err := unix.Open(tidFailPath, unix.O_WRONLY, 0)
	if err != nil {
		t.Fatalf("open %s: %v", tidFailPath, err)
	}
	_ = os.WriteFile(filepath.Join(failslabDir, "task-filter"), []byte("Y"), 0o644)
	_ = os.WriteFile(filepath.Join(failslabDir, "ignore-gfp-wait"), []byte("N"), 0o644)
	_ = os.WriteFile(filepath.Join(failslabDir, "probability"), []byte("100"), 0o644)
	_ = os.WriteFile(filepath.Join(failslabDir, "interval"), []byte("1"), 0o644)
	_ = os.WriteFile(filepath.Join(failslabDir, "times"), []byte("20"), 0o644)
	_ = os.WriteFile(filepath.Join(failslabDir, "space"), []byte("0"), 0o644)

	hookRan := false
	owner.onBeforeAnchorPin = func() {
		hookRan = true
		_, _ = unix.Pwrite(failFD, []byte("1\n"), 0)
	}
	owner.ensureAnchor(fn, zap.NewNop())
	_, _ = unix.Pwrite(failFD, []byte("0\n"), 0)
	remRaw, _ := os.ReadFile(filepath.Join(failslabDir, "times"))
	_ = unix.Close(failFD)
	_ = os.WriteFile(filepath.Join(failslabDir, "times"), []byte("0"), 0o644)
	_ = os.WriteFile(filepath.Join(failslabDir, "probability"), []byte("0"), 0o644)
	owner.onBeforeAnchorPin = nil

	if !hookRan {
		t.Fatal("onBeforeAnchorPin hook did not run")
	}
	if rem, perr := strconv.Atoi(strings.TrimSpace(string(remRaw))); perr == nil && rem >= 20 {
		t.Fatalf("failslab did not inject -ENOMEM during anchor link pin (times remained %d)", rem)
	}
}

func pendingAnchorState(owner *graceOwner) (int, link.ID) {
	owner.anchorMu.Lock()
	defer owner.anchorMu.Unlock()
	var id link.ID
	for _, p := range owner.unpinnedAnchors {
		if p.lnk == nil {
			continue
		}
		if info, err := p.lnk.Info(); err == nil {
			id = info.ID
		}
	}
	return len(owner.unpinnedAnchors), id
}

func linkIDAlive(id link.ID) bool {
	if id == 0 {
		return false
	}
	l, err := link.NewFromID(id)
	if err != nil {
		return false
	}
	_ = l.Close()
	return true
}

func sendUnixStreamHelper(t *testing.T, n int) {
	t.Helper()
	fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM, 0)
	if err != nil {
		t.Fatalf("socketpair stream: %v", err)
	}
	defer func() {
		_ = unix.Close(fds[0])
		_ = unix.Close(fds[1])
	}()
	buf := make([]byte, 8)
	for i := 0; i < n; i++ {
		if _, err := unix.Write(fds[0], []byte("x")); err != nil {
			t.Fatalf("stream write: %v", err)
		}
		if _, err := unix.Read(fds[1], buf); err != nil {
			t.Fatalf("stream read: %v", err)
		}
	}
}

func sendUnixSeqpacketHelper(t *testing.T, n int) {
	t.Helper()
	fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_SEQPACKET, 0)
	if err != nil {
		t.Fatalf("socketpair seqpacket: %v", err)
	}
	defer func() {
		_ = unix.Close(fds[0])
		_ = unix.Close(fds[1])
	}()
	buf := make([]byte, 8)
	for i := 0; i < n; i++ {
		if err := unix.Send(fds[0], []byte("x"), 0); err != nil {
			t.Fatalf("seqpacket send: %v", err)
		}
		if _, _, err := unix.Recvfrom(fds[1], buf, 0); err != nil {
			t.Fatalf("seqpacket recv: %v", err)
		}
	}
}

// TestOnKernelAnchorPinRetryFailKeepsLink proves that when ensureAnchor's
// initial l.Pin fails under memory pressure AND a subsequent reaper retry ALSO
// fails under failslab, retryUnpinnedAnchors keeps the live link FD open
// (never closing the function's only link under pressure, which would panic
// 5.10 and KASAN 6.12), traffic survives cleanly, and a later clean retry pins
// the anchor onto host bpffs.
func TestOnKernelAnchorPinRetryFailKeepsLink(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("requires root/CAP_BPF")
	}
	if _, err := os.Stat("/sys/kernel/debug/failslab"); err != nil {
		t.Skipf("requires CONFIG_FAILSLAB=y: %v", err)
	}
	if err := rlimit.RemoveMemlock(); err != nil {
		t.Fatal(err)
	}
	var stfs unix.Statfs_t
	if err := unix.Statfs(DefaultNativeBPFFSPath, &stfs); err != nil || stfs.Type != unix.BPF_FS_MAGIC {
		t.Skipf("%s is not a bpffs mount", DefaultNativeBPFFSPath)
	}
	resetStateForTest(t)

	root := filepath.Join(DefaultNativeBPFFSPath, "keploy-bpfsafety-apretry-"+randomNonce())
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	owner, err := openGraceOwnerInRoot(root, zap.NewNop())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = unix.Close(owner.lockFD) }()

	// Use unix_stream_sendmsg (distinct from every other test in this boot so
	// this anchor is the function's true ONLY link).
	const fn = "unix_stream_sendmsg"
	sendUnixStreamHelper(t, 10)
	pinFailAnchorForTest(t, owner, fn)

	n, id := pendingAnchorState(owner)
	if n != 1 || id == 0 || !linkIDAlive(id) {
		t.Fatalf("initial pin failure did not retain live anchor link (pending=%d id=%d alive=%v)", n, id, linkIDAlive(id))
	}
	if got := CountAnchorLinks(root); got != 0 {
		t.Fatalf("expected 0 pinned anchors after initial pin failure, got %d", got)
	}

	// Keep failslab armed across the retry: retryUnpinnedAnchors MUST retain
	// the live link FD rather than closing the function's only link.
	injectFailslabClose(t, func() { owner.retryUnpinnedAnchors() })

	n2, _ := pendingAnchorState(owner)
	alive := linkIDAlive(id)
	time.Sleep(1500 * time.Millisecond)
	for i := 0; i < 10; i++ {
		sendUnixStreamHelper(t, 100)
		time.Sleep(50 * time.Millisecond)
	}
	if n2 != 1 || !alive {
		t.Fatalf("failed retry dropped the anchor link (pending=%d alive=%v)", n2, alive)
	}

	// Once memory pressure clears, the next retry MUST pin the retained link.
	owner.retryUnpinnedAnchors()
	n3, _ := pendingAnchorState(owner)
	if n3 != 0 || CountAnchorLinks(root) != 1 || !linkIDAlive(id) {
		t.Fatalf("clean retry did not pin anchor link (pending=%d anchorLinks=%d alive=%v)", n3, CountAnchorLinks(root), linkIDAlive(id))
	}
}

// TestOnKernelAnchorEMFILEKeepsLink proves that when an anchor is already
// pinned on host bpffs and a second owner's ensureAnchor hits EMFILE in
// LoadPinnedLink, ensureAnchor leaves linkPath untouched and the pinned link
// stays alive.
func TestOnKernelAnchorEMFILEKeepsLink(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("requires root/CAP_BPF")
	}
	if err := rlimit.RemoveMemlock(); err != nil {
		t.Fatal(err)
	}
	var stfs unix.Statfs_t
	if err := unix.Statfs(DefaultNativeBPFFSPath, &stfs); err != nil || stfs.Type != unix.BPF_FS_MAGIC {
		t.Skipf("%s is not a bpffs mount", DefaultNativeBPFFSPath)
	}
	resetStateForTest(t)

	root := filepath.Join(DefaultNativeBPFFSPath, "keploy-bpfsafety-aemfile-"+randomNonce())
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	a, err := openGraceOwnerInRoot(root, zap.NewNop())
	if err != nil {
		t.Fatal(err)
	}

	const fn = "bpf_fentry_test4"
	a.ensureAnchor(fn, zap.NewNop())
	linkPath := filepath.Join(root, AnchorsSubdir, sanitizeToken(fn)+anchorLinkSuffix)
	l0, err := link.LoadPinnedLink(linkPath, nil)
	if err != nil {
		_ = unix.Close(a.lockFD)
		t.Fatalf("first anchor not pinned at %s: %v", linkPath, err)
	}
	info, err := l0.Info()
	_ = l0.Close()
	_ = unix.Close(a.lockFD)
	if err != nil || info.ID == 0 {
		t.Fatalf("read first anchor link info: id=%d err=%v", info.ID, err)
	}
	id := info.ID

	b, err := openGraceOwnerInRoot(root, zap.NewNop())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = unix.Close(b.lockFD) }()

	var old unix.Rlimit
	if err := unix.Getrlimit(unix.RLIMIT_NOFILE, &old); err != nil {
		t.Fatal(err)
	}
	probe, err := unix.Open("/dev/null", unix.O_RDONLY|unix.O_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	_ = unix.Close(probe)
	// Leave room for unix.Open(anchorsDir) inside ensureAnchor so the very
	// next FD allocation inside link.LoadPinnedLink fails with EMFILE.
	lim := unix.Rlimit{Cur: uint64(probe + 1), Max: old.Max}
	if err := unix.Setrlimit(unix.RLIMIT_NOFILE, &lim); err != nil {
		t.Fatal(err)
	}
	b.ensureAnchor(fn, zap.NewNop())
	if err := unix.Setrlimit(unix.RLIMIT_NOFILE, &old); err != nil {
		t.Fatalf("restore RLIMIT_NOFILE: %v", err)
	}

	if _, serr := os.Stat(linkPath); serr != nil {
		t.Fatalf("EMFILE on LoadPinnedLink removed live anchor pin %s: %v", linkPath, serr)
	}
	if !linkIDAlive(id) {
		t.Fatalf("EMFILE on LoadPinnedLink caused live anchor link %d to vanish", id)
	}

	b.ensureAnchor(fn, zap.NewNop())
	b.anchorMu.Lock()
	marked := b.anchoredFuncs[fn]
	b.anchorMu.Unlock()
	if !marked || CountAnchorLinks(root) != 1 {
		t.Fatalf("after restoring RLIMIT_NOFILE, expected anchored=true anchorLinks=1, got marked=%v anchorLinks=%d", marked, CountAnchorLinks(root))
	}
}

// TestOnKernelShutdownRetainsUnpinnedAnchor proves that when ensureAnchor's
// initial l.Pin fails AND GracefulShutdown's own retry also fails under
// failslab, GracefulShutdown moves the unpinned anchor link into
// retainedAnchorLinks so neither shutdown nor Go runtime GC closes the
// function's only link before process exit.
func TestOnKernelShutdownRetainsUnpinnedAnchor(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("requires root/CAP_BPF")
	}
	if _, err := os.Stat("/sys/kernel/debug/failslab"); err != nil {
		t.Skipf("requires CONFIG_FAILSLAB=y: %v", err)
	}
	if err := rlimit.RemoveMemlock(); err != nil {
		t.Fatal(err)
	}
	var stfs unix.Statfs_t
	if err := unix.Statfs(DefaultNativeBPFFSPath, &stfs); err != nil || stfs.Type != unix.BPF_FS_MAGIC {
		t.Skipf("%s is not a bpffs mount", DefaultNativeBPFFSPath)
	}
	resetStateForTest(t)

	root := filepath.Join(DefaultNativeBPFFSPath, "keploy-bpfsafety-ashut-"+randomNonce())
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	owner, err := openGraceOwnerInRoot(root, zap.NewNop())
	if err != nil {
		t.Fatal(err)
	}
	stateMu.Lock()
	activeOwner = owner
	stateMu.Unlock()

	// Use unix_seqpacket_sendmsg (distinct from every other test in this boot
	// so this anchor is the function's true ONLY link).
	const fn = "unix_seqpacket_sendmsg"
	sendUnixSeqpacketHelper(t, 10)
	pinFailAnchorForTest(t, owner, fn)

	n, id := pendingAnchorState(owner)
	if n != 1 || id == 0 || !linkIDAlive(id) {
		t.Fatalf("initial pin failure did not retain live anchor link (pending=%d id=%d alive=%v)", n, id, linkIDAlive(id))
	}

	// Keep failslab armed across GracefulShutdown so its retryUnpinnedAnchors
	// also fails and the link must be retained in retainedAnchorLinks.
	injectFailslabClose(t, func() { GracefulShutdown(context.Background(), zap.NewNop()) })

	stateMu.Lock()
	retained := len(retainedAnchorLinks)
	var retainedLink link.Link
	if retained > 0 {
		retainedLink = retainedAnchorLinks[0]
	}
	stateMu.Unlock()

	for i := 0; i < 3; i++ {
		runtime.GC()
		time.Sleep(300 * time.Millisecond)
	}

	alive := linkIDAlive(id)
	time.Sleep(1000 * time.Millisecond)
	for i := 0; i < 10; i++ {
		sendUnixSeqpacketHelper(t, 100)
		time.Sleep(50 * time.Millisecond)
	}
	if retained != 1 || !alive {
		t.Fatalf("unpinned anchor link was not retained across shutdown + GC (retained=%d alive=%v)", retained, alive)
	}

	// Pin the retained link cleanly now that retention across shutdown + GC is
	// verified, so resetStateForTest's cleanup does not let a later test's GC
	// drop the function's only link in the same VM boot.
	if retainedLink != nil {
		_ = retainedLink.Pin(filepath.Join(root, AnchorsSubdir, sanitizeToken(fn)+anchorLinkSuffix))
	}
}

// TestOnKernelStaticTrampolineUnpinnedAnchorRefusesFexitAndExitCloseSurvives
// proves both halves of the unpinned-anchor exit-close policy across all 4
// debug kernels (5.10, 5.15, 5.19, 6.12):
//   - On static-trampoline kernels (Linux 5.10.0–5.10.27 and 5.11.0–5.11.10,
//     where !kernelHasDynamicTrampImage()): AttachTracing FAILS CLOSED with
//     ErrAnchorUnpinned (0 leaked pins), closing the unpinned fentry-only
//     anchor while a task sleeps in unix_stream_recvmsg causes zero panic, and
//     once pressure clears AttachTracing pins the anchor and captures cleanly.
//   - On dynamic-trampoline kernels (5.10.28+, 5.11.11+, 5.12+): AttachTracing
//     proceeds while the anchor is unpinned, a sleeper under fexit survives
//     fexit detach + clean unpinned anchor exit-close with zero panic.
func TestOnKernelStaticTrampolineUnpinnedAnchorRefusesFexitAndExitCloseSurvives(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("requires root/CAP_BPF")
	}
	if _, err := os.Stat("/sys/kernel/debug/failslab"); err != nil {
		t.Skipf("requires CONFIG_FAILSLAB=y: %v", err)
	}
	if err := rlimit.RemoveMemlock(); err != nil {
		t.Fatal(err)
	}
	var stfs unix.Statfs_t
	if err := unix.Statfs(DefaultNativeBPFFSPath, &stfs); err != nil || stfs.Type != unix.BPF_FS_MAGIC {
		t.Skipf("%s is not a bpffs mount", DefaultNativeBPFFSPath)
	}
	resetStateForTest(t)

	staticKernel := !kernelHasDynamicTrampImage()
	root := filepath.Join(DefaultNativeBPFFSPath, "keploy-bpfsafety-staticunp-"+randomNonce())
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	owner, err := openGraceOwnerInRoot(root, zap.NewNop())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = unix.Close(owner.lockFD) }()
	stateMu.Lock()
	activeOwner = owner
	stateMu.Unlock()

	const fn = "unix_stream_recvmsg"
	fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = unix.Close(fds[0])
		_ = unix.Close(fds[1])
	}()

	hitMap, err := ebpf.NewMap(&ebpf.MapSpec{Type: ebpf.Array, KeySize: 4, ValueSize: 8, MaxEntries: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = hitMap.Close() }()
	p := makeCountingFexitProg(t, "r7_static_fexit", fn, hitMap)
	defer func() { _ = p.Close() }()

	failslabDir := "/sys/kernel/debug/failslab"
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
	_ = os.WriteFile(filepath.Join(failslabDir, "times"), []byte("1"), 0o644)
	_ = os.WriteFile(filepath.Join(failslabDir, "space"), []byte("0"), 0o644)
	hookRan := false
	owner.onBeforeAnchorPin = func() {
		hookRan = true
		_, _ = unix.Pwrite(failFD, []byte("1\n"), 0)
	}
	fexitLink, attachErr := AttachTracing(p, "r7_static", fn, "p")
	_, _ = unix.Pwrite(failFD, []byte("0\n"), 0)
	remRaw, _ := os.ReadFile(filepath.Join(failslabDir, "times"))
	_ = unix.Close(failFD)
	_ = os.WriteFile(filepath.Join(failslabDir, "times"), []byte("0"), 0o644)
	_ = os.WriteFile(filepath.Join(failslabDir, "probability"), []byte("0"), 0o644)
	runtime.UnlockOSThread()
	owner.onBeforeAnchorPin = nil

	if !hookRan {
		t.Fatal("onBeforeAnchorPin hook did not run during AttachTracing")
	}
	if rem, perr := strconv.Atoi(strings.TrimSpace(string(remRaw))); perr == nil && rem >= 1 {
		t.Fatalf("failslab did not inject -ENOMEM during anchor pin (times remained %d)", rem)
	}
	n, id := pendingAnchorState(owner)
	if n != 1 || id == 0 || !linkIDAlive(id) {
		t.Fatalf("expected 1 live unpinned anchor after failslab, pending=%d id=%d alive=%v", n, id, linkIDAlive(id))
	}
	if owner.isAnchorPinned(fn) {
		t.Fatalf("expected isAnchorPinned(%s)=false while link is unpinned", fn)
	}

	if staticKernel {
		if fexitLink != nil {
			_ = fexitLink.Close()
			t.Fatalf("static-trampoline kernel: AttachTracing(fexit/%s) unexpectedly succeeded while anchor was unpinned", fn)
		}
		if !errors.Is(attachErr, ErrAnchorUnpinned) {
			t.Fatalf("static-trampoline kernel: AttachTracing(fexit/%s) err = %v, want ErrAnchorUnpinned", fn, attachErr)
		}
		if got := countPinnedProgs(owner.dir); got != 0 {
			t.Fatalf("static-trampoline kernel: expected 0 leaked pins after ErrAnchorUnpinned, got %d", got)
		}
	} else {
		if attachErr != nil || fexitLink == nil {
			t.Fatalf("dynamic-trampoline kernel: AttachTracing(fexit/%s) must proceed while anchor is unpinned, got link=%v err=%v", fn, fexitLink, attachErr)
		}
	}

	// Put a thread to sleep in unix_stream_recvmsg, detach fexit (if dynamic),
	// cleanly close the unpinned anchor (simulating process exit), then wake
	// the sleeper and verify zero panic.
	done := make(chan error, 1)
	go func() {
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
		buf := make([]byte, 8)
		_, rerr := unix.Read(fds[1], buf)
		done <- rerr
	}()
	time.Sleep(500 * time.Millisecond)

	if fexitLink != nil {
		_ = fexitLink.Close()
		time.Sleep(500 * time.Millisecond)
	}

	owner.anchorMu.Lock()
	var unpinnedLink link.Link
	if len(owner.unpinnedAnchors) > 0 {
		unpinnedLink = owner.unpinnedAnchors[0].lnk
		owner.unpinnedAnchors = nil
	}
	owner.anchorMu.Unlock()
	if unpinnedLink == nil {
		t.Fatal("expected unpinned anchor link to close for exit simulation")
	}
	_ = unpinnedLink.Close()
	time.Sleep(1500 * time.Millisecond)

	if _, err := unix.Write(fds[0], []byte("x")); err != nil {
		t.Fatal(err)
	}
	select {
	case rerr := <-done:
		if rerr != nil {
			t.Fatalf("sleeper read err = %v", rerr)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("sleeper did not wake after clean unpinned anchor close")
	}

	if staticKernel {
		// Now that memory pressure cleared, AttachTracing pins the anchor
		// cleanly and attaches fexit/unix_stream_recvmsg with zero panic.
		l2, err := AttachTracing(p, "r7_static", fn, "p")
		if err != nil {
			t.Fatalf("AttachTracing(fexit/%s) after pressure cleared: %v", fn, err)
		}
		if !owner.isAnchorPinned(fn) || CountAnchorLinks(root) != 1 {
			_ = l2.Close()
			t.Fatalf("expected pinned anchor after recovery (pinned=%v anchorLinks=%d)", owner.isAnchorPinned(fn), CountAnchorLinks(root))
		}
		done2 := make(chan error, 1)
		go func() {
			runtime.LockOSThread()
			defer runtime.UnlockOSThread()
			buf := make([]byte, 8)
			_, rerr := unix.Read(fds[1], buf)
			done2 <- rerr
		}()
		time.Sleep(500 * time.Millisecond)
		_ = l2.Close()
		time.Sleep(500 * time.Millisecond)
		if _, err := unix.Write(fds[0], []byte("y")); err != nil {
			t.Fatal(err)
		}
		select {
		case rerr := <-done2:
			if rerr != nil {
				t.Fatalf("second sleeper read err = %v", rerr)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("second sleeper did not wake after fexit detach with pinned anchor")
		}
	}
}

func blockAnchorsDirForTest(t *testing.T, root string) (unblock func()) {
	t.Helper()
	ad := filepath.Join(root, AnchorsSubdir)
	bak := ad + "-blockbak"
	if err := os.Rename(ad, bak); err != nil {
		t.Fatalf("move anchors dir aside: %v", err)
	}
	m, err := ebpf.NewMap(&ebpf.MapSpec{Type: ebpf.Array, KeySize: 4, ValueSize: 4, MaxEntries: 1})
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Pin(ad); err != nil {
		_ = m.Close()
		t.Fatalf("pin blocker map at %s: %v", ad, err)
	}
	_ = m.Close()
	return func() {
		if err := os.Remove(ad); err != nil {
			t.Fatalf("unpin blocker map: %v", err)
		}
		if err := os.Rename(bak, ad); err != nil {
			t.Fatalf("restore anchors dir: %v", err)
		}
	}
}

func pinnedAnchorLinkID(root, fn string) (link.ID, error) {
	l, err := link.LoadPinnedLink(filepath.Join(root, AnchorsSubdir, sanitizeToken(fn)+anchorLinkSuffix), nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = l.Close() }()
	info, err := l.Info()
	if err != nil {
		return 0, err
	}
	return info.ID, nil
}

// TestOnKernelPersistentAnchorPinFailure proves across all 4 kernels (5.10,
// 5.15, 5.19, 6.12) that when anchor pinning fails persistently across 6
// reaper rounds and 6 AttachTracing retries (ENOTDIR via blocker map):
//   - On static-trampoline kernels (5.10.0–5.10.27, 5.11.0–5.11.10): EVERY
//     attempt returns ErrAnchorUnpinned with 0 leaked prog pins and keeps the
//     single pending anchor FD (killing M42 and M42b).
//   - On dynamic-trampoline kernels (5.10.28+, 5.11.11+, 5.12+): EVERY attempt
//     attaches cleanly while the anchor is unpinned (killing M44).
//   - Once unblocked, the next AttachTracing pins that exact SAME anchor link
//     ID (no second anchor) and the attached fexit captures traffic cleanly.
func TestOnKernelPersistentAnchorPinFailure(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("requires root/CAP_BPF")
	}
	if err := rlimit.RemoveMemlock(); err != nil {
		t.Fatal(err)
	}
	var stfs unix.Statfs_t
	if err := unix.Statfs(DefaultNativeBPFFSPath, &stfs); err != nil || stfs.Type != unix.BPF_FS_MAGIC {
		t.Skipf("%s is not a bpffs mount", DefaultNativeBPFFSPath)
	}
	resetStateForTest(t)

	staticKernel := !kernelHasDynamicTrampImage()
	root := filepath.Join(DefaultNativeBPFFSPath, "keploy-bpfsafety-persist-"+randomNonce())
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	owner, err := openGraceOwnerInRoot(root, zap.NewNop())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = unix.Close(owner.lockFD) }()
	stateMu.Lock()
	activeOwner = owner
	stateMu.Unlock()

	// Fresh function unused by any other test in this boot.
	const fn = "unix_dgram_recvmsg"
	hitMap, err := ebpf.NewMap(&ebpf.MapSpec{Type: ebpf.Array, KeySize: 4, ValueSize: 8, MaxEntries: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = hitMap.Close() }()
	p := makeCountingFexitProg(t, "persist_fexit", fn, hitMap)
	defer func() { _ = p.Close() }()

	var unblock func()
	owner.onBeforeAnchorPin = func() { unblock = blockAnchorsDirForTest(t, root) }
	l0, err0 := AttachTracing(p, "persist_test", fn, "p")
	owner.onBeforeAnchorPin = nil
	if unblock == nil {
		t.Fatal("onBeforeAnchorPin seam did not run")
	}
	unblocked := false
	defer func() {
		if !unblocked {
			unblock()
		}
	}()

	n0, id0 := pendingAnchorState(owner)
	if n0 != 1 || id0 == 0 || !linkIDAlive(id0) {
		t.Fatalf("blocked pin did not leave 1 live pending anchor (pending=%d id=%d alive=%v)", n0, id0, linkIDAlive(id0))
	}
	attempts := []error{err0}
	if l0 != nil {
		_ = l0.Close()
	}

	for i := 1; i <= 6; i++ {
		owner.reaper.round()
		// On dynamic kernels, load a fresh program per attempt so early dynamic
		// kernels (e.g. 5.11.11) that reject re-attaching a closed tracing FD
		// with ENOENT do not fail falsely; on static kernels, re-trying the same
		// program exercises the refused path directly.
		attemptProg := p
		if !staticKernel {
			attemptProg = makeCountingFexitProg(t, fmt.Sprintf("persist_fexit_%d", i), fn, hitMap)
		}
		l, aerr := AttachTracing(attemptProg, "persist_test", fn, "p")
		if l != nil {
			_ = l.Close()
		}
		if attemptProg != p {
			_ = attemptProg.Close()
		}
		attempts = append(attempts, aerr)
		n, id := pendingAnchorState(owner)
		if n != 1 || id != id0 {
			t.Fatalf("attempt %d: anchor state changed under persistent pin failure (pending=%d id=%d, want 1/%d)", i, n, id, id0)
		}
	}
	for i, aerr := range attempts {
		if staticKernel && !errors.Is(aerr, ErrAnchorUnpinned) {
			t.Fatalf("static kernel: attempt %d attached or returned wrong err while anchor unpinned: %v", i, aerr)
		}
		if !staticKernel && aerr != nil {
			t.Fatalf("dynamic kernel: attempt %d refused while anchor unpinned: %v", i, aerr)
		}
	}
	if staticKernel && countPinnedProgs(owner.dir) != 0 {
		t.Fatalf("static kernel: %d program pins leaked by refused attaches", countPinnedProgs(owner.dir))
	}

	unblock()
	unblocked = true
	finalProg := p
	if !staticKernel {
		finalProg = makeCountingFexitProg(t, "persist_fexit_rec", fn, hitMap)
		defer func() { _ = finalProg.Close() }()
	}
	l, err := AttachTracing(finalProg, "persist_test", fn, "p")
	if err != nil {
		t.Fatalf("AttachTracing after unblocking anchor pin: %v", err)
	}
	defer func() { _ = l.Close() }()
	nAfter, _ := pendingAnchorState(owner)
	pid, perr := pinnedAnchorLinkID(root, fn)
	if perr != nil || nAfter != 0 || !owner.isAnchorPinned(fn) || CountAnchorLinks(root) != 1 || pid != id0 {
		t.Fatalf("recovery did not pin the existing anchor FD (pending=%d pinned=%v anchorLinks=%d pinnedID=%d err=%v want %d)",
			nAfter, owner.isAnchorPinned(fn), CountAnchorLinks(root), pid, perr, id0)
	}

	var before, after uint64
	_ = hitMap.Lookup(uint32(0), &before)
	sendUnixDgramsHelper(t, 5)
	_ = hitMap.Lookup(uint32(0), &after)
	if after <= before {
		t.Fatalf("fexit/%s attached after recovery did not capture (hits %d -> %d)", fn, before, after)
	}
}

// TestOnKernelRootStatfsFaultNeverFailsOpen proves across all 4 debug kernels
// that:
//  1. Injecting slab faults via /proc/self/task/<tid>/fail-nth across
//     openGraceOwnerInRoot NEVER yields an owner with err==nil and
//     rootIsBPFFS==false on a real bpffs root (openGraceOwnerInRoot fails
//     closed and cleans up on unix.Statfs error).
//  2. On static-trampoline kernels, even if rootIsBPFFS were false, the
//     static-trampoline guard in pinProgram still refuses with
//     ErrAnchorUnpinned and leaks 0 pins.
func TestOnKernelRootStatfsFaultNeverFailsOpen(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("requires root/CAP_BPF")
	}
	failslabDir := "/sys/kernel/debug/failslab"
	if _, err := os.Stat(failslabDir); err != nil {
		t.Skipf("requires CONFIG_FAILSLAB=y: %v", err)
	}
	if err := rlimit.RemoveMemlock(); err != nil {
		t.Fatal(err)
	}
	var stfs unix.Statfs_t
	if err := unix.Statfs(DefaultNativeBPFFSPath, &stfs); err != nil || stfs.Type != unix.BPF_FS_MAGIC {
		t.Skipf("%s is not a bpffs mount", DefaultNativeBPFFSPath)
	}
	resetStateForTest(t)

	root := filepath.Join(DefaultNativeBPFFSPath, "keploy-bpfsafety-statfs-"+randomNonce())
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}

	for _, kv := range [][2]string{{"task-filter", "N"}, {"ignore-gfp-wait", "N"}, {"probability", "0"}, {"times", "-1"}, {"verbose", "0"}} {
		_ = os.WriteFile(filepath.Join(failslabDir, kv[0]), []byte(kv[1]), 0o644)
	}
	defer func() {
		_ = os.WriteFile(filepath.Join(failslabDir, "times"), []byte("0"), 0o644)
		_ = os.WriteFile(filepath.Join(failslabDir, "probability"), []byte("0"), 0o644)
	}()

	type sweepRes struct {
		failOpenNth int
		errCount    int
		skipReason  string
	}
	ch := make(chan sweepRes, 1)
	go func() {
		runtime.LockOSThread()
		// Intentionally do not unlock so any residual fail-nth dies with this thread.
		var r sweepRes
		nfd, err := unix.Open(fmt.Sprintf("/proc/self/task/%d/fail-nth", unix.Gettid()), unix.O_RDWR, 0)
		if err != nil {
			r.skipReason = fmt.Sprintf("fail-nth unavailable: %v", err)
			ch <- r
			return
		}
		defer func() { _ = unix.Close(nfd) }()
		arm := func(n int) { _, _ = unix.Pwrite(nfd, []byte(strconv.Itoa(n)), 0) }
		left := func() string {
			b := make([]byte, 16)
			k, _ := unix.Pread(nfd, b, 0)
			if k < 0 {
				k = 0
			}
			return strings.TrimSpace(string(b[:k]))
		}
		for n := 1; n <= 250; n++ {
			arm(n)
			o, oerr := openGraceOwnerInRoot(root, zap.NewNop())
			lf := left()
			if lf != "0" {
				arm(0)
			}
			if oerr != nil {
				r.errCount++
				continue
			}
			isBPF := o.rootIsBPFFS
			_ = unix.Close(o.lockFD)
			_ = os.RemoveAll(o.dir)
			if !isBPF {
				r.failOpenNth = n
				break
			}
			if lf != "0" {
				break
			}
		}
		ch <- r
	}()
	res := <-ch
	if res.skipReason != "" {
		t.Skip(res.skipReason)
	}
	if res.failOpenNth != 0 {
		t.Fatalf("openGraceOwnerInRoot with fail-nth=%d returned err=nil with rootIsBPFFS=false on a real bpffs root (silent fail-open!)", res.failOpenNth)
	}
	if res.errCount == 0 {
		t.Fatal("fail-nth sweep injected 0 errors into openGraceOwnerInRoot")
	}

	// Belt-and-suspenders: on static-trampoline kernels, even if rootIsBPFFS
	// were false, pinProgram MUST still refuse unanchored tracing attaches.
	if !kernelHasDynamicTrampImage() {
		owner, err := openGraceOwnerInRoot(root, zap.NewNop())
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = unix.Close(owner.lockFD) }()
		owner.rootIsBPFFS = false
		stateMu.Lock()
		activeOwner = owner
		stateMu.Unlock()

		p := makeCountingFexitProg(t, "statfs_guard", "bpf_fentry_test5", nil)
		defer func() { _ = p.Close() }()
		l, aerr := AttachTracing(p, "statfs_guard", "bpf_fentry_test5", "p")
		if l != nil {
			_ = l.Close()
			t.Fatal("static-trampoline kernel attached unanchored fexit when rootIsBPFFS=false")
		}
		if !errors.Is(aerr, ErrAnchorUnpinned) {
			t.Fatalf("AttachTracing with rootIsBPFFS=false err = %v, want ErrAnchorUnpinned", aerr)
		}
		if got := countPinnedProgs(owner.dir); got != 0 {
			t.Fatalf("expected 0 leaked pins after ErrAnchorUnpinned, got %d", got)
		}
	}
}

// TestOnKernelInitUnprotectedStaticTrampolineRefusesFexit proves end-to-end
// through the production bpfsafety.Init + AttachTracing entry points across
// all 4 debug kernels (5.10, 5.15, 5.19, 6.12) that:
//  1. When Init enters unprotected mode because host bpffs is missing/invalid,
//     it logs UnprotectedWarnMessage; on static-trampoline kernels (5.10),
//     AttachTracing(fexit/unix_stream_recvmsg) STILL fails closed with
//     ErrAnchorUnpinned (preventing unanchored exit-close node panics); on
//     dynamic kernels (5.15, 5.19, 6.12), it attaches and survives a clean
//     exit-time close with a reader asleep inside unix_stream_recvmsg.
//  2. When host bpffs validates but grace directory setup fails persistently
//     (e.g. statfs ENOMEM on both attempts), Init logs UnprotectedInitErrWarnMessage
//     (naming the real init error rather than claiming bpffs is unmounted) and
//     static-trampoline kernels STILL refuse AttachTracing with ErrAnchorUnpinned.
//  3. A single transient failslab fault sweeping Init(ModeNative) on real
//     /sys/fs/bpf either recovers via openGraceOwner's retry (arming host bpffs)
//     or refuses fexit on static-trampoline kernels — never attaching an
//     unanchored fexit that panics on sleeper exit-close.
func TestOnKernelInitUnprotectedStaticTrampolineRefusesFexit(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("requires root/CAP_BPF")
	}
	failslabDir := "/sys/kernel/debug/failslab"
	if _, err := os.Stat(failslabDir); err != nil {
		t.Skipf("requires CONFIG_FAILSLAB=y: %v", err)
	}
	if err := rlimit.RemoveMemlock(); err != nil {
		t.Fatal(err)
	}
	var stfs unix.Statfs_t
	if err := unix.Statfs(DefaultNativeBPFFSPath, &stfs); err != nil || stfs.Type != unix.BPF_FS_MAGIC {
		t.Skipf("%s is not a bpffs mount", DefaultNativeBPFFSPath)
	}
	resetStateForTest(t)
	t.Setenv(EnvAllowUnprotectedTrampolines, "")

	staticKernel := !kernelHasDynamicTrampImage()
	const fn = "unix_stream_recvmsg"

	fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = unix.Close(fds[0])
		_ = unix.Close(fds[1])
	}()

	checkAttachAndSleeper := func(t *testing.T, tag string) {
		t.Helper()
		hitMap, herr := ebpf.NewMap(&ebpf.MapSpec{Type: ebpf.Array, KeySize: 4, ValueSize: 8, MaxEntries: 1})
		if herr != nil {
			t.Fatal(herr)
		}
		defer func() { _ = hitMap.Close() }()
		p := makeCountingFexitProg(t, tag, fn, hitMap)
		defer func() { _ = p.Close() }()

		l, aerr := AttachTracing(p, tag, fn, "p")
		if staticKernel {
			if l != nil {
				_ = l.Close()
				t.Fatalf("%s: static-trampoline kernel attached unanchored fexit/%s in unprotected mode", tag, fn)
			}
			if !errors.Is(aerr, ErrAnchorUnpinned) {
				t.Fatalf("%s: static-trampoline kernel err = %v, want ErrAnchorUnpinned", tag, aerr)
			}
			return
		}
		if aerr != nil || l == nil {
			t.Fatalf("%s: dynamic kernel refused fexit/%s in unprotected mode: l=%v err=%v", tag, fn, l, aerr)
		}
		done := make(chan error, 1)
		go func() {
			runtime.LockOSThread()
			defer runtime.UnlockOSThread()
			buf := make([]byte, 8)
			_, rerr := unix.Read(fds[1], buf)
			done <- rerr
		}()
		time.Sleep(400 * time.Millisecond)
		_ = l.Close()
		time.Sleep(400 * time.Millisecond)
		if _, werr := unix.Write(fds[0], []byte("z")); werr != nil {
			t.Fatal(werr)
		}
		select {
		case rerr := <-done:
			if rerr != nil {
				t.Fatalf("%s: sleeper read err = %v", tag, rerr)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("%s: sleeper did not wake after fexit close", tag)
		}
	}

	// 1. Missing/non-bpffs host path -> Init unprotected mode with UnprotectedWarnMessage.
	t.Run("missing_host_bpffs", func(t *testing.T) {
		resetStateForTest(t)
		t.Setenv(EnvHostBPFFS, t.TempDir())
		core, logs := observer.New(zap.WarnLevel)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		if ierr := Init(ctx, zap.New(core), ModeNative); ierr != nil {
			t.Fatalf("Init(missing bpffs) = %v, want nil", ierr)
		}
		if Armed() || !UnprotectedTrampolinesActive() {
			t.Fatalf("expected unprotected mode, got armed=%v unprot=%v", Armed(), UnprotectedTrampolinesActive())
		}
		if logs.Len() < 1 || logs.All()[0].Message != UnprotectedWarnMessage {
			t.Fatalf("missing bpffs WARN = %+v, want %q", logs.All(), UnprotectedWarnMessage)
		}
		checkAttachAndSleeper(t, "unprot_nobpffs")
	})

	// 2. Validated real /sys/fs/bpf mount whose grace setup fails persistently
	//    (e.g. statfs ENOMEM) -> Init unprotected mode with UnprotectedInitErrWarnMessage.
	t.Run("validated_bpffs_init_error", func(t *testing.T) {
		resetStateForTest(t)
		t.Setenv(EnvHostBPFFS, "")
		openGraceOwnerInRootFn = func(root string, _ *zap.Logger) (*graceOwner, error) {
			return nil, fmt.Errorf("statfs grace root %s: %w", root, unix.ENOMEM)
		}
		core, logs := observer.New(zap.WarnLevel)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		if ierr := Init(ctx, zap.New(core), ModeNative); ierr != nil {
			t.Fatalf("Init(validated bpffs statfs ENOMEM) = %v, want nil", ierr)
		}
		if Armed() || !UnprotectedTrampolinesActive() {
			t.Fatalf("expected unprotected mode, got armed=%v unprot=%v", Armed(), UnprotectedTrampolinesActive())
		}
		if logs.Len() < 1 || logs.All()[0].Message != UnprotectedInitErrWarnMessage {
			t.Fatalf("validated bpffs init-err WARN = %+v, want %q", logs.All(), UnprotectedInitErrWarnMessage)
		}
		checkAttachAndSleeper(t, "unprot_initerr")
	})

	// 3. Real failslab single-fault sweep over Init(ModeNative) on /sys/fs/bpf:
	//    every single transient fault (including ensureNativeBPFFS statfs,
	//    mount-check statfs/stat, and grace-dir statfs) recovers via
	//    openGraceOwner's retry and ends Armed, never falling into unprotected
	//    mode with a misleading mount WARN and NEVER stacking a second bpffs
	//    mount over /sys/fs/bpf.
	countBPFFSMounts := func(target string) int {
		b, err := os.ReadFile("/proc/self/mountinfo")
		if err != nil {
			return -1
		}
		n := 0
		for _, line := range strings.Split(string(b), "\n") {
			sep := strings.Index(line, " - ")
			if sep < 0 {
				continue
			}
			left := strings.Fields(line[:sep])
			right := strings.Fields(line[sep+3:])
			if len(left) >= 5 && len(right) >= 1 && left[4] == target && right[0] == "bpf" {
				n++
			}
		}
		return n
	}
	t.Run("real_failslab_init_sweep", func(t *testing.T) {
		resetStateForTest(t)
		t.Setenv(EnvHostBPFFS, "")
		baseMounts := countBPFFSMounts(DefaultNativeBPFFSPath)
		if baseMounts < 1 {
			t.Fatalf("expected >=1 bpffs mount at %s before sweep, got %d", DefaultNativeBPFFSPath, baseMounts)
		}
		for _, kv := range [][2]string{{"task-filter", "N"}, {"ignore-gfp-wait", "N"}, {"probability", "0"}, {"times", "-1"}, {"verbose", "0"}} {
			_ = os.WriteFile(filepath.Join(failslabDir, kv[0]), []byte(kv[1]), 0o644)
		}
		defer func() {
			_ = os.WriteFile(filepath.Join(failslabDir, "times"), []byte("0"), 0o644)
			_ = os.WriteFile(filepath.Join(failslabDir, "probability"), []byte("0"), 0o644)
		}()

		type res struct {
			errMsg string
		}
		ch := make(chan res, 1)
		go func() {
			runtime.LockOSThread()
			nfd, oerr := unix.Open(fmt.Sprintf("/proc/self/task/%d/fail-nth", unix.Gettid()), unix.O_RDWR, 0)
			if oerr != nil {
				ch <- res{errMsg: "open fail-nth: " + oerr.Error()}
				return
			}
			defer func() { _ = unix.Close(nfd) }()
			arm := func(n int) { _, _ = unix.Pwrite(nfd, []byte(strconv.Itoa(n)), 0) }
			left := func() string {
				b := make([]byte, 16)
				k, _ := unix.Pread(nfd, b, 0)
				if k < 0 {
					k = 0
				}
				return strings.TrimSpace(string(b[:k]))
			}
			for n := 1; n <= 300; n++ {
				core, logs := observer.New(zap.WarnLevel)
				ctx, cancel := context.WithCancel(context.Background())
				arm(n)
				_ = Init(ctx, zap.New(core), ModeNative)
				lf := left()
				if lf != "0" {
					arm(0)
				}
				stateMu.Lock()
				o := activeOwner
				stateMu.Unlock()
				unprot := UnprotectedTrampolinesActive()
				gotMounts := countBPFFSMounts(DefaultNativeBPFFSPath)
				if gotMounts > baseMounts {
					cancel()
					if o != nil {
						if o.reaper != nil {
							o.reaper.stopAndWait(o.reaperCancel)
						}
						stateMu.Lock()
						activeOwner = nil
						stateMu.Unlock()
						_ = unix.Close(o.lockFD)
						_ = os.RemoveAll(o.dir)
					}
					for k := 0; k < 8 && countBPFFSMounts(DefaultNativeBPFFSPath) > baseMounts; k++ {
						_ = unix.Unmount(DefaultNativeBPFFSPath, 0)
					}
					ch <- res{errMsg: fmt.Sprintf("single failslab fault fail-nth=%d stacked a new bpffs mount over %s (%d -> %d)", n, DefaultNativeBPFFSPath, baseMounts, gotMounts)}
					return
				}
				if unprot || o == nil {
					cancel()
					ch <- res{errMsg: fmt.Sprintf("single failslab fault fail-nth=%d left Init unprotected (armed=%v unprot=%v logs=%+v)", n, o != nil, unprot, logs.All())}
					return
				}
				cancel()
				if o.reaper != nil {
					o.reaper.stopAndWait(o.reaperCancel)
				}
				stateMu.Lock()
				activeOwner = nil
				stateMu.Unlock()
				_ = unix.Close(o.lockFD)
				_ = os.RemoveAll(o.dir)
				unprotectedTrampolines.Store(false)
				explicitUnprotectedOptOut.Store(false)
				unprotectedWarned.Store(false)
				stateMu.Lock()
				unprotectedLogger = nil
				unprotectedWarnedFuncs = map[string]bool{}
				stateMu.Unlock()
				if lf != "0" {
					break
				}
			}
			ch <- res{}
		}()
		r := <-ch
		if r.errMsg != "" {
			t.Fatal(r.errMsg)
		}
	})

	// 4. Held failslab fault across openGraceOwnerInRoot retry: re-arm fail-nth
	//    on each call to openGraceOwnerInRoot so a real kernel fault holds across
	//    both attempts, putting Init into unprotected mode with
	//    UnprotectedInitErrWarnMessage while static kernels still refuse fexit.
	t.Run("held_failslab_grace_statfs", func(t *testing.T) {
		resetStateForTest(t)
		t.Setenv(EnvHostBPFFS, "")
		for _, kv := range [][2]string{{"task-filter", "N"}, {"ignore-gfp-wait", "N"}, {"probability", "0"}, {"times", "-1"}, {"verbose", "0"}} {
			_ = os.WriteFile(filepath.Join(failslabDir, kv[0]), []byte(kv[1]), 0o644)
		}
		defer func() {
			_ = os.WriteFile(filepath.Join(failslabDir, "times"), []byte("0"), 0o644)
			_ = os.WriteFile(filepath.Join(failslabDir, "probability"), []byte("0"), 0o644)
		}()

		sweepProg := makeCountingFexitProg(t, "held_init", fn, nil)
		defer func() { _ = sweepProg.Close() }()

		type res struct {
			hitUnprot int
			errMsg    string
		}
		ch := make(chan res, 1)
		go func() {
			runtime.LockOSThread()
			nfd, oerr := unix.Open(fmt.Sprintf("/proc/self/task/%d/fail-nth", unix.Gettid()), unix.O_RDWR, 0)
			if oerr != nil {
				ch <- res{errMsg: "open fail-nth: " + oerr.Error()}
				return
			}
			defer func() { _ = unix.Close(nfd) }()
			arm := func(n int) { _, _ = unix.Pwrite(nfd, []byte(strconv.Itoa(n)), 0) }
			left := func() string {
				b := make([]byte, 16)
				k, _ := unix.Pread(nfd, b, 0)
				if k < 0 {
					k = 0
				}
				return strings.TrimSpace(string(b[:k]))
			}
			var r res
			for n := 1; n <= 80; n++ {
				targetN := n
				fired := false
				openGraceOwnerInRootFn = func(root string, logger *zap.Logger) (*graceOwner, error) {
					arm(targetN)
					owner, oerr := openGraceOwnerInRoot(root, logger)
					if left() == "0" {
						fired = true
					} else {
						arm(0)
					}
					return owner, oerr
				}
				core, logs := observer.New(zap.WarnLevel)
				ctx, cancel := context.WithCancel(context.Background())
				_ = Init(ctx, zap.New(core), ModeNative)
				arm(0)
				stateMu.Lock()
				o := activeOwner
				stateMu.Unlock()
				if UnprotectedTrampolinesActive() {
					r.hitUnprot++
					if logs.Len() < 1 || logs.All()[0].Message != UnprotectedInitErrWarnMessage {
						cancel()
						r.errMsg = fmt.Sprintf("held fail-nth=%d WARN = %+v, want %q", n, logs.All(), UnprotectedInitErrWarnMessage)
						ch <- r
						return
					}
					if staticKernel {
						l, aerr := AttachTracing(sweepProg, "held_init", fn, "p")
						if l != nil {
							_ = l.Close()
							cancel()
							r.errMsg = fmt.Sprintf("held fail-nth=%d: static kernel attached unanchored fexit in unprotected mode", n)
							ch <- r
							return
						}
						if !errors.Is(aerr, ErrAnchorUnpinned) {
							cancel()
							r.errMsg = fmt.Sprintf("held fail-nth=%d: static kernel err=%v, want ErrAnchorUnpinned", n, aerr)
							ch <- r
							return
						}
					}
				}
				cancel()
				if o != nil {
					if o.reaper != nil {
						o.reaper.stopAndWait(o.reaperCancel)
					}
					stateMu.Lock()
					activeOwner = nil
					stateMu.Unlock()
					_ = unix.Close(o.lockFD)
					_ = os.RemoveAll(o.dir)
				}
				unprotectedTrampolines.Store(false)
				explicitUnprotectedOptOut.Store(false)
				unprotectedWarned.Store(false)
				stateMu.Lock()
				unprotectedLogger = nil
				unprotectedWarnedFuncs = map[string]bool{}
				stateMu.Unlock()
				if o != nil && !fired {
					break
				}
			}
			ch <- r
		}()
		r := <-ch
		if r.errMsg != "" {
			t.Fatal(r.errMsg)
		}
		if r.hitUnprot == 0 {
			t.Fatal("held failslab sweep never triggered unprotected mode across openGraceOwnerInRoot")
		}
	})

	// 5. Held failslab fault across validateHostBPFFSPathFn retry: re-arm fail-nth
	//    before every mount-check validation so a real kernel fault holds across
	//    both attempts, putting Init into unprotected mode with
	//    UnprotectedMountCheckErrWarnMessage (never the mount hint, killing M63
	//    and M65 on the kernel gate), never stacking mounts, and keeping static
	//    kernels fail-closed.
	t.Run("held_failslab_mount_check", func(t *testing.T) {
		resetStateForTest(t)
		t.Setenv(EnvHostBPFFS, "")
		baseMounts := countBPFFSMounts(DefaultNativeBPFFSPath)
		for _, kv := range [][2]string{{"task-filter", "N"}, {"ignore-gfp-wait", "N"}, {"probability", "0"}, {"times", "-1"}, {"verbose", "0"}} {
			_ = os.WriteFile(filepath.Join(failslabDir, kv[0]), []byte(kv[1]), 0o644)
		}
		defer func() {
			_ = os.WriteFile(filepath.Join(failslabDir, "times"), []byte("0"), 0o644)
			_ = os.WriteFile(filepath.Join(failslabDir, "probability"), []byte("0"), 0o644)
		}()

		sweepProg := makeCountingFexitProg(t, "held_mnt", fn, nil)
		defer func() { _ = sweepProg.Close() }()

		type res struct {
			hitUnprot int
			errMsg    string
		}
		ch := make(chan res, 1)
		go func() {
			runtime.LockOSThread()
			nfd, oerr := unix.Open(fmt.Sprintf("/proc/self/task/%d/fail-nth", unix.Gettid()), unix.O_RDWR, 0)
			if oerr != nil {
				ch <- res{errMsg: "open fail-nth: " + oerr.Error()}
				return
			}
			defer func() { _ = unix.Close(nfd) }()
			arm := func(n int) { _, _ = unix.Pwrite(nfd, []byte(strconv.Itoa(n)), 0) }
			left := func() string {
				b := make([]byte, 16)
				k, _ := unix.Pread(nfd, b, 0)
				if k < 0 {
					k = 0
				}
				return strings.TrimSpace(string(b[:k]))
			}
			var r res
			for n := 1; n <= 40; n++ {
				targetN := n
				fired := false
				validateHostBPFFSPathFn = func(path string, chk bool) error {
					arm(targetN)
					verr := ValidateHostBPFFSPath(path, chk)
					if left() == "0" {
						fired = true
					} else {
						arm(0)
					}
					return verr
				}
				core, logs := observer.New(zap.WarnLevel)
				ctx, cancel := context.WithCancel(context.Background())
				_ = Init(ctx, zap.New(core), ModeNative)
				arm(0)
				validateHostBPFFSPathFn = ValidateHostBPFFSPath
				stateMu.Lock()
				o := activeOwner
				stateMu.Unlock()
				if gotMounts := countBPFFSMounts(DefaultNativeBPFFSPath); gotMounts > baseMounts {
					cancel()
					for k := 0; k < 8 && countBPFFSMounts(DefaultNativeBPFFSPath) > baseMounts; k++ {
						_ = unix.Unmount(DefaultNativeBPFFSPath, 0)
					}
					r.errMsg = fmt.Sprintf("held mount-check fail-nth=%d stacked bpffs mount (%d -> %d)", n, baseMounts, gotMounts)
					ch <- r
					return
				}
				if UnprotectedTrampolinesActive() {
					r.hitUnprot++
					if logs.Len() < 1 || logs.All()[0].Message != UnprotectedMountCheckErrWarnMessage {
						cancel()
						r.errMsg = fmt.Sprintf("held mount-check fail-nth=%d WARN = %+v, want %q", n, logs.All(), UnprotectedMountCheckErrWarnMessage)
						ch <- r
						return
					}
					if staticKernel {
						l, aerr := AttachTracing(sweepProg, "held_mnt", fn, "p")
						if l != nil {
							_ = l.Close()
							cancel()
							r.errMsg = fmt.Sprintf("held mount-check fail-nth=%d: static kernel attached unanchored fexit in unprotected mode", n)
							ch <- r
							return
						}
						if !errors.Is(aerr, ErrAnchorUnpinned) {
							cancel()
							r.errMsg = fmt.Sprintf("held mount-check fail-nth=%d: static kernel err=%v, want ErrAnchorUnpinned", n, aerr)
							ch <- r
							return
						}
					}
				}
				cancel()
				if o != nil {
					if o.reaper != nil {
						o.reaper.stopAndWait(o.reaperCancel)
					}
					stateMu.Lock()
					activeOwner = nil
					stateMu.Unlock()
					_ = unix.Close(o.lockFD)
					_ = os.RemoveAll(o.dir)
				}
				unprotectedTrampolines.Store(false)
				explicitUnprotectedOptOut.Store(false)
				unprotectedWarned.Store(false)
				stateMu.Lock()
				unprotectedLogger = nil
				unprotectedWarnedFuncs = map[string]bool{}
				stateMu.Unlock()
				if o != nil && !fired {
					break
				}
			}
			ch <- r
		}()
		r := <-ch
		if r.errMsg != "" {
			t.Fatal(r.errMsg)
		}
		if r.hitUnprot == 0 {
			t.Fatal("held mount-check failslab sweep never triggered unprotected mode")
		}
	})

	// 6. Container-native single-fault failslab sweep with bpffs mounted at
	//    /run/keploy/host-bpffs and runningInContainerFn=true: proves a transient
	//    statfs/stat ENOMEM on /run/keploy/host-bpffs is NOT swallowed by the
	//    /sys/fs/bpf fallback's config error — every single fault recovers to
	//    Armed with 0 misleading mount hints and 0 stacked mounts.
	t.Run("container_native_failslab_sweep", func(t *testing.T) {
		resetStateForTest(t)
		t.Setenv(EnvHostBPFFS, "")
		runningInContainerFn = func() bool { return true }
		if err := os.MkdirAll(DefaultHostBPFFSPath, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", DefaultHostBPFFSPath, err)
		}
		mountedHere := false
		if countBPFFSMounts(DefaultHostBPFFSPath) == 0 {
			if err := unix.Mount("bpf", DefaultHostBPFFSPath, "bpf", 0, ""); err != nil {
				t.Fatalf("mount bpf at %s: %v", DefaultHostBPFFSPath, err)
			}
			mountedHere = true
		}
		defer func() {
			if mountedHere {
				_ = unix.Unmount(DefaultHostBPFFSPath, unix.MNT_DETACH)
			}
		}()
		baseHostMounts := countBPFFSMounts(DefaultHostBPFFSPath)
		baseSysMounts := countBPFFSMounts(DefaultNativeBPFFSPath)

		for _, kv := range [][2]string{{"task-filter", "N"}, {"ignore-gfp-wait", "N"}, {"probability", "0"}, {"times", "-1"}, {"verbose", "0"}} {
			_ = os.WriteFile(filepath.Join(failslabDir, kv[0]), []byte(kv[1]), 0o644)
		}
		defer func() {
			_ = os.WriteFile(filepath.Join(failslabDir, "times"), []byte("0"), 0o644)
			_ = os.WriteFile(filepath.Join(failslabDir, "probability"), []byte("0"), 0o644)
		}()

		type res struct {
			errMsg string
		}
		ch := make(chan res, 1)
		go func() {
			runtime.LockOSThread()
			nfd, oerr := unix.Open(fmt.Sprintf("/proc/self/task/%d/fail-nth", unix.Gettid()), unix.O_RDWR, 0)
			if oerr != nil {
				ch <- res{errMsg: "open fail-nth: " + oerr.Error()}
				return
			}
			defer func() { _ = unix.Close(nfd) }()
			arm := func(n int) { _, _ = unix.Pwrite(nfd, []byte(strconv.Itoa(n)), 0) }
			left := func() string {
				b := make([]byte, 16)
				k, _ := unix.Pread(nfd, b, 0)
				if k < 0 {
					k = 0
				}
				return strings.TrimSpace(string(b[:k]))
			}
			for n := 1; n <= 300; n++ {
				core, logs := observer.New(zap.WarnLevel)
				ctx, cancel := context.WithCancel(context.Background())
				arm(n)
				_ = Init(ctx, zap.New(core), ModeNative)
				lf := left()
				if lf != "0" {
					arm(0)
				}
				stateMu.Lock()
				o := activeOwner
				stateMu.Unlock()
				unprot := UnprotectedTrampolinesActive()
				if countBPFFSMounts(DefaultHostBPFFSPath) > baseHostMounts || countBPFFSMounts(DefaultNativeBPFFSPath) > baseSysMounts {
					cancel()
					ch <- res{errMsg: fmt.Sprintf("container-native fail-nth=%d stacked a bpffs mount", n)}
					return
				}
				if unprot || o == nil {
					cancel()
					ch <- res{errMsg: fmt.Sprintf("container-native single failslab fault fail-nth=%d left Init unprotected (armed=%v unprot=%v logs=%+v)", n, o != nil, unprot, logs.All())}
					return
				}
				cancel()
				if o.reaper != nil {
					o.reaper.stopAndWait(o.reaperCancel)
				}
				stateMu.Lock()
				activeOwner = nil
				stateMu.Unlock()
				_ = unix.Close(o.lockFD)
				_ = os.RemoveAll(o.dir)
				unprotectedTrampolines.Store(false)
				explicitUnprotectedOptOut.Store(false)
				unprotectedWarned.Store(false)
				stateMu.Lock()
				unprotectedLogger = nil
				unprotectedWarnedFuncs = map[string]bool{}
				stateMu.Unlock()
				if lf != "0" {
					break
				}
			}
			ch <- res{}
		}()
		r := <-ch
		if r.errMsg != "" {
			t.Fatal(r.errMsg)
		}
	})
}

// TestOnKernelLegacyMixedDirUnresolvedProgDead reproduces a dead grace dir left
// by an older agent (64-byte un-keyed target_funcs holding only
// ["unix_dgram_sendmsg"]) mixing resolved prog A and unresolved/legacy-named
// prog B on unix_stream_connect whose close hits -ENOMEM. Proves on 5.10, 5.15,
// and 5.19 that un-keyed legacy target_funcs entries NEVER satisfy per-pin
// lookup (kills M24!), the whole dir stays pinned, and 500 connect() calls
// survive with zero kernel panic.
func TestOnKernelLegacyMixedDirUnresolvedProgDead(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("requires root/CAP_BPF")
	}
	if _, err := os.Stat("/sys/kernel/debug/failslab"); err != nil {
		t.Skip("requires CONFIG_FAILSLAB=y")
	}
	if err := rlimit.RemoveMemlock(); err != nil {
		t.Fatal(err)
	}
	var stfs unix.Statfs_t
	if err := unix.Statfs(DefaultNativeBPFFSPath, &stfs); err != nil || stfs.Type != unix.BPF_FS_MAGIC {
		t.Skipf("%s is not a bpffs mount", DefaultNativeBPFFSPath)
	}
	resetStateForTest(t)

	probeP := makeCountingFexitProg(t, "leg_probe", "unix_stream_connect", nil)
	info, err := queryProgInfo(probeP.FD())
	_ = probeP.Close()
	if err != nil {
		t.Fatal(err)
	}
	if info.AttachBtfID != 0 {
		t.Skip("Linux 6.0+ populates attach_btf_id directly; legacy un-keyed target_funcs check applies to 5.10..5.19")
	}

	root := filepath.Join(DefaultNativeBPFFSPath, "keploy-bpfsafety-legmix-"+randomNonce())
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}

	hitB, err := ebpf.NewMap(&ebpf.MapSpec{Type: ebpf.Array, KeySize: 4, ValueSize: 8, MaxEntries: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = hitB.Close() }()

	pa := makeCountingFexitProg(t, "leg_a", "unix_dgram_sendmsg", nil)
	pb := makeCountingFexitProg(t, "leg_b", "unix_stream_connect", hitB)
	pbInfo, _ := pb.Info()
	pbID, _ := pbInfo.ID()

	owner, err := openGraceOwnerInRoot(root, zap.NewNop())
	if err != nil {
		_ = pa.Close()
		_ = pb.Close()
		t.Fatal(err)
	}
	stateMu.Lock()
	activeOwner = owner
	stateMu.Unlock()

	la, err := AttachTracing(pa, "legacy_test", "unix_dgram_sendmsg", "p")
	if err != nil {
		_ = pa.Close()
		_ = pb.Close()
		_ = unix.Close(owner.lockFD)
		t.Fatal(err)
	}

	// Pin prog B under an ELF-style name without per-pin record, and replace
	// target_funcs with a legacy 64-byte-slot un-keyed map ["unix_dgram_sendmsg"].
	other := makeCountingFexitProg(t, "leg_other", "unix_stream_connect", nil)
	defer func() { _ = other.Close() }()
	lo, err := link.AttachTracing(link.TracingOptions{Program: other})
	if err != nil {
		_ = la.Close()
		_ = pa.Close()
		_ = pb.Close()
		_ = unix.Close(owner.lockFD)
		t.Fatal(err)
	}
	defer func() { _ = lo.Close() }()

	if err := pb.Pin(filepath.Join(owner.dir, "000002-review-keploy_stream_connect-p")); err != nil {
		_ = la.Close()
		_ = pa.Close()
		_ = pb.Close()
		_ = unix.Close(owner.lockFD)
		t.Fatal(err)
	}
	lb, err := link.AttachTracing(link.TracingOptions{Program: pb})
	if err != nil {
		_ = la.Close()
		_ = pa.Close()
		_ = pb.Close()
		_ = unix.Close(owner.lockFD)
		t.Fatal(err)
	}

	// Write legacy 64-byte un-keyed target_funcs map containing ONLY "unix_dgram_sendmsg".
	tfPath := filepath.Join(owner.dir, targetFuncsFileName)
	_ = os.Remove(tfPath)
	legMap, err := ebpf.NewMap(&ebpf.MapSpec{Name: "grace_targets", Type: ebpf.Array, KeySize: 4, ValueSize: 64, MaxEntries: 128})
	if err != nil {
		t.Fatal(err)
	}
	var slot [64]byte
	copy(slot[:63], "unix_dgram_sendmsg")
	if err := legMap.Update(uint32(0), slot[:], ebpf.UpdateAny); err != nil {
		_ = legMap.Close()
		t.Fatal(err)
	}
	if err := legMap.Pin(tfPath); err != nil {
		_ = legMap.Close()
		t.Fatal(err)
	}
	_ = legMap.Close()

	connectUnixStreamHelper(t, 10)
	var c0 uint64
	_ = hitB.Lookup(uint32(0), &c0)

	_ = la.Close()
	injectFailslabClose(t, func() { _ = lb.Close() })
	_ = pa.Close()
	_ = pb.Close()
	_ = unix.Close(owner.lockFD)
	stateMu.Lock()
	activeOwner = nil
	stateMu.Unlock()

	time.Sleep(300 * time.Millisecond)
	connectUnixStreamHelper(t, 50)
	var c1 uint64
	_ = hitB.Lookup(uint32(0), &c1)
	if c1 <= c0 {
		t.Fatalf("expected stale image to keep calling B (c0=%d c1=%d)", c0, c1)
	}

	ro, err := openGraceOwnerInRoot(root, zap.NewNop())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { ro.reaper.closeNoopCache(); _ = unix.Close(ro.lockFD) }()

	for i := 0; i < 10; i++ {
		ro.reaper.round()
		time.Sleep(250 * time.Millisecond)
	}
	if _, err := os.Stat(owner.dir); err != nil {
		t.Fatalf("legacy mixed dir with un-keyed target_funcs was prematurely reaped (M24!): %v", err)
	}
	if pAlive, aerr := ebpf.NewProgramFromID(pbID); aerr != nil {
		t.Fatalf("unresolved prog B %d was freed despite legacy un-keyed target_funcs: %v", pbID, aerr)
	} else {
		_ = pAlive.Close()
	}
	connectUnixStreamHelper(t, 500)
}

// TestOnKernelSlotReuseConvergence proves that on Linux 6.12 (where
// bpf_prog_pack reuses first-fit 64-byte trampoline slots), a dead-owner grace
// directory converges and is reaped promptly instead of livelocking forever on
// the reused (key, addr) entry.
func TestOnKernelSlotReuseConvergence(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("requires root/CAP_BPF")
	}
	if err := rlimit.RemoveMemlock(); err != nil {
		t.Fatal(err)
	}
	var stfs unix.Statfs_t
	if err := unix.Statfs(DefaultNativeBPFFSPath, &stfs); err != nil || stfs.Type != unix.BPF_FS_MAGIC {
		t.Skipf("%s is not a bpffs mount", DefaultNativeBPFFSPath)
	}
	resetStateForTest(t)

	root := filepath.Join(DefaultNativeBPFFSPath, "keploy-bpfsafety-slot-"+randomNonce())
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })

	owner, err := openGraceOwnerInRoot(root, zap.NewNop())
	if err != nil {
		t.Fatal(err)
	}
	stateMu.Lock()
	activeOwner = owner
	stateMu.Unlock()

	p, err := ebpf.NewProgram(&ebpf.ProgramSpec{
		Name:       "slot_p",
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
		t.Fatal(err)
	}
	l, err := AttachTracing(p, "slot_test", "bpf_fentry_test1", "p")
	if err != nil {
		_ = p.Close()
		_ = unix.Close(owner.lockFD)
		t.Fatal(err)
	}
	_ = l.Close()
	_ = p.Close()
	_ = unix.Close(owner.lockFD)
	stateMu.Lock()
	activeOwner = nil
	stateMu.Unlock()

	ro, err := openGraceOwnerInRoot(root, zap.NewNop())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { ro.reaper.closeNoopCache(); _ = unix.Close(ro.lockFD) }()

	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		ro.reaper.round()
		if _, err := os.Stat(owner.dir); os.IsNotExist(err) {
			return
		}
		time.Sleep(300 * time.Millisecond)
	}
	t.Fatalf("dead grace dir %s livelocked under bpf_prog_pack slot reuse (>20s)", owner.dir)
}

// TestOnKernelModuleTargetDeadOwnerENOMEM proves that a kernel-module tracing
// target (tls.ko `tls_init`) on Linux 5.11..5.19 matches /proc/kallsyms via
// low-31-bit module BTF type ID, forces a rebuild via pinned no-op when
// unlink fails with -ENOMEM, and reaps cleanly with zero panic.
func TestOnKernelModuleTargetDeadOwnerENOMEM(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("requires root/CAP_BPF")
	}
	if _, err := os.Stat("/sys/kernel/debug/failslab"); err != nil {
		t.Skip("requires CONFIG_FAILSLAB=y")
	}
	if _, err := os.Stat("/tls.ko"); err != nil {
		t.Skipf("/tls.ko not bundled for this kernel: %v", err)
	}
	if err := rlimit.RemoveMemlock(); err != nil {
		t.Fatal(err)
	}
	img, err := os.ReadFile("/tls.ko")
	if err != nil {
		t.Fatalf("read /tls.ko: %v", err)
	}
	if err := unix.InitModule(img, ""); err != nil && !errors.Is(err, unix.EEXIST) {
		t.Fatalf("init_module tls: %v", err)
	}
	if _, err := os.Stat("/sys/kernel/btf/tls"); err != nil {
		t.Skipf("kernel has no /sys/kernel/btf/tls: %v", err)
	}
	resetStateForTest(t)

	root := filepath.Join(DefaultNativeBPFFSPath, "keploy-bpfsafety-mod-"+randomNonce())
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = cleanupAnchorsInRoot(context.Background(), zap.NewNop(), root, 5*time.Second, nil)
		_ = os.RemoveAll(root)
	})

	callTLSInit := func(n int) {
		for i := 0; i < n; i++ {
			fd, serr := unix.Socket(unix.AF_INET, unix.SOCK_STREAM, 0)
			if serr != nil {
				t.Fatal(serr)
			}
			_ = unix.SetsockoptString(fd, unix.IPPROTO_TCP, unix.TCP_ULP, "tls")
			_ = unix.Close(fd)
		}
	}

	hitMap, err := ebpf.NewMap(&ebpf.MapSpec{Type: ebpf.Array, KeySize: 4, ValueSize: 8, MaxEntries: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = hitMap.Close() }()

	p := makeCountingFexitProg(t, "mod_tls", "tls_init", hitMap)
	owner, err := openGraceOwnerInRoot(root, zap.NewNop())
	if err != nil {
		_ = p.Close()
		t.Fatal(err)
	}
	stateMu.Lock()
	activeOwner = owner
	stateMu.Unlock()

	l, err := AttachTracing(p, "mod_test", "tls_init", "p")
	if err != nil {
		_ = p.Close()
		_ = unix.Close(owner.lockFD)
		t.Fatalf("AttachTracing(tls_init): %v", err)
	}
	callTLSInit(10)
	var c0 uint64
	_ = hitMap.Lookup(uint32(0), &c0)
	if c0 < 10 {
		t.Fatalf("before detach tls_init hits = %d, want >= 10", c0)
	}

	injectFailslabClose(t, func() { _ = l.Close() })
	_ = p.Close()
	_ = unix.Close(owner.lockFD)
	stateMu.Lock()
	activeOwner = nil
	stateMu.Unlock()

	time.Sleep(300 * time.Millisecond)
	callTLSInit(50)
	var c1 uint64
	_ = hitMap.Lookup(uint32(0), &c1)
	if c1 <= c0 {
		t.Fatalf("expected stale tls.ko trampoline to keep calling prog (c0=%d c1=%d)", c0, c1)
	}

	ro, err := openGraceOwnerInRoot(root, zap.NewNop())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { ro.reaper.closeNoopCache(); _ = unix.Close(ro.lockFD) }()

	ro.reaper.round() // Pass 1
	ro.reaper.round() // Pass 2: MUST match tls.ko module key in kallsyms + force-rebuild (NOT reap with 0 no-ops!)
	if got := countPinnedProgs(ro.dir); got == 0 {
		t.Fatalf("expected reaper no-op pinned in %s for tls_init module target, found 0", ro.dir)
	}
	time.Sleep(300 * time.Millisecond)
	callTLSInit(50)
	var c2 uint64
	_ = hitMap.Lookup(uint32(0), &c2)
	if c2 != c1 {
		t.Fatalf("stale tls.ko trampoline STILL called prog after rebuild: c1=%d c2=%d", c1, c2)
	}

	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		ro.reaper.round()
		if _, err := os.Stat(owner.dir); os.IsNotExist(err) {
			break
		}
		time.Sleep(300 * time.Millisecond)
	}
	if _, err := os.Stat(owner.dir); !os.IsNotExist(err) {
		t.Fatalf("owner.dir still exists after tls.ko rebuild + kallsyms clear")
	}
	time.Sleep(500 * time.Millisecond)
	callTLSInit(100)
}

// TestOnKernelUnpinFailedAttach proves that if link.AttachTracing fails,
// AttachTracing removes the unused bpffs pin created by PinBeforeAttach (kills M15).
func TestOnKernelUnpinFailedAttach(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("requires root/CAP_BPF")
	}
	if err := rlimit.RemoveMemlock(); err != nil {
		t.Fatal(err)
	}
	var stfs unix.Statfs_t
	if err := unix.Statfs(DefaultNativeBPFFSPath, &stfs); err != nil || stfs.Type != unix.BPF_FS_MAGIC {
		t.Skipf("%s is not a bpffs mount", DefaultNativeBPFFSPath)
	}
	resetStateForTest(t)

	root := filepath.Join(DefaultNativeBPFFSPath, "keploy-bpfsafety-unpin-"+randomNonce())
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })

	owner, err := openGraceOwnerInRoot(root, zap.NewNop())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = unix.Close(owner.lockFD) }()
	stateMu.Lock()
	activeOwner = owner
	stateMu.Unlock()

	// Load a valid non-tracing Kprobe program: prog.Pin succeeds, then
	// link.AttachTracing fails -> AttachTracing MUST unpin the unused file!
	kprog, err := ebpf.NewProgram(&ebpf.ProgramSpec{
		Name:    "unpin_fail_kp",
		Type:    ebpf.Kprobe,
		License: "GPL",
		Instructions: asm.Instructions{
			asm.Mov.Imm(asm.R0, 0),
			asm.Return(),
		},
	})
	if err != nil {
		t.Fatalf("load kprobe: %v", err)
	}
	defer func() { _ = kprog.Close() }()

	l, err := AttachTracing(kprog, "unpin_test", "not_tracing", "p")
	if err == nil {
		_ = l.Close()
		t.Fatal("expected AttachTracing on Kprobe program to fail")
	}
	if got := countPinnedProgs(owner.dir); got != 0 {
		t.Fatalf("expected 0 leaked pins after failed AttachTracing (M15!), found %d", got)
	}
}

var _ = context.Background
