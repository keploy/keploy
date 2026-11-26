//go:build linux && pidmap_integration

// Root-only kernel integration test for orphan cgroup BPF program cleanup and
// bpffs directory flock liveness. Runs in the pidmap_integration tier alongside
// instance_integration_linux_test.go.

package linux

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/cilium/ebpf"
	"github.com/cilium/ebpf/asm"
	"github.com/cilium/ebpf/link"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
	"golang.org/x/sys/unix"
)

func loadMinimalCgroupConnect4Prog(t *testing.T) *ebpf.Program {
	t.Helper()
	spec := &ebpf.ProgramSpec{
		Name:       "k_connect4",
		Type:       ebpf.CGroupSockAddr,
		AttachType: ebpf.AttachCGroupInet4Connect,
		License:    "GPL",
		Instructions: asm.Instructions{
			asm.Mov.Imm(asm.R0, 1),
			asm.Return(),
		},
	}
	prog, err := ebpf.NewProgram(spec)
	if err != nil {
		t.Fatalf("load minimal k_connect4 cgroup program: %v", err)
	}
	t.Cleanup(func() { _ = prog.Close() })
	return prog
}

// TestOrphanCleanup_RealKernel_DetachesRawKeepsBPFLink attaches two k_connect4
// programs to a real temporary cgroup v2 leaf — one via a kernel bpf_link and
// one via legacy RawAttachProgram — runs orphan cleanup, and proves ONLY the
// raw-attached program is detached while the bpf_link-attached program remains
// attached.
func TestOrphanCleanup_RealKernel_DetachesRawKeepsBPFLink(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("requires root/CAP_BPF")
	}
	cgDir := filepath.Join("/sys/fs/cgroup", fmt.Sprintf("keploy-cleanup-test-%d", os.Getpid()))
	if err := os.Mkdir(cgDir, 0o755); err != nil {
		t.Fatalf("create temp cgroup %s: %v", cgDir, err)
	}
	t.Cleanup(func() { _ = os.Remove(cgDir) })

	linkedProg := loadMinimalCgroupConnect4Prog(t)
	rawProg := loadMinimalCgroupConnect4Prog(t)

	linkedInfo, err := linkedProg.Info()
	if err != nil {
		t.Fatalf("linkedProg.Info: %v", err)
	}
	linkedID, ok := linkedInfo.ID()
	if !ok {
		t.Fatal("linkedProg.Info returned no ID")
	}

	rawInfo, err := rawProg.Info()
	if err != nil {
		t.Fatalf("rawProg.Info: %v", err)
	}
	rawID, ok := rawInfo.ID()
	if !ok {
		t.Fatal("rawProg.Info returned no ID")
	}

	// 1. Attach via bpf_link (modern attachment — must survive orphan cleanup).
	cgLink, err := link.AttachCgroup(link.CgroupOptions{
		Path:    cgDir,
		Attach:  ebpf.AttachCGroupInet4Connect,
		Program: linkedProg,
	})
	if err != nil {
		t.Fatalf("link.AttachCgroup: %v", err)
	}
	t.Cleanup(func() { _ = cgLink.Close() })

	// 2. Attach via legacy RawAttachProgram with AllowMulti so both coexist.
	cgFile, err := os.Open(cgDir)
	if err != nil {
		t.Fatalf("open cgroup %s: %v", cgDir, err)
	}
	t.Cleanup(func() { _ = cgFile.Close() })

	if err := link.RawAttachProgram(link.RawAttachProgramOptions{
		Target:  int(cgFile.Fd()),
		Program: rawProg,
		Attach:  ebpf.AttachCGroupInet4Connect,
		Flags:   unix.BPF_F_ALLOW_MULTI,
	}); err != nil {
		t.Fatalf("link.RawAttachProgram: %v", err)
	}
	t.Cleanup(func() {
		_ = link.RawDetachProgram(link.RawDetachProgramOptions{
			Target:  int(cgFile.Fd()),
			Program: rawProg,
			Attach:  ebpf.AttachCGroupInet4Connect,
		})
	})

	// Confirm live link enumeration finds linkedID and NOT rawID.
	liveLinks, err := queryLiveCgroupLinkProgIDs()
	if err != nil {
		t.Fatalf("queryLiveCgroupLinkProgIDs: %v", err)
	}
	if _, found := liveLinks[uint32(linkedID)]; !found {
		t.Fatalf("queryLiveCgroupLinkProgIDs missed linked prog %d (got %v)", linkedID, liveLinks)
	}
	if _, found := liveLinks[uint32(rawID)]; found {
		t.Fatalf("queryLiveCgroupLinkProgIDs unexpectedly included raw-attached prog %d", rawID)
	}

	// Run orphan cleanup with /proc disabled and an empty lockDir so only the
	// bpf_link filter protects linkedProg. Wrap detachRawProgram and capture
	// INFO logs so this integration test FAILS if the bpf_link filter is
	// removed (otherwise the kernel's ENOENT on RawDetachProgram would hide it).
	core, logs := observer.New(zapcore.InfoLevel)
	logger := zap.New(core)
	var detachAttempts []uint32

	ops := defaultCleanupOps()
	ops.hasOtherProc = func(int) bool { return false }
	realDetach := ops.detachRawProgram
	ops.detachRawProgram = func(cgroupFD uintptr, progID uint32, attachType ebpf.AttachType) error {
		detachAttempts = append(detachAttempts, progID)
		return realDetach(cgroupFD, progID, attachType)
	}

	cleaned := detectAndCleanOrphanedCgroupProgramsWithOps(logger, cgDir, t.TempDir(), "self", ops)
	if cleaned != 1 {
		t.Fatalf("cleaned = %d, want 1 (only raw-attached program)", cleaned)
	}
	if len(detachAttempts) != 1 || detachAttempts[0] != uint32(rawID) {
		t.Fatalf("detachRawProgram called for %v, want ONLY rawID=%d (never linkedID=%d)", detachAttempts, rawID, linkedID)
	}

	sawRawLog := false
	for _, entry := range logs.All() {
		if entry.Message != "Detaching orphaned BPF program from cgroup" {
			continue
		}
		gotID, ok := entry.ContextMap()["progID"].(uint32)
		if !ok {
			t.Fatalf("Detaching INFO log progID has unexpected type %T", entry.ContextMap()["progID"])
		}
		if gotID == uint32(linkedID) {
			t.Fatalf("emitted misleading Detaching INFO log for bpf_link-owned program %d", linkedID)
		}
		if gotID == uint32(rawID) {
			sawRawLog = true
		}
	}
	if !sawRawLog {
		t.Fatalf("expected Detaching INFO log for raw-attached program %d", rawID)
	}

	// Query the cgroup afterward: linkedProg MUST still be attached, rawProg MUST be gone.
	remaining, _, err := queryAttachedPrograms(cgFile.Fd(), ebpf.AttachCGroupInet4Connect)
	if err != nil {
		t.Fatalf("queryAttachedPrograms after cleanup: %v", err)
	}
	var sawLinked, sawRaw bool
	for _, ap := range remaining {
		if ap.ID == uint32(linkedID) {
			sawLinked = true
		}
		if ap.ID == uint32(rawID) {
			sawRaw = true
		}
	}
	if !sawLinked {
		t.Fatalf("bpf_link-attached program %d was detached (remaining=%+v)", linkedID, remaining)
	}
	if sawRaw {
		t.Fatalf("raw-attached orphan program %d was NOT detached (remaining=%+v)", rawID, remaining)
	}
}

// TestInstanceLock_RealBPFFS_FlockWorks verifies directory flock(2) works on a
// real mounted bpffs directory under /sys/fs/bpf.
func TestInstanceLock_RealBPFFS_FlockWorks(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("requires root/CAP_BPF")
	}
	if fi, err := os.Stat("/sys/fs/bpf"); err != nil || !fi.IsDir() {
		t.Skip("/sys/fs/bpf not mounted")
	}
	lockDir := filepath.Join("/sys/fs/bpf", fmt.Sprintf("keploy-lock-test-%d", os.Getpid()))
	if err := os.Mkdir(lockDir, 0o700); err != nil {
		t.Fatalf("mkdir on bpffs %s: %v", lockDir, err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(lockDir) })

	il, err := acquireInstanceLock(lockDir, "")
	if err != nil {
		t.Fatalf("acquireInstanceLock on bpffs: %v", err)
	}
	t.Cleanup(func() { _ = il.Release() })

	if !hasOtherLiveInstanceLocks(lockDir, "other-instance") {
		t.Fatal("expected held bpffs lock to be visible to another instance")
	}
	if hasOtherLiveInstanceLocks(lockDir, il.ID()) {
		t.Fatal("instance must not observe its own bpffs lock as a sibling")
	}
	if err := il.Release(); err != nil {
		t.Fatalf("Release on bpffs: %v", err)
	}
	if hasOtherLiveInstanceLocks(lockDir, "other-instance") {
		t.Fatal("released bpffs lock still reported alive")
	}
}
