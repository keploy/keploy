//go:build linux && pidmap_integration

package linux

import (
	"bufio"
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/cilium/ebpf"
	"github.com/cilium/ebpf/asm"
	"github.com/cilium/ebpf/link"
	"github.com/cilium/ebpf/rlimit"
	"go.keploy.io/server/v3/pkg/agent"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
	"golang.org/x/sys/unix"
)

const (
	orphanIntStageEnv     = "KEPLOY_TEST_ORPHAN_CG_STAGE"
	orphanIntCgroupEnv    = "KEPLOY_TEST_ORPHAN_CG_PATH"
	orphanIntWantCgOwner  = "KEPLOY_TEST_ORPHAN_CG_WANT_CG_OWNER"
	orphanIntWantHasOther = "KEPLOY_TEST_ORPHAN_CG_WANT_HAS_OTHER"
	orphanIntWantClean    = "KEPLOY_TEST_ORPHAN_CG_WANT_CLEANED"
)

func testDummySockOps(t *testing.T, name string) *ebpf.Program {
	t.Helper()
	p, err := ebpf.NewProgram(&ebpf.ProgramSpec{
		Name:       name,
		Type:       ebpf.SockOps,
		AttachType: ebpf.AttachCGroupSockOps,
		License:    "GPL",
		Instructions: asm.Instructions{
			asm.Mov.Imm(asm.R0, 1),
			asm.Return(),
		},
	})
	if err != nil {
		t.Fatalf("load dummy sockops %s: %v", name, err)
	}
	return p
}

func testDummyConnect4(t *testing.T, name string) *ebpf.Program {
	t.Helper()
	p, err := ebpf.NewProgram(&ebpf.ProgramSpec{
		Name:       name,
		Type:       ebpf.CGroupSockAddr,
		AttachType: ebpf.AttachCGroupInet4Connect,
		License:    "GPL",
		Instructions: asm.Instructions{
			asm.Mov.Imm(asm.R0, 1),
			asm.Return(),
		},
	})
	if err != nil {
		t.Fatalf("load dummy connect4 %s: %v", name, err)
	}
	return p
}

func testMakeTempCgroup(t *testing.T, suffix string) string {
	t.Helper()
	root, err := agent.DetectCgroupPath(zap.NewNop())
	if err != nil {
		t.Skipf("no cgroup2 mount available: %v", err)
	}
	cg := filepath.Join(root, fmt.Sprintf("keploy-orphan-int-%d-%s-%d", os.Getpid(), suffix, time.Now().UnixNano()))
	if err := os.Mkdir(cg, 0755); err != nil {
		t.Skipf("cannot create temporary sub-cgroup %s: %v", cg, err)
	}
	t.Cleanup(func() { _ = os.Remove(cg) })
	return cg
}

func testAttachLegacy(t *testing.T, cgPath string, prog *ebpf.Program, at ebpf.AttachType) func() {
	t.Helper()
	f, err := os.Open(cgPath)
	if err != nil {
		t.Fatalf("open cgroup %s: %v", cgPath, err)
	}
	defer func() { _ = f.Close() }()
	// BPF_F_ALLOW_MULTI = 2
	if err := link.RawAttachProgram(link.RawAttachProgramOptions{
		Target:  int(f.Fd()),
		Program: prog,
		Attach:  at,
		Flags:   2,
	}); err != nil {
		t.Fatalf("RawAttachProgram(%s): %v", at, err)
	}
	return func() {
		cf, err := os.Open(cgPath)
		if err != nil {
			return
		}
		defer func() { _ = cf.Close() }()
		_ = link.RawDetachProgram(link.RawDetachProgramOptions{
			Target:  int(cf.Fd()),
			Program: prog,
			Attach:  at,
		})
	}
}

func testCountAttached(t *testing.T, cgPath string, at ebpf.AttachType) int {
	t.Helper()
	f, err := os.Open(cgPath)
	if err != nil {
		t.Fatalf("open cgroup %s: %v", cgPath, err)
	}
	defer func() { _ = f.Close() }()
	progs, err := queryAttachedPrograms(f.Fd(), at)
	if err != nil {
		t.Fatalf("queryAttachedPrograms(%s): %v", at, err)
	}
	return len(progs)
}

// runInIsolatedPIDNS re-execs this test in a fresh PID+mount namespace (with
// its own /proc mount so PID 1 is the only process visible in /proc — matching
// a Kubernetes hostPID: false pod mounting host /sys/fs/cgroup).
// wantHasOtherGlobal, when true, also asserts HasOtherKeployProcesses(1)==true;
// when false, we assert only the cgroup-scoped owner check so unrelated keploy
// lanes running concurrently on the same host kernel cannot cause flakes.
func runInIsolatedPIDNS(t *testing.T, cgPath string, wantCgOwner bool, wantHasOtherGlobal bool, wantCleaned int) {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestOrphanCgroupCleanupAcrossPIDNamespaces$", "-test.v", "-test.count=1")
	cmd.Env = append(os.Environ(),
		orphanIntStageEnv+"=isolated-cleaner",
		orphanIntCgroupEnv+"="+cgPath,
		fmt.Sprintf("%s=%v", orphanIntWantCgOwner, wantCgOwner),
		fmt.Sprintf("%s=%v", orphanIntWantHasOther, wantHasOtherGlobal),
		fmt.Sprintf("%s=%d", orphanIntWantClean, wantCleaned),
	)
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Cloneflags: syscall.CLONE_NEWPID | syscall.CLONE_NEWNS,
		Pdeathsig:  syscall.SIGKILL,
	}
	out, err := cmd.CombinedOutput()
	if err != nil || !bytes.Contains(out, []byte("--- PASS: TestOrphanCgroupCleanupAcrossPIDNamespaces")) {
		t.Fatalf("isolated PID-ns cleaner failed (%v):\n%s", err, out)
	}
}

func runIsolatedCleanerChild(t *testing.T) {
	// Make mount propagation private and mount a fresh /proc for this new PID
	// namespace so /proc sees ONLY this process (PID 1), identically to a
	// non-hostPID pod.
	if err := unix.Mount("", "/", "", unix.MS_REC|unix.MS_PRIVATE, ""); err != nil {
		t.Fatalf("MS_PRIVATE: %v", err)
	}
	if err := unix.Mount("proc", "/proc", "proc", 0, ""); err != nil {
		t.Fatalf("mount /proc in new PID namespace: %v", err)
	}
	if err := rlimit.RemoveMemlock(); err != nil {
		t.Fatalf("RemoveMemlock: %v", err)
	}
	if os.Getpid() != 1 {
		t.Fatalf("expected PID 1 in isolated PID namespace, got %d", os.Getpid())
	}
	if hasOtherKeployProcInProcfs(os.Getpid()) {
		t.Fatalf("expected /proc in isolated PID namespace to hide host processes")
	}

	cgPath := os.Getenv(orphanIntCgroupEnv)
	wantCgOwner := os.Getenv(orphanIntWantCgOwner) == "true"
	wantHasOtherGlobal := os.Getenv(orphanIntWantHasOther) == "true"
	wantCleaned := 0
	_, _ = fmt.Sscanf(os.Getenv(orphanIntWantClean), "%d", &wantCleaned)

	cgID, err := cgroupIDOfPath(cgPath)
	if err != nil {
		t.Fatalf("cgroupIDOfPath(%s): %v", cgPath, err)
	}
	gotCgOwner, err := realHasOtherCgroupOwnerMap(cgID, nil)
	if err != nil {
		t.Fatalf("realHasOtherCgroupOwnerMap(%d): %v", cgID, err)
	}
	if gotCgOwner != wantCgOwner {
		t.Fatalf("realHasOtherCgroupOwnerMap(%d) = %v, want %v", cgID, gotCgOwner, wantCgOwner)
	}
	if wantHasOtherGlobal {
		if !HasOtherKeployProcesses(os.Getpid()) {
			t.Fatalf("HasOtherKeployProcesses(1) = false, want true")
		}
	}

	core, logs := observer.New(zap.InfoLevel)
	logger := zap.New(core)
	gotCleaned := DetectAndCleanOrphanedCgroupPrograms(logger, cgPath)
	if gotCleaned != wantCleaned {
		t.Fatalf("DetectAndCleanOrphanedCgroupPrograms(%s) = %d, want %d (logs: %v)", cgPath, gotCleaned, wantCleaned, logs.All())
	}
	if wantCleaned == 0 {
		for _, e := range logs.All() {
			if strings.Contains(e.Message, "Detaching orphaned BPF program") {
				t.Fatalf("unexpected detach INFO log when wantCleaned=0: %v", e)
			}
		}
	}
}

func runCrashedLegacyHolderChild(t *testing.T) {
	if err := rlimit.RemoveMemlock(); err != nil {
		t.Fatalf("RemoveMemlock: %v", err)
	}
	cgPath := os.Getenv(orphanIntCgroupEnv)
	tok, err := acquireCgroupOwnerToken(cgPath)
	if err != nil {
		t.Fatalf("acquireCgroupOwnerToken: %v", err)
	}
	// Intentionally do NOT close tok or detach on exit: parent sends SIGKILL
	// to prove the kernel automatically destroys keploy_cg_owner while the
	// legacy BPF_PROG_ATTACH remains orphaned for cleanup.
	_ = tok
	p := testDummyConnect4(t, "k_connect4")
	_ = testAttachLegacy(t, cgPath, p, ebpf.AttachCGroupInet4Connect)
	fmt.Println("LEGACY_ORPHAN_READY")
	select {}
}

// TestOrphanCgroupCleanupAcrossPIDNamespaces proves on a real Linux kernel:
//  1. A live owner holding keploy_cg_owner protects BOTH bpf_link and legacy
//     BPF_PROG_ATTACH programs from a non-hostPID (CLONE_NEWPID) starter.
//  2. Even without keploy_cg_owner (pre-fix binary), live bpf_link programs
//     are never touched and emit zero bogus detach logs.
//  3. After a holder is SIGKILLed, its keploy_cg_owner map disappears in the
//     kernel and its orphaned legacy BPF_PROG_ATTACH IS cleaned up.
//  4. A live owner on cgroup A does not block cleaning a genuine legacy orphan
//     on a different cgroup B.
func TestOrphanCgroupCleanupAcrossPIDNamespaces(t *testing.T) {
	switch os.Getenv(orphanIntStageEnv) {
	case "isolated-cleaner":
		runIsolatedCleanerChild(t)
		return
	case "crashed-legacy-holder":
		runCrashedLegacyHolderChild(t)
		return
	}

	if os.Geteuid() != 0 {
		t.Skip("requires root for cgroup BPF and CLONE_NEWPID")
	}
	if err := rlimit.RemoveMemlock(); err != nil {
		t.Skipf("RemoveMemlock: %v", err)
	}

	t.Run("live_owner_protects_bpf_link_and_legacy_across_pid_ns", func(t *testing.T) {
		cg := testMakeTempCgroup(t, "live-owner")
		tok, err := acquireCgroupOwnerToken(cg)
		if err != nil {
			t.Fatalf("acquireCgroupOwnerToken: %v", err)
		}
		defer tok.Close()

		pSock := testDummySockOps(t, "k_sockops")
		defer func() { _ = pSock.Close() }()
		lSock, err := link.AttachCgroup(link.CgroupOptions{
			Path:    cg,
			Attach:  ebpf.AttachCGroupSockOps,
			Program: pSock,
		})
		if err != nil {
			t.Fatalf("AttachCgroup k_sockops: %v", err)
		}
		defer func() { _ = lSock.Close() }()

		pConn := testDummyConnect4(t, "k_connect4")
		defer func() { _ = pConn.Close() }()
		detachLegacy := testAttachLegacy(t, cg, pConn, ebpf.AttachCGroupInet4Connect)
		defer detachLegacy()

		runInIsolatedPIDNS(t, cg, true, true, 0)

		if n := testCountAttached(t, cg, ebpf.AttachCGroupSockOps); n != 1 {
			t.Fatalf("k_sockops bpf_link count after isolated cleaner = %d, want 1", n)
		}
		if n := testCountAttached(t, cg, ebpf.AttachCGroupInet4Connect); n != 1 {
			t.Fatalf("k_connect4 legacy count after isolated cleaner = %d, want 1", n)
		}
	})

	t.Run("prefix_bpf_link_without_owner_map_is_untouched_and_silent", func(t *testing.T) {
		cg := testMakeTempCgroup(t, "prefix-link")
		pSock := testDummySockOps(t, "k_sockops")
		defer func() { _ = pSock.Close() }()
		lSock, err := link.AttachCgroup(link.CgroupOptions{
			Path:    cg,
			Attach:  ebpf.AttachCGroupSockOps,
			Program: pSock,
		})
		if err != nil {
			t.Fatalf("AttachCgroup k_sockops: %v", err)
		}
		defer func() { _ = lSock.Close() }()

		// No keploy_cg_owner token held (simulates pre-fix host agent).
		runInIsolatedPIDNS(t, cg, false, false, 0)

		if n := testCountAttached(t, cg, ebpf.AttachCGroupSockOps); n != 1 {
			t.Fatalf("k_sockops bpf_link count after isolated cleaner = %d, want 1", n)
		}
	})

	t.Run("sigkilled_legacy_orphan_is_cleaned", func(t *testing.T) {
		cg := testMakeTempCgroup(t, "sigkill-orphan")
		cmd := exec.Command(os.Args[0], "-test.run=^TestOrphanCgroupCleanupAcrossPIDNamespaces$", "-test.count=1")
		cmd.Env = append(os.Environ(),
			orphanIntStageEnv+"=crashed-legacy-holder",
			orphanIntCgroupEnv+"="+cg,
		)
		stdout, err := cmd.StdoutPipe()
		if err != nil {
			t.Fatal(err)
		}
		if err := cmd.Start(); err != nil {
			t.Fatalf("start crashed-legacy-holder: %v", err)
		}
		ready := false
		sc := bufio.NewScanner(stdout)
		for sc.Scan() {
			if strings.Contains(sc.Text(), "LEGACY_ORPHAN_READY") {
				ready = true
				break
			}
		}
		if !ready {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
			t.Fatal("crashed-legacy-holder never became ready")
		}

		// Hard-kill the holder: kernel immediately destroys its unpinned
		// keploy_cg_owner map while the legacy BPF_PROG_ATTACH stays attached.
		if err := cmd.Process.Signal(syscall.SIGKILL); err != nil {
			t.Fatalf("SIGKILL holder: %v", err)
		}
		_ = cmd.Wait()

		if n := testCountAttached(t, cg, ebpf.AttachCGroupInet4Connect); n != 1 {
			t.Fatalf("expected 1 orphaned legacy k_connect4 before cleanup, got %d", n)
		}

		// Cleaner in a separate PID namespace must detect no live owner and
		// detach the 1 orphaned legacy program.
		runInIsolatedPIDNS(t, cg, false, false, 1)

		if n := testCountAttached(t, cg, ebpf.AttachCGroupInet4Connect); n != 0 {
			t.Fatalf("expected 0 attached k_connect4 after cleanup, got %d", n)
		}
	})

	t.Run("live_owner_on_other_cgroup_does_not_block_orphan_cleanup", func(t *testing.T) {
		cgA := testMakeTempCgroup(t, "other-a")
		cgB := testMakeTempCgroup(t, "other-b")

		tokA, err := acquireCgroupOwnerToken(cgA)
		if err != nil {
			t.Fatalf("acquireCgroupOwnerToken(cgA): %v", err)
		}
		defer tokA.Close()

		pA := testDummyConnect4(t, "k_connect4")
		defer func() { _ = pA.Close() }()
		detachA := testAttachLegacy(t, cgA, pA, ebpf.AttachCGroupInet4Connect)
		defer detachA()

		// Simulate an orphan on cgB (no owner token for cgB).
		pB := testDummyConnect4(t, "k_connect4")
		defer func() { _ = pB.Close() }()
		detachB := testAttachLegacy(t, cgB, pB, ebpf.AttachCGroupInet4Connect)
		defer detachB()

		// Cleaner checks cgB: HasOtherKeployProcesses is true globally (cgA
		// holds a token), yet DetectAndCleanOrphanedCgroupPrograms(cgB) scopes
		// by cgroup ID and cleans cgB's 1 orphan while preserving cgA!
		runInIsolatedPIDNS(t, cgB, false, true, 1)

		if n := testCountAttached(t, cgB, ebpf.AttachCGroupInet4Connect); n != 0 {
			t.Fatalf("expected 0 attached k_connect4 on cgB after cleanup, got %d", n)
		}
		if n := testCountAttached(t, cgA, ebpf.AttachCGroupInet4Connect); n != 1 {
			t.Fatalf("expected cgA's k_connect4 untouched (=1), got %d", n)
		}
	})
}
