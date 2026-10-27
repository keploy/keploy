//go:build linux && pidmap_integration

// Root-only, beside pidmaps_integration_linux_test.go: the entries a skipped
// exit leaves in the PID maps, against keploy's own BPF object's maps.

package linux

import (
	"fmt"
	"os"
	"os/exec"
	"testing"

	"github.com/cilium/ebpf"
	"github.com/cilium/ebpf/btf"
	"github.com/cilium/ebpf/rlimit"
	"go.uber.org/zap"
	"golang.org/x/sys/unix"

	"go.keploy.io/server/v3/config"
)

// hooksWithObjects are hooks with keploy's object loaded, nothing attached:
// their maps are the real ones.
func hooksWithObjects(t *testing.T) *Hooks {
	t.Helper()
	if os.Geteuid() != 0 {
		t.Fatal("this test needs root (it loads BPF programs); run the test binary with sudo")
	}
	var st unix.Stat_t
	if err := unix.Stat("/proc/self/ns/pid", &st); err != nil || st.Ino != initialPIDNSInode {
		t.Fatal("this test runs in the initial PID namespace, whose numbers the PID maps hold")
	}
	if err := rlimit.RemoveMemlock(); err != nil {
		t.Fatal(err)
	}
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
	return h
}

// goneTGID is the TGID of a process that ran and was reaped, and that no
// process has since.
func goneTGID(t *testing.T) uint32 {
	t.Helper()
	cmd := exec.Command("true")
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	if err := unix.Kill(cmd.Process.Pid, 0); err != unix.ESRCH {
		t.Fatalf("TGID %d was reaped but kill(0) says %v: given to another process already", cmd.Process.Pid, err)
	}
	return uint32(cmd.Process.Pid)
}

// The sweep deletes what a process whose exit the kernel skipped left in the
// PID maps once it is gone, with or without a start time, and nothing of a
// live process's.
func TestTheSweepDeletesTheEntriesOfProcessesThatAreGone(t *testing.T) {
	h := hooksWithObjects(t)
	targets, excluded, starts := h.objects.TargetNamespacePids, h.objects.ExcludedPids, h.objects.PidStartTimes
	gone, goneNoStart, alive := goneTGID(t), goneTGID(t), uint32(os.Getpid())
	put := func(m *ebpf.Map, k uint32, v any) {
		t.Helper()
		if err := m.Update(k, v, ebpf.UpdateAny); err != nil {
			t.Fatal(err)
		}
	}
	put(targets, gone, uint8(1))
	put(starts, gone, uint64(12345))
	put(excluded, goneNoStart, uint8(1))
	put(targets, alive, uint8(1))
	put(starts, alive, uint64(67890))

	n, zombies, err := sweepGonePIDs(targets, excluded, starts, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(zombies) != 0 {
		t.Errorf("zombies %v among the TGIDs, want none", zombies)
	}
	if n != 2 {
		t.Errorf("cleared %d TGIDs, want the 2 gone ones", n)
	}
	var v8 uint8
	var v64 uint64
	if targets.Lookup(gone, &v8) == nil || starts.Lookup(gone, &v64) == nil {
		t.Errorf("TGID %d is gone, but its entries are still there", gone)
	}
	if excluded.Lookup(goneNoStart, &v8) == nil {
		t.Errorf("TGID %d is gone, but its excluded_pids entry is still there", goneNoStart)
	}
	if targets.Lookup(alive, &v8) != nil || starts.Lookup(alive, &v64) != nil {
		t.Errorf("TGID %d is alive, and the sweep deleted its entries", alive)
	}
}

// A process with entries that has exited but is not reaped yet (a zombie)
// keeps its entries, and is counted, so that another sweep comes: once it
// is reaped, that sweep deletes them. The child is held a zombie by waiting
// on it without reaping (WNOWAIT): no clock.
func TestTheSweepKeepsAZombiesEntriesUntilItIsReaped(t *testing.T) {
	h := hooksWithObjects(t)
	targets, excluded, starts := h.objects.TargetNamespacePids, h.objects.ExcludedPids, h.objects.PidStartTimes
	cmd := exec.Command("true")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pid := cmd.Process.Pid
	var info unix.Siginfo
	if err := unix.Waitid(unix.P_PID, pid, &info, unix.WEXITED|unix.WNOWAIT, nil); err != nil {
		t.Fatal(err)
	}
	zombie := uint32(pid)
	if err := targets.Update(zombie, uint8(1), ebpf.UpdateAny); err != nil {
		t.Fatal(err)
	}
	if err := starts.Update(zombie, uint64(4242), ebpf.UpdateAny); err != nil {
		t.Fatal(err)
	}
	n, zombies, err := sweepGonePIDs(targets, excluded, starts, nil)
	if err != nil {
		t.Fatal(err)
	}
	var v8 uint8
	if n != 0 || fmt.Sprint(zombies) != fmt.Sprint([]uint32{zombie}) || targets.Lookup(zombie, &v8) != nil {
		t.Fatalf("with TGID %d a zombie: cleared %d, zombies %v, its entry there: %v; want 0, [%d], true", zombie, n, zombies, targets.Lookup(zombie, &v8) == nil, zombie)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatal(err)
	}
	// The next sweep looks at the zombie alone.
	n, zombies, err = sweepGonePIDs(targets, excluded, starts, zombies)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 || len(zombies) != 0 || targets.Lookup(zombie, &v8) == nil {
		t.Fatalf("with TGID %d reaped: cleared %d, zombies %v, its entry gone: %v; want 1, none, true", zombie, n, zombies, targets.Lookup(zombie, &v8) != nil)
	}
}

// The kernel says whether it counts the runs it skips as its own struct
// bpf_prog_info says it does: with a recursion_misses member (5.12 onwards,
// or a kernel it was backported to), by the kernel's BTF.
func TestTheKernelSaysWhetherItCountsSkippedRuns(t *testing.T) {
	h := hooksWithObjects(t)
	spec, err := btf.LoadKernelSpec()
	if err != nil {
		t.Fatalf("read the kernel's BTF, which keploy's objects need anyway: %v", err)
	}
	var info *btf.Struct
	if err := spec.TypeByName("bpf_prog_info", &info); err != nil {
		t.Fatal(err)
	}
	want := false
	for _, m := range info.Members {
		if m.Name == "recursion_misses" {
			want = true
		}
	}
	got, err := recursionMissesCounted(h.objects.K_processExit)
	if err != nil || got != want {
		t.Fatalf("recursionMissesCounted = %v, %v; want %v, nil (bpf_prog_info has recursion_misses: %v)", got, err, want, want)
	}
}
