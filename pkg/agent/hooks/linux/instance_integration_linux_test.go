//go:build linux && pidmap_integration

// Root-only, beside skipwatch_integration_linux_test.go: keploy instances
// (Hooks.SetInstance, EndInstance, InstanceLost) against keploy's own BPF
// object's maps, with k_instance_fork attached as SetInstance attaches it.

package linux

import (
	"bufio"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"

	"github.com/cilium/ebpf"
	"go.uber.org/zap"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/agent"
)

// instanceHooks are hooksWithObjects whose k_instance_fork link, which
// SetInstance attaches, is closed before their objects.
func instanceHooks(t *testing.T) *Hooks {
	t.Helper()
	h := hooksWithObjects(t)
	t.Cleanup(func() {
		if err := h.closeInstanceFork(); err != nil {
			t.Error(err)
		}
	})
	return h
}

// worker is a shell that forks one process each time a line is written to
// it and writes that process's TGID back: a test worker that starts what it
// tests. Its process group is killed at the test's end.
type worker struct {
	cmd *exec.Cmd
	in  io.WriteCloser
	out *bufio.Reader
}

func startWorker(t *testing.T) *worker {
	t.Helper()
	cmd := exec.Command("sh", "-c", `while read l; do sleep 1000 & echo $!; done`)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	in, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		_ = in.Close()
		_ = cmd.Wait()
	})
	return &worker{cmd: cmd, in: in, out: bufio.NewReader(out)}
}

func (w *worker) tgid() uint32 { return uint32(w.cmd.Process.Pid) }

// fork has the worker fork a process and returns its TGID.
func (w *worker) fork(t *testing.T) uint32 {
	t.Helper()
	if _, err := io.WriteString(w.in, "\n"); err != nil {
		t.Fatal(err)
	}
	line, err := w.out.ReadString('\n')
	if err != nil {
		t.Fatalf("read the TGID of the worker's fork: %v", err)
	}
	n, err := strconv.ParseUint(strings.TrimSpace(line), 10, 32)
	if err != nil {
		t.Fatalf("the worker's fork: %q is no TGID", line)
	}
	return uint32(n)
}

// instanceOf is the instance instance_of_tgid holds tgid in, 0 for none.
func instanceOf(t *testing.T, h *Hooks, tgid uint32) uint32 {
	t.Helper()
	var id uint32
	if err := h.objects.InstanceOfTgid.Lookup(tgid, &id); err != nil {
		if errors.Is(err, ebpf.ErrKeyNotExist) {
			return 0
		}
		t.Fatal(err)
	}
	return id
}

// A worker given an instance is in it, and so is each process it forks from
// then on; nothing else is. The id is not given again while a process is in
// it, nor is a process in one given another. Its end takes every one of
// them out, and says how many, and the id can then be given again.
func TestAWorkersForksAreInItsInstanceUntilItEnds(t *testing.T) {
	h := instanceHooks(t)
	w := startWorker(t)
	before := w.fork(t) // forked before the claim: never the worker's
	if err := h.SetInstance(w.tgid(), 7); err != nil {
		t.Fatal(err)
	}
	child, second := w.fork(t), w.fork(t)
	for _, p := range []uint32{w.tgid(), child, second} {
		if got := instanceOf(t, h, p); got != 7 {
			t.Errorf("process %d (the worker %d or its fork after the claim) is in instance %d, want 7", p, w.tgid(), got)
		}
	}
	if got := instanceOf(t, h, before); got != 0 {
		t.Errorf("process %d, forked before the worker's claim, is in instance %d, want none", before, got)
	}
	if got := instanceOf(t, h, uint32(os.Getpid())); got != 0 {
		t.Errorf("the test process, which no worker forked, is in instance %d, want none", got)
	}

	other := startWorker(t)
	if err := h.SetInstance(other.tgid(), 7); !errors.Is(err, agent.ErrInstanceHeld) {
		t.Errorf("SetInstance of the held instance 7: %v, want ErrInstanceHeld", err)
	}
	if err := h.SetInstance(child, 8); err == nil || !strings.Contains(err.Error(), "already in keploy instance 7") {
		t.Errorf("SetInstance of process %d, in instance 7, to 8: %v, want it refused as already in 7", child, err)
	}
	if got := instanceOf(t, h, child); got != 7 {
		t.Errorf("after the refusal process %d is in instance %d, want 7", child, got)
	}
	if got := instanceOf(t, h, other.tgid()); got != 0 {
		t.Errorf("after the refusal process %d is in instance %d, want none", other.tgid(), got)
	}
	lost, err := h.InstanceLost(7)
	if err != nil {
		t.Fatal(err)
	}
	if lost != (agent.InstanceLoss{}) {
		t.Errorf("instance 7 lost %+v of its forks, want none", lost)
	}

	members, err := instanceMembers(h.objects.InstanceOfTgid, 7)
	if err != nil {
		t.Fatal(err)
	}
	n, err := h.EndInstance(7)
	if err != nil {
		t.Fatal(err)
	}
	if n != len(members) || n < 3 {
		t.Errorf("EndInstance(7) took %d processes out, want the %d in it (%v), the worker and its 2 forks among them", n, len(members), members)
	}
	for _, p := range []uint32{w.tgid(), child, second} {
		if got := instanceOf(t, h, p); got != 0 {
			t.Errorf("after its end process %d is in instance %d, want none", p, got)
		}
	}
	if after := w.fork(t); instanceOf(t, h, after) != 0 {
		t.Errorf("the worker's fork %d after its instance ended is in instance %d, want none", after, instanceOf(t, h, after))
	}
	if err := h.SetInstance(other.tgid(), 7); err != nil {
		t.Errorf("SetInstance of the ended instance 7: %v, want it given", err)
	}
	if got := instanceOf(t, h, other.tgid()); got != 7 {
		t.Errorf("process %d is in instance %d, want 7", other.tgid(), got)
	}
}

// A process that is gone, or a zombie, is not given an instance and leaves
// no entry; nor are an id the kernel does not keep or a TGID that is none.
func TestAnInstanceIsNotGivenToAProcessThatIsGone(t *testing.T) {
	h := instanceHooks(t)
	gone := goneTGID(t)
	if err := h.SetInstance(gone, 9); err == nil {
		t.Errorf("SetInstance of the reaped process %d: given, want refused", gone)
	}
	_, zombie := zombieChild(t)
	if err := h.SetInstance(zombie, 9); err == nil || !strings.Contains(err.Error(), "exited before") {
		t.Errorf("SetInstance of the zombie %d: %v, want it refused as exited", zombie, err)
	}
	for _, p := range []uint32{gone, zombie} {
		if got := instanceOf(t, h, p); got != 0 {
			t.Errorf("process %d is gone, and is in instance %d, want none", p, got)
		}
	}
	live := startWorker(t)
	// Each refusal says which number is wrong.
	noID, noTGID := "is not one the kernel keeps", "is not a process's TGID"
	for _, c := range []struct {
		tgid, id uint32
		why      string
	}{{live.tgid(), 0, noID}, {live.tgid(), agent.MaxInstanceID + 1, noID}, {0, 9, noTGID}, {1 << 31, 9, noTGID}} {
		if err := h.SetInstance(c.tgid, c.id); err == nil || !strings.Contains(err.Error(), c.why) {
			t.Errorf("SetInstance(%d, %d): %v, want it refused as one that %s", c.tgid, c.id, err, c.why)
		}
		if got := instanceOf(t, h, c.tgid); got != 0 {
			t.Errorf("after the refusal process %d is in instance %d, want none", c.tgid, got)
		}
	}
	for _, id := range []uint32{0, agent.MaxInstanceID + 1} {
		if _, err := h.EndInstance(id); err == nil || !strings.Contains(err.Error(), noID) {
			t.Errorf("EndInstance(%d): %v, want it refused as one that %s", id, err, noID)
		}
		if _, err := h.InstanceLost(id); err == nil || !strings.Contains(err.Error(), noID) {
			t.Errorf("InstanceLost(%d): %v, want it refused as one that %s", id, err, noID)
		}
	}
	if err := h.SetInstance(live.tgid(), agent.MaxInstanceID); err != nil {
		t.Errorf("SetInstance of the highest id %d: %v, want it given", agent.MaxInstanceID, err)
	}
}

// What the kernel counted lost for an id is the instance's that had it
// last: the id's count reads as it is, and is zeroed when the id is given
// again.
func TestAnIdsLostForksAreZeroedWhenItIsGiven(t *testing.T) {
	h := instanceHooks(t)
	was := instanceLost{Forks: 3, LastTgid: 42, LastErr: -7}
	if err := h.objects.InstanceLost.Update(uint32(5), was, ebpf.UpdateAny); err != nil {
		t.Fatal(err)
	}
	got, err := h.InstanceLost(5)
	if err != nil {
		t.Fatal(err)
	}
	if got != (agent.InstanceLoss{Forks: 3, LastTgid: 42, LastErr: -7}) {
		t.Errorf("InstanceLost(5) = %+v, want what the kernel counted, %+v", got, was)
	}
	w := startWorker(t)
	if err := h.SetInstance(w.tgid(), 5); err != nil {
		t.Fatal(err)
	}
	if got, err := h.InstanceLost(5); err != nil || got != (agent.InstanceLoss{}) {
		t.Errorf("after SetInstance InstanceLost(5) = %+v, %v; want none", got, err)
	}
}

// A later claim of an id is the id with the claim's generation
// (agent.InstanceValue): the worker and each process it forks are in that
// whole value, which their connections carry. A process of the id's earlier
// claim still in it holds the id whatever the generation asked; the end
// takes out the id's processes; the kernel's count of lost forks is the
// id's, read and zeroed by the value of any of its claims.
func TestALaterClaimOfAnIdCarriesItsGeneration(t *testing.T) {
	h := instanceHooks(t)
	first, later := agent.InstanceValue(7, 1), agent.InstanceValue(7, 2)
	w := startWorker(t)
	if err := h.SetInstance(w.tgid(), first); err != nil {
		t.Fatal(err)
	}
	child := w.fork(t)
	for _, p := range []uint32{w.tgid(), child} {
		if got := instanceOf(t, h, p); got != first {
			t.Errorf("process %d (the worker %d or its fork) holds %d, want the whole value %d", p, w.tgid(), got, first)
		}
	}
	other := startWorker(t)
	if err := h.SetInstance(other.tgid(), later); !errors.Is(err, agent.ErrInstanceHeld) {
		t.Errorf("SetInstance of id 7's later claim %d while the earlier one's processes are in it: %v, want ErrInstanceHeld", later, err)
	}
	was := instanceLost{Forks: 2, LastTgid: 42, LastErr: -7}
	if err := h.objects.InstanceLost.Update(uint32(7), was, ebpf.UpdateAny); err != nil {
		t.Fatal(err)
	}
	if got, err := h.InstanceLost(first); err != nil || got != (agent.InstanceLoss{Forks: 2, LastTgid: 42, LastErr: -7}) {
		t.Errorf("InstanceLost(%d) = %+v, %v; want id 7's count", first, got, err)
	}
	n, err := h.EndInstance(first)
	if err != nil {
		t.Fatal(err)
	}
	if n < 2 {
		t.Errorf("EndInstance(%d) took %d processes out, want the worker and its fork", first, n)
	}
	for _, p := range []uint32{w.tgid(), child} {
		if got := instanceOf(t, h, p); got != 0 {
			t.Errorf("after its end process %d holds %d, want none", p, got)
		}
	}
	if err := h.SetInstance(other.tgid(), later); err != nil {
		t.Fatalf("SetInstance of id 7's later claim %d once it ended: %v, want it given", later, err)
	}
	if got := instanceOf(t, h, other.tgid()); got != later {
		t.Errorf("the later claim's worker holds %d, want %d", got, later)
	}
	if got, err := h.InstanceLost(later); err != nil || got != (agent.InstanceLoss{}) {
		t.Errorf("after the later claim InstanceLost(%d) = %+v, %v; want id 7's count zeroed", later, got, err)
	}
}

// Two workers' instances are live at once, and each process either forks is
// in its own worker's, never the other's.
func TestTwoWorkersForksAreEachInItsOwnInstance(t *testing.T) {
	h := instanceHooks(t)
	a, b := startWorker(t), startWorker(t)
	if err := h.SetInstance(a.tgid(), 5); err != nil {
		t.Fatal(err)
	}
	if err := h.SetInstance(b.tgid(), 6); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		fa, fb := a.fork(t), b.fork(t)
		if got := instanceOf(t, h, fa); got != 5 {
			t.Errorf("worker %d's fork %d is in instance %d, want its worker's 5", a.tgid(), fa, got)
		}
		if got := instanceOf(t, h, fb); got != 6 {
			t.Errorf("worker %d's fork %d is in instance %d, want its worker's 6", b.tgid(), fb, got)
		}
	}
}

// An agent in a PID namespace of its own numbers processes by its own
// namespace's PIDs, not the kernel's TGIDs instance_of_tgid holds: it gives
// no instance, and says why, both for the whole run (InstanceRefusal) and
// to a process it is asked to put in one. An agent in the host's refuses
// none for the whole run.
func TestAnAgentInAPIDNamespaceOfItsOwnGivesNoInstance(t *testing.T) {
	h := NewHooks(zap.NewNop(), &config.Config{})
	if !inOwnPIDNamespace(t) {
		if err := h.InstanceRefusal(); err != nil {
			t.Errorf("InstanceRefusal in the host's PID namespace: %v, want none", err)
		}
		return
	}
	if err := h.InstanceRefusal(); !errors.Is(err, errNotHostPIDs) {
		t.Errorf("InstanceRefusal in a PID namespace of its own: %v, want errNotHostPIDs", err)
	}
	if err := h.SetInstance(uint32(os.Getpid()), 3); !errors.Is(err, errNotHostPIDs) {
		t.Errorf("SetInstance from a PID namespace of its own: %v, want errNotHostPIDs", err)
	}
}

// Unloading the hooks detaches k_instance_fork with them: no program of an
// agent that closed runs at the host's forks.
func TestUnloadingTheHooksDetachesTheInstanceFork(t *testing.T) {
	h := instanceHooks(t)
	w := startWorker(t)
	if err := h.SetInstance(w.tgid(), 4); err != nil {
		t.Fatal(err)
	}
	l := h.instanceFork
	if l == nil {
		t.Fatal("SetInstance attached no k_instance_fork")
	}
	if _, err := l.Info(); err != nil {
		t.Fatalf("k_instance_fork's link before the unload: %v", err)
	}
	h.unLoad(context.Background(), agent.HookCfg{})
	if h.instanceFork != nil {
		t.Error("the hooks unloaded and still hold k_instance_fork's link")
	}
	if _, err := l.Info(); err == nil {
		t.Error("the hooks unloaded and k_instance_fork's link is still open")
	}
}
