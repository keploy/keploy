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
//  1. PinBeforeAttach creates a pinned anchor link under <root>/anchors/<func>.link
//     and <root>/anchors/<func>.prog BEFORE attaching the real program.
//  2. CleanupAnchors REFUSES to remove anchors while any live agent holds flock.
//  3. Once the live agent exits, CleanupAnchors calmly detaches the anchor link,
//     waits for /proc/kallsyms to confirm the trampoline image is freed, and
//     unpins both .link and .prog cleanly.
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
		_ = cleanupAnchorsInRoot(context.Background(), zap.NewNop(), root, 5*time.Second, nil)
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

	// Agent exits cleanly: anchor MUST STILL survive agent exit!
	_ = l.Close()
	_ = prog.Close()
	_ = unix.Close(owner.lockFD)
	stateMu.Lock()
	activeOwner = nil
	stateMu.Unlock()
	if got := CountAnchorLinks(root); got != 1 {
		t.Fatalf("anchor disappeared on agent exit (CountAnchorLinks=%d, want 1)", got)
	}

	// Calm removal with no live agents MUST detach link, verify kallsyms clears, and remove both .link and .prog!
	if err := cleanupAnchorsInRoot(context.Background(), zap.NewNop(), root, 10*time.Second, nil); err != nil {
		t.Fatalf("calm CleanupAnchors failed: %v", err)
	}
	if _, err := os.Stat(linkPath); !os.IsNotExist(err) {
		t.Fatalf("anchor link still exists after calm removal: %v", err)
	}
	if _, err := os.Stat(progPath); !os.IsNotExist(err) {
		t.Fatalf("anchor prog still exists after calm removal: %v", err)
	}
}

// TestOnKernelAnchorCalmRemovalDetectsStaleAfterENOMEM proves on a real Linux
// debug kernel with CONFIG_FAILSLAB=y that if detaching the last anchor link
// during calm CleanupAnchors hits -ENOMEM (the last-link ftrace_ops UAF state):
//  1. CleanupAnchors detects that the trampoline image persisted in /proc/kallsyms.
//  2. It sets StaleTrampolineDetected()=true and returns a drain-and-reboot error.
//  3. It KEEPS the anchor program (.prog) pinned in host bpffs so the stale
//     trampoline image never faults on a freed struct bpf_prog.
func TestOnKernelAnchorCalmRemovalDetectsStaleAfterENOMEM(t *testing.T) {
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

	root := filepath.Join(DefaultNativeBPFFSPath, "keploy-bpfsafety-anchor-enomem-"+randomNonce())
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", root, err)
	}
	// Do NOT remove root if stale .prog must remain pinned for the rest of this VM boot.

	// Attach an auxiliary link on bpf_fentry_test2 so tr->refcnt stays >= 1 when
	// failslab injects -ENOMEM on the anchor's close(linkFD). On Linux 6.12 with
	// KASAN, -ENOMEM on the true last link triggers an immediate kernel slab-UAF
	// inside bpf_trampoline_put -> ftrace_ops_trampoline before close() returns
	// (the exact kernel bug anchor links exist to prevent). With refcnt >= 1,
	// failslab still fails bpf_trampoline_unlink_prog with -ENOMEM and leaves the
	// snapshotted trampoline image (still calling the anchor prog) stuck in
	// /proc/kallsyms so we can verify CleanupAnchors' stale detection, alert, and
	// fail-closed -prog retention end to end on the KASAN debug kernel.
	auxProg, err := ebpf.NewProgram(&ebpf.ProgramSpec{
		Name:       "anchor_aux_keep",
		Type:       ebpf.Tracing,
		AttachType: ebpf.AttachTraceFEntry,
		AttachTo:   "bpf_fentry_test2",
		License:    "GPL",
		Instructions: asm.Instructions{
			asm.Mov.Imm(asm.R0, 0),
			asm.Return(),
		},
	})
	if err != nil {
		t.Fatalf("load auxProg: %v", err)
	}
	defer func() { _ = auxProg.Close() }()
	auxLink, err := link.AttachTracing(link.TracingOptions{Program: auxProg})
	if err != nil {
		t.Fatalf("attach auxLink: %v", err)
	}
	defer func() {
		// Closing auxLink without failslab forces a clean trampoline rebuild
		// that retires the stale image so subsequent tests in this VM stay clean.
		_ = auxLink.Close()
		time.Sleep(500 * time.Millisecond)
		_ = os.RemoveAll(root)
	}()

	owner, err := openGraceOwnerInRoot(root, zap.NewNop())
	if err != nil {
		t.Fatalf("openGraceOwnerInRoot: %v", err)
	}
	owner.ensureAnchor("bpf_fentry_test2", zap.NewNop())
	_ = unix.Close(owner.lockFD)

	progPath := filepath.Join(root, AnchorsSubdir, "bpf_fentry_test2"+anchorProgSuffix)
	if _, err := os.Stat(progPath); err != nil {
		t.Fatalf("expected anchor prog %s: %v", progPath, err)
	}

	var failFD int
	hooks := &anchorCleanupHooks{
		beforeLinkClose: func(funcName string) {
			runtime.LockOSThread()
			tidFailPath := fmt.Sprintf("/proc/self/task/%d/make-it-fail", unix.Gettid())
			fd, oerr := unix.Open(tidFailPath, unix.O_WRONLY, 0)
			if oerr == nil {
				failFD = fd
				_ = os.WriteFile(filepath.Join(failslabDir, "task-filter"), []byte("Y"), 0o644)
				_ = os.WriteFile(filepath.Join(failslabDir, "ignore-gfp-wait"), []byte("N"), 0o644)
				_ = os.WriteFile(filepath.Join(failslabDir, "probability"), []byte("100"), 0o644)
				_ = os.WriteFile(filepath.Join(failslabDir, "interval"), []byte("1"), 0o644)
				_ = os.WriteFile(filepath.Join(failslabDir, "times"), []byte("20"), 0o644)
				_ = os.WriteFile(filepath.Join(failslabDir, "space"), []byte("0"), 0o644)
				_, _ = unix.Pwrite(failFD, []byte("1\n"), 0)
			}
		},
		afterLinkClose: func(funcName string) {
			if failFD > 0 {
				_, _ = unix.Pwrite(failFD, []byte("0\n"), 0)
				_ = unix.Close(failFD)
				failFD = 0
			}
			_ = os.WriteFile(filepath.Join(failslabDir, "times"), []byte("0"), 0o644)
			_ = os.WriteFile(filepath.Join(failslabDir, "probability"), []byte("0"), 0o644)
			runtime.UnlockOSThread()
		},
	}

	err = cleanupAnchorsInRoot(context.Background(), zap.NewNop(), root, 3*time.Second, hooks)
	if err == nil || !strings.Contains(err.Error(), "drain and reboot") {
		t.Fatalf("expected drain-and-reboot stale error when anchor detach hits ENOMEM, got %v", err)
	}
	if !StaleTrampolineDetected() {
		t.Fatal("expected StaleTrampolineDetected()=true after stale anchor detach")
	}
	if _, serr := os.Stat(progPath); serr != nil {
		t.Fatalf("anchor prog %s was UNPINNED despite surviving stale trampoline image: %v", progPath, serr)
	}
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
	// On Linux 5.12+ (dynamic bpf_tramp_image), owner.dir remains pinned until
	// the old image clears from /proc/kallsyms; on Linux 5.10..5.11 (static
	// in-place trampoline), Pass 2 forces a synchronous rebuild + 2s
	// BPF_ENABLE_STATS flat run_cnt check and may already reap owner.dir.
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

var _ = context.Background
