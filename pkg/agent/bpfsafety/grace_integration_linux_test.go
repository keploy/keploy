//go:build linux && integration

package bpfsafety

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
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
//  3. After owner's flock releases, a SECOND process's reaper writes the shared
//     16-byte BPF_MAP_TYPE_ARRAY CLOCK_BOOTTIME stamp on real bpffs, and a
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

	// Simulate 3 distinct short-lived reaper processes sharing the pinned BPF_MAP_TYPE_ARRAY stamp on bpffs!
	now := clockBoottimeNs()
	makeReaper := func() *graceReaper {
		r := newGraceReaper(root, GraceDirPrefix+"other-agent", 15*time.Minute, 3*time.Minute, 1*time.Minute, zap.NewNop())
		r.bootNowNs = func() uint64 { return now }
		return r
	}

	// Reaper process 1: pins first_free_boottime BPF_MAP_TYPE_ARRAY in owner.dir.
	makeReaper().round()
	if _, err := os.Stat(filepath.Join(owner.dir, stampFileName)); err != nil {
		t.Fatalf("expected pinned bpffs stamp map %s: %v", stampFileName, err)
	}

	// Reaper process 2 (15m01s later): reads pinned map, updates deadlineHitBootNs, does NOT reap yet.
	now += uint64(15*time.Minute + time.Second)
	makeReaper().round()
	alive2, err := ebpf.NewProgramFromID(progID)
	if err != nil {
		t.Fatalf("prog %d reaped on first confirm pass: %v", progID, err)
	}
	_ = alive2.Close()

	// Reaper process 3 (1m01s later): reads pinned map, sees confirmGap met, unpins & removes dir!
	now += uint64(1*time.Minute + time.Second)
	makeReaper().round()

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

// TestDetachedHolderKeepsProgLoadedAfterParentExits proves on the real Linux
// kernel that when host bpffs is unavailable (OSS detached-holder mode):
//  1. A child process attaches a real tracing program via
//     AttachTracingWithInProcessHold, runs GracefulShutdown (with W=2s in test
//     short-grace mode), and EXITS IMMEDIATELY (< 1s, zero blocking hold and
//     zero inherited stdout/stderr pipe hang).
//  2. AFTER the child process has completely exited, ebpf.NewProgramFromID
//     confirms the BPF program is STILL LOADED in the kernel (held by the
//     detached holder process).
//  3. Once W=2s elapses and the detached holder exits, ebpf.NewProgramFromID
//     returns ENOENT.
func TestDetachedHolderKeepsProgLoadedAfterParentExits(t *testing.T) {
	if os.Getenv("KEPLOY_TEST_DETACHED_HOLDER_CHILD") == "1" {
		if err := rlimit.RemoveMemlock(); err != nil {
			t.Fatalf("RemoveMemlock: %v", err)
		}
		AllowShortGraceForTest(t)
		ResetShutdownForTest(t)
		prog, err := ebpf.NewProgram(&ebpf.ProgramSpec{
			Name:       "grace_holder_test",
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
			_ = prog.Close()
			t.Fatalf("prog.Info: %v", err)
		}
		progID, ok := info.ID()
		if !ok {
			_ = prog.Close()
			t.Fatal("prog.Info returned no ID")
		}
		l, err := AttachTracingWithInProcessHold(prog)
		if err != nil {
			_ = prog.Close()
			t.Fatalf("AttachTracingWithInProcessHold: %v", err)
		}
		RegisterDetachHook(func() {
			_ = l.Close()
			_ = prog.Close()
		})
		shutdownStartNs := time.Now().UnixNano()
		GracefulShutdown(context.Background(), zap.NewNop(), 2*time.Second)
		shutdownEndNs := time.Now().UnixNano()
		fmt.Printf("HOLDER_TEST_PROG_ID=%d\n", progID)
		fmt.Printf("HOLDER_TEST_SHUTDOWN_NS=%d\n", shutdownStartNs)
		fmt.Printf("HOLDER_TEST_DONE_NS=%d\n", shutdownEndNs)
		return
	}

	if os.Geteuid() != 0 {
		t.Skip("requires root/CAP_BPF")
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	cmd := exec.Command(exe, "-test.run=^TestDetachedHolderKeepsProgLoadedAfterParentExits$", "-test.v")
	cmd.Env = append(os.Environ(), "KEPLOY_TEST_DETACHED_HOLDER_CHILD=1")

	out, err := cmd.CombinedOutput()
	parentObservedExit := time.Now()
	if err != nil {
		t.Fatalf("child exited with error: %v\noutput:\n%s", err, string(out))
	}

	var (
		progID          ebpf.ProgramID
		shutdownStartNs int64
		shutdownEndNs   int64
	)
	for _, line := range strings.Split(string(out), "\n") {
		switch {
		case strings.HasPrefix(line, "HOLDER_TEST_PROG_ID="):
			raw := strings.TrimSpace(strings.TrimPrefix(line, "HOLDER_TEST_PROG_ID="))
			id, perr := strconv.ParseUint(raw, 10, 32)
			if perr != nil {
				t.Fatalf("parse progID %q: %v", raw, perr)
			}
			progID = ebpf.ProgramID(id)
		case strings.HasPrefix(line, "HOLDER_TEST_SHUTDOWN_NS="):
			raw := strings.TrimSpace(strings.TrimPrefix(line, "HOLDER_TEST_SHUTDOWN_NS="))
			v, perr := strconv.ParseInt(raw, 10, 64)
			if perr != nil {
				t.Fatalf("parse shutdownStartNs %q: %v", raw, perr)
			}
			shutdownStartNs = v
		case strings.HasPrefix(line, "HOLDER_TEST_DONE_NS="):
			raw := strings.TrimSpace(strings.TrimPrefix(line, "HOLDER_TEST_DONE_NS="))
			v, perr := strconv.ParseInt(raw, 10, 64)
			if perr != nil {
				t.Fatalf("parse shutdownEndNs %q: %v", raw, perr)
			}
			shutdownEndNs = v
		}
	}
	if progID == 0 || shutdownStartNs == 0 || shutdownEndNs == 0 {
		t.Fatalf("child output missing expected markers:\n%s", string(out))
	}
	shutdownStart := time.Unix(0, shutdownStartNs)
	shutdownDur := time.Duration(shutdownEndNs - shutdownStartNs)
	if shutdownDur >= 1*time.Second {
		t.Fatalf("GracefulShutdown blocked for %v (>= 1s) instead of handing off to detached 2s holder immediately", shutdownDur)
	}
	if parentObservedExit.Sub(shutdownStart) >= 1500*time.Millisecond {
		t.Fatalf("child took %v after GracefulShutdown to exit (pipe held by holder?)", parentObservedExit.Sub(shutdownStart))
	}

	// Child has ALREADY EXITED! Wait until W - 0.5s (1.5s after shutdownStart)
	// and verify the prog is STILL loaded thanks to the detached 2s holder.
	if rem := time.Until(shutdownStart.Add(1500 * time.Millisecond)); rem > 0 {
		time.Sleep(rem)
	}
	alive, err := ebpf.NewProgramFromID(progID)
	if err != nil {
		t.Fatalf("prog %d disappeared before W-0.5s (1.5s of 2s hold) after parent exited: %v", progID, err)
	}
	_ = alive.Close()

	// After W + 3s (5s after shutdownStart), the detached 2s holder MUST have
	// exited and the prog MUST be freed (ENOENT).
	deadline := shutdownStart.Add(5 * time.Second)
	for time.Now().Before(deadline) {
		p, err := ebpf.NewProgramFromID(progID)
		if err != nil {
			return // gone cleanly after detached holder exited!
		}
		_ = p.Close()
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("prog %d still loaded at W+3s (5s) after detached 2s holder was spawned", progID)
}

var _ = context.Background
