//go:build linux

package linux

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/cilium/ebpf"
	"go.keploy.io/server/v3/pkg/agent/bpfsafety"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// An orphan is found by the name the kernel keeps, at most 15 bytes: a
// program whose name is longer (k_udp_sock_release, k_ready_connect4) is
// matched by its cut name, never missed.
func TestAnOrphanIsMatchedByTheNameTheKernelKeeps(t *testing.T) {
	for in, want := range map[string]string{
		"k_connect4":         "k_connect4",
		"k_udp_sock_release": "k_udp_sock_rele",
		"k_ready_connect4":   "k_ready_connect",
		"k_ready_connect6":   "k_ready_connect",
	} {
		if got := kernelProgName(in); got != want {
			t.Errorf("kernelProgName(%q) = %q, want %q", in, got, want)
		}
	}
	for name := range keployBPFProgNames {
		if n := kernelProgName(name); len(n) > 15 {
			t.Errorf("%s: kept as %q, longer than the kernel keeps", name, n)
		}
	}
}

// The orphan cleanup's match is by the kernel's cut name: an attached
// k_ready_connect4 is k_ready_connect there, and found.
func TestAnOrphanCleanupMatchesTheCutName(t *testing.T) {
	if !orphanNamed("k_ready_connect", "k_ready_connect4") || !orphanNamed("k_udp_sock_rele", "k_udp_sock_release") || !orphanNamed("k_connect4", "k_connect4") {
		t.Fatal("an orphan attached under its cut name is not matched")
	}
	if orphanNamed("k_connect6", "k_connect4") {
		t.Fatal("another program matched")
	}
}

// TestOrphanCleanup_SkipsBPFLinkAttachedPrograms verifies that cgroup programs
// attached via a kernel bpf_link are never passed to RawDetachProgram, while
// genuine legacy raw-attached programs ARE detached. Covers both the primary
// link.Iterator path (which cgroup BPF_PROG_QUERY always takes because the
// kernel returns HaveLinkInfo()==false for cgroups) and a future kernel
// reporting HaveLinkInfo()==true directly.
func TestOrphanCleanup_SkipsBPFLinkAttachedPrograms(t *testing.T) {
	cases := []struct {
		name         string
		haveLinkInfo bool
		progs        []attachedCgroupProg
		liveLinks    map[uint32]struct{}
		wantDetached []uint32
	}{
		{
			name:         "cgroup query without link info falls back to link.Iterator",
			haveLinkInfo: false,
			progs: []attachedCgroupProg{
				{ID: 101, HasLink: false}, // owned by live cgroup bpf_link
				{ID: 202, HasLink: false}, // genuine raw-attached orphan
			},
			liveLinks:    map[uint32]struct{}{101: {}},
			wantDetached: []uint32{202},
		},
		{
			name:         "query reports link info directly",
			haveLinkInfo: true,
			progs: []attachedCgroupProg{
				{ID: 301, HasLink: true},  // owned by bpf_link
				{ID: 402, HasLink: false}, // genuine raw-attached orphan
			},
			wantDetached: []uint32{402},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			core, logs := observer.New(zapcore.InfoLevel)
			logger := zap.New(core)

			var detached []uint32
			ops := cleanupOps{
				hasOtherProc: func(int) bool { return false },
				queryPrograms: func(_ uintptr, at ebpf.AttachType) ([]attachedCgroupProg, bool, error) {
					if at == ebpf.AttachCGroupInet4Connect {
						return tc.progs, tc.haveLinkInfo, nil
					}
					return nil, tc.haveLinkInfo, nil
				},
				liveCgroupLinkIDs: func() (map[uint32]struct{}, error) {
					return tc.liveLinks, nil
				},
				lookupProgName: func(id uint32) (string, error) {
					return "k_connect4", nil
				},
				detachRawProgram: func(_ uintptr, id uint32, _ ebpf.AttachType) error {
					detached = append(detached, id)
					return nil
				},
			}

			cleaned := detectAndCleanOrphanedCgroupProgramsWithOps(logger, t.TempDir(), t.TempDir(), "self", ops)
			if cleaned != len(tc.wantDetached) {
				t.Fatalf("cleaned = %d, want %d (detached=%v)", cleaned, len(tc.wantDetached), detached)
			}
			if len(detached) != len(tc.wantDetached) || detached[0] != tc.wantDetached[0] {
				t.Fatalf("detached = %v, want %v", detached, tc.wantDetached)
			}

			// Verify INFO log assertions with exact uint32 matching:
			// - the bpf_link-owned program MUST NOT emit "Detaching orphaned BPF program from cgroup"
			// - the raw-attached program MUST emit it (positive control so the assertion is live)
			linkedID := tc.progs[0].ID
			rawID := tc.progs[1].ID
			sawRawLog := false
			for _, entry := range logs.All() {
				if entry.Message != "Detaching orphaned BPF program from cgroup" {
					continue
				}
				rawVal, ok := entry.ContextMap()["progID"]
				if !ok {
					t.Fatalf("Detaching INFO log missing progID field: %+v", entry.ContextMap())
				}
				gotID, ok := rawVal.(uint32)
				if !ok {
					t.Fatalf("progID field has type %T, want uint32", rawVal)
				}
				if gotID == linkedID {
					t.Fatalf("emitted misleading Detaching INFO log for bpf_link-owned program %d", linkedID)
				}
				if gotID == rawID {
					sawRawLog = true
				}
			}
			if !sawRawLog {
				t.Fatalf("expected Detaching INFO log for raw-attached program %d (positive control)", rawID)
			}
		})
	}
}

// TestOrphanCleanup_FlockProtectsLiveSiblingWhenProcIsBlind simulates a sibling
// Keploy instance whose PID is invisible in /proc (e.g. an isolated container
// PID namespace sharing /sys/fs/bpf): the sibling's held flock must prevent
// orphan cleanup from touching cgroup programs, and once the sibling exits (or
// is SIGKILLed) cleanup proceeds normally.
func TestOrphanCleanup_FlockProtectsLiveSiblingWhenProcIsBlind(t *testing.T) {
	lockDir := t.TempDir()

	sibling, err := acquireInstanceLock(lockDir, "sibling-in-other-pidns")
	if err != nil {
		t.Fatalf("acquire sibling lock: %v", err)
	}
	defer func() { _ = sibling.Release() }()

	detachCalls := 0
	ops := cleanupOps{
		// Simulate isolated PID namespace where /proc sees no other keploy.
		hasOtherProc: func(int) bool { return false },
		queryPrograms: func(_ uintptr, at ebpf.AttachType) ([]attachedCgroupProg, bool, error) {
			if at == ebpf.AttachCGroupInet4Connect {
				return []attachedCgroupProg{{ID: 555, HasLink: false}}, false, nil
			}
			return nil, false, nil
		},
		liveCgroupLinkIDs: func() (map[uint32]struct{}, error) { return map[uint32]struct{}{}, nil },
		lookupProgName:    func(uint32) (string, error) { return "k_connect4", nil },
		detachRawProgram: func(_ uintptr, _ uint32, _ ebpf.AttachType) error {
			detachCalls++
			return nil
		},
	}

	// While sibling holds its flock, orphan cleanup must skip completely.
	if n := detectAndCleanOrphanedCgroupProgramsWithOps(zap.NewNop(), t.TempDir(), lockDir, "new-agent", ops); n != 0 || detachCalls != 0 {
		t.Fatalf("cleanup ran while sibling flock was held: n=%d detachCalls=%d", n, detachCalls)
	}

	// Once sibling releases its lock, orphan cleanup must proceed and detach the raw orphan.
	if err := sibling.Release(); err != nil {
		t.Fatalf("release sibling lock: %v", err)
	}
	if n := detectAndCleanOrphanedCgroupProgramsWithOps(zap.NewNop(), t.TempDir(), lockDir, "new-agent", ops); n != 1 || detachCalls != 1 {
		t.Fatalf("cleanup after sibling release: n=%d detachCalls=%d, want 1, 1", n, detachCalls)
	}
}

// TestInstanceLock_SIGKILLAutoReleasesFlock spawns a real child subprocess that
// acquires an instance lock, then SIGKILLs it (without running Release()) and
// proves the kernel immediately releases the flock so hasOtherLiveInstanceLocks
// reports false.
func TestInstanceLock_SIGKILLAutoReleasesFlock(t *testing.T) {
	if os.Getenv("KEPLOY_TEST_FLOCK_HOLDER") == "1" {
		dir := os.Getenv("KEPLOY_TEST_LOCK_DIR")
		id := os.Getenv("KEPLOY_TEST_LOCK_ID")
		il, err := acquireInstanceLock(dir, id)
		if err != nil {
			fmt.Fprintf(os.Stderr, "helper acquire failed: %v\n", err)
			os.Exit(2)
		}
		defer func() { _ = il.Release() }()
		fmt.Println("LOCKED")
		// Block until stdin closes (or parent kills us via SIGKILL / Pdeathsig).
		_, _ = io.ReadAll(os.Stdin)
		os.Exit(0)
	}

	lockDir := t.TempDir()
	cmd := exec.Command(os.Args[0], "-test.run=^TestInstanceLock_SIGKILLAutoReleasesFlock$")
	cmd.Env = append(os.Environ(),
		"KEPLOY_TEST_FLOCK_HOLDER=1",
		"KEPLOY_TEST_LOCK_DIR="+lockDir,
		"KEPLOY_TEST_LOCK_ID=child-sigkill",
	)
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Setpgid:   true,
		Pdeathsig: syscall.SIGKILL,
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatalf("stdin pipe: %v", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("stdout pipe: %v", err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start helper subprocess: %v", err)
	}
	reaped := false
	t.Cleanup(func() {
		_ = stdin.Close()
		if !reaped && cmd.Process != nil {
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
			_ = cmd.Wait()
		}
	})

	buf := make([]byte, 32)
	n, err := stdout.Read(buf)
	if err != nil || string(buf[:n]) != "LOCKED\n" {
		t.Fatalf("helper did not lock cleanly: n=%d out=%q err=%v", n, string(buf[:n]), err)
	}

	if !hasOtherLiveInstanceLocks(lockDir, "self") {
		t.Fatal("expected live child subprocess lock to be detected")
	}

	// Simulate abrupt crash (SIGKILL): Release() never runs, kernel drops flock.
	if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil {
		t.Fatalf("SIGKILL child: %v", err)
	}
	_ = cmd.Wait()
	reaped = true

	if hasOtherLiveInstanceLocks(lockDir, "self") {
		t.Fatal("dead child flock still reported alive after SIGKILL")
	}

	// Backdate the dead lock directory past staleLockReapAge and verify the
	// next probe opportunistically removes the stale directory.
	deadPath := filepath.Join(lockDir, "child-sigkill"+instanceLockSuffix)
	old := time.Now().Add(-2 * staleLockReapAge)
	if err := os.Chtimes(deadPath, old, old); err != nil {
		t.Fatalf("chtimes stale lock dir: %v", err)
	}
	if hasOtherLiveInstanceLocks(lockDir, "self") {
		t.Fatal("stale lock dir reported alive")
	}
	if _, err := os.Stat(deadPath); !os.IsNotExist(err) {
		t.Fatalf("expected stale dead lock dir to be reaped, stat err=%v", err)
	}
}

// TestInstanceLock_ConcurrentAcquireAndProbe_NoRaces hammers acquireInstanceLock
// and hasOtherLiveInstanceLocks concurrently from multiple goroutines to prove
// neither the probe race (EWOULDBLOCK on acquire) nor the reclaim race (locking
// an unlinked directory when stale reaping runs with zero grace) occurs.
func TestInstanceLock_ConcurrentAcquireAndProbe_NoRaces(t *testing.T) {
	runStress := func(t *testing.T, reapAge time.Duration) {
		t.Helper()
		prevAge := staleLockReapAge
		staleLockReapAge = reapAge
		t.Cleanup(func() { staleLockReapAge = prevAge })

		lockDir := t.TempDir()
		const workers = 4
		const iters = 250

		var wg sync.WaitGroup
		errCh := make(chan error, workers*2)

		for w := 0; w < workers; w++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for i := 0; i < iters; i++ {
					il, err := acquireInstanceLock(lockDir, "")
					if err != nil {
						errCh <- fmt.Errorf("acquireInstanceLock failed: %w", err)
						return
					}
					// Verify our lock directory actually exists on disk and is
					// visible to siblings while held (catches unlinked-inode race).
					if !sameInode(il.fd, il.path) {
						_ = il.Release()
						errCh <- fmt.Errorf("acquired lock %s is unlinked or inode-mismatched", il.path)
						return
					}
					if !hasOtherLiveInstanceLocks(lockDir, "external-observer") {
						_ = il.Release()
						errCh <- fmt.Errorf("live lock %s not visible to observer", il.path)
						return
					}
					if err := il.Release(); err != nil {
						errCh <- fmt.Errorf("release lock %s: %w", il.path, err)
						return
					}
				}
			}()

			wg.Add(1)
			go func() {
				defer wg.Done()
				for i := 0; i < iters; i++ {
					_ = hasOtherLiveInstanceLocks(lockDir, "")
				}
			}()
		}

		wg.Wait()
		close(errCh)
		for err := range errCh {
			t.Fatal(err)
		}
	}

	t.Run("default reap grace", func(t *testing.T) {
		runStress(t, staleLockReapAge)
	})
	t.Run("zero reap grace forces active reclaim race", func(t *testing.T) {
		runStress(t, 0)
	})
}

// TestCleanOrphanedBPFPins_RespectsFlockWhenProcIsBlind verifies that
// CleanOrphanedBPFPins skips legacy pin removal when a sibling instance holds
// an exclusive flock even if /proc reports no other Keploy processes.
func TestCleanOrphanedBPFPins_RespectsFlockWhenProcIsBlind(t *testing.T) {
	lockDir := t.TempDir()
	sibling, err := acquireInstanceLock(lockDir, "live-sibling")
	if err != nil {
		t.Fatalf("acquire sibling lock: %v", err)
	}
	defer func() { _ = sibling.Release() }()

	tmpPin := filepath.Join(t.TempDir(), "keploy_fake_legacy_pin")
	if err := os.WriteFile(tmpPin, []byte("pin"), 0o600); err != nil {
		t.Fatalf("write fake pin: %v", err)
	}

	prevExtra := ExtraBPFPinPatterns
	ExtraBPFPinPatterns = []string{tmpPin}
	t.Cleanup(func() { ExtraBPFPinPatterns = prevExtra })

	// While sibling flock is held and /proc is blind, legacy pin must survive.
	cleanOrphanedBPFPins(zap.NewNop(), lockDir, "self", func(int) bool { return false })
	if _, err := os.Stat(tmpPin); err != nil {
		t.Fatalf("legacy pin was deleted while sibling flock was held: %v", err)
	}

	// After sibling releases its lock, legacy pin is cleaned.
	if err := sibling.Release(); err != nil {
		t.Fatalf("release sibling: %v", err)
	}
	cleanOrphanedBPFPins(zap.NewNop(), lockDir, "self", func(int) bool { return false })
	if _, err := os.Stat(tmpPin); !os.IsNotExist(err) {
		t.Fatalf("expected legacy pin to be removed after sibling released lock, stat err=%v", err)
	}
}

func TestIsKeployProcessIgnoresDetachedBPFHolder(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	// Copy test binary to a path whose basename contains "keploy" so
	// /proc/<pid>/exe matches keploy just like the real binary.
	fakeKeploy := filepath.Join(t.TempDir(), "keploy-holder-identity-test")
	src, err := os.Open(exe)
	if err != nil {
		t.Fatalf("open exe: %v", err)
	}
	dst, err := os.OpenFile(fakeKeploy, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
	if err != nil {
		_ = src.Close()
		t.Fatalf("create fake keploy: %v", err)
	}
	if _, err := io.Copy(dst, src); err != nil {
		_ = src.Close()
		_ = dst.Close()
		t.Fatalf("copy exe: %v", err)
	}
	_ = src.Close()
	_ = dst.Close()

	cmd := exec.Command(fakeKeploy)
	cmd.Args = []string{bpfsafety.DetachedHolderComm}
	cmd.Env = []string{
		"KEPLOY_INTERNAL_BPF_HOLD_MS=15000",
		"KEPLOY_INTERNAL_BPF_HOLD_COUNT=0",
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start detached holder: %v", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})

	pidStr := strconv.Itoa(cmd.Process.Pid)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if b, rerr := os.ReadFile(filepath.Join("/proc", pidStr, "comm")); rerr == nil && strings.TrimSpace(string(b)) == bpfsafety.DetachedHolderComm {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	if isKeployProcess(pidStr) {
		t.Fatalf("isKeployProcess(%s) = true for detached holder (comm=%s); want false so orphan cleanup is not blocked", pidStr, bpfsafety.DetachedHolderComm)
	}
}
