//go:build linux && pidmap_integration

// Root-only, beside pidmaps_integration_linux_test.go: the entries a skipped
// exit leaves in the PID maps, against keploy's own BPF object's maps.

package linux

import (
	"fmt"
	"os"
	"os/exec"
	"sort"
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
// PID maps and in a keploy instance (instance_of_tgid) once it is gone, with
// or without a start time, and nothing of a live process's.
func TestTheSweepDeletesTheEntriesOfProcessesThatAreGone(t *testing.T) {
	h := hooksWithObjects(t)
	targets, excluded, starts, instances := h.objects.TargetNamespacePids, h.objects.ExcludedPids, h.objects.PidStartTimes, h.objects.InstanceOfTgid
	gone, goneNoStart, goneMember, alive := goneTGID(t), goneTGID(t), goneTGID(t), uint32(os.Getpid())
	put := func(m *ebpf.Map, k uint32, v any) {
		t.Helper()
		if err := m.Update(k, v, ebpf.UpdateAny); err != nil {
			t.Fatal(err)
		}
	}
	put(targets, gone, uint8(1))
	put(starts, gone, uint64(12345))
	put(excluded, goneNoStart, uint8(1))
	put(instances, gone, uint32(3))
	put(instances, goneMember, uint32(4))
	put(targets, alive, uint8(1))
	put(starts, alive, uint64(67890))
	put(instances, alive, uint32(5))

	n, zombies, err := sweepGonePIDs(targets, excluded, starts, instances, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(zombies) != 0 {
		t.Errorf("zombies %v among the TGIDs, want none", zombies)
	}
	if n != 3 {
		t.Errorf("cleared %d TGIDs, want the 3 gone ones", n)
	}
	var v32 uint32
	if instances.Lookup(gone, &v32) == nil || instances.Lookup(goneMember, &v32) == nil {
		t.Errorf("TGIDs %d and %d are gone, but an instance_of_tgid entry of theirs is still there", gone, goneMember)
	}
	if instances.Lookup(alive, &v32) != nil {
		t.Errorf("TGID %d is alive, and the sweep took it out of its keploy instance", alive)
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

// zombieChild starts a process that exits at once and holds it a zombie by
// waiting on it without reaping it (WNOWAIT): no clock. Its Wait reaps it.
func zombieChild(t *testing.T) (*exec.Cmd, uint32) {
	t.Helper()
	cmd := exec.Command("true")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Wait() })
	var info unix.Siginfo
	if err := unix.Waitid(unix.P_PID, cmd.Process.Pid, &info, unix.WEXITED|unix.WNOWAIT, nil); err != nil {
		t.Fatal(err)
	}
	return cmd, uint32(cmd.Process.Pid)
}

// A process with entries that has exited but is not reaped yet (a zombie)
// keeps its entries, and is counted, so that another sweep comes: once it
// is reaped, that sweep deletes them. So does one whose only entry is its
// keploy instance's (instance_of_tgid).
func TestTheSweepKeepsAZombiesEntriesUntilItIsReaped(t *testing.T) {
	h := hooksWithObjects(t)
	targets, excluded, starts, instances := h.objects.TargetNamespacePids, h.objects.ExcludedPids, h.objects.PidStartTimes, h.objects.InstanceOfTgid
	cmd, zombie := zombieChild(t)
	memberCmd, member := zombieChild(t)
	if err := targets.Update(zombie, uint8(1), ebpf.UpdateAny); err != nil {
		t.Fatal(err)
	}
	if err := starts.Update(zombie, uint64(4242), ebpf.UpdateAny); err != nil {
		t.Fatal(err)
	}
	if err := instances.Update(member, uint32(6), ebpf.UpdateAny); err != nil {
		t.Fatal(err)
	}
	n, zombies, err := sweepGonePIDs(targets, excluded, starts, instances, nil)
	if err != nil {
		t.Fatal(err)
	}
	sort.Slice(zombies, func(i, j int) bool { return zombies[i] < zombies[j] })
	want := []uint32{zombie, member}
	sort.Slice(want, func(i, j int) bool { return want[i] < want[j] })
	var v8 uint8
	var v32 uint32
	if n != 0 || fmt.Sprint(zombies) != fmt.Sprint(want) || targets.Lookup(zombie, &v8) != nil || instances.Lookup(member, &v32) != nil {
		t.Fatalf("with TGIDs %d and %d zombies: cleared %d, zombies %v, entries there: %v, %v; want 0, %v, true, true", zombie, member, n, zombies, targets.Lookup(zombie, &v8) == nil, instances.Lookup(member, &v32) == nil, want)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatal(err)
	}
	if err := memberCmd.Wait(); err != nil {
		t.Fatal(err)
	}
	// The next sweep looks at the zombies alone.
	n, zombies, err = sweepGonePIDs(targets, excluded, starts, instances, zombies)
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 || len(zombies) != 0 || targets.Lookup(zombie, &v8) == nil || instances.Lookup(member, &v32) == nil {
		t.Fatalf("with TGIDs %d and %d reaped: cleared %d, zombies %v, entries gone: %v, %v; want 2, none, true, true", zombie, member, n, zombies, targets.Lookup(zombie, &v8) != nil, instances.Lookup(member, &v32) != nil)
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
