//go:build linux

package linux

import (
	"errors"
	"fmt"
	"math"

	"github.com/cilium/ebpf"
	"github.com/cilium/ebpf/link"
	"golang.org/x/sys/unix"

	"go.keploy.io/server/v3/pkg/agent"
)

// Keploy instances (agent.InstanceStamper): a test worker's processes,
// kept by the kernel (keploy/ebpf k_instance.h). instance_of_tgid maps a
// process's kernel TGID to its instance; the agent writes the worker's own
// entry (SetInstance), k_instance_fork copies it to each new process a
// member forks, k_process_exit deletes a process's at its exit, and the
// sweep (sweepGonePIDs) what a skipped exit left. connect4 and connect6
// stamp the caller's instance into the connection's dest_info_t, which the
// proxy reads from the socket it accepted (ProxyConnPair).

// instanceLost mirrors struct instance_lost_t (k_instance.h): the forks of
// an instance the kernel could not put in it.
type instanceLost struct {
	Forks    uint64
	LastTgid uint32
	LastErr  int32
}

var _ agent.InstanceStamper = (*Hooks)(nil)

// SetInstance puts the process tgid (a kernel TGID) in instance id, and
// every process it forks from then on (agent.InstanceStamper).
//
// k_instance_fork is attached at the first call, so a host with no
// instance runs no program at its forks; the process's entry is written
// after it, so no fork of the process from then on is missed. The entry is
// written only where none is (a process already in an instance stays in
// it), and checked after: a process that exited, or became a zombie, before
// the write had its exit run before there was anything to delete, so its
// entry is deleted here and the call refused.
func (h *Hooks) SetInstance(tgid, id uint32) error {
	if id == 0 || id > agent.MaxInstanceID {
		return fmt.Errorf("keploy instance %d is not one the kernel keeps (1 to %d)", id, agent.MaxInstanceID)
	}
	if tgid == 0 || tgid > math.MaxInt32 {
		return fmt.Errorf("%d is not a process's TGID", tgid)
	}
	if err := hostPIDs(); err != nil {
		return err
	}
	h.objectsMutex.RLock()
	defer h.objectsMutex.RUnlock()
	of, lost := h.objects.InstanceOfTgid, h.objects.InstanceLost
	if of == nil || lost == nil {
		return errors.New("the hooks are not loaded: instance_of_tgid is not open")
	}
	held, err := instanceMembers(of, id)
	if err != nil {
		return err
	}
	if len(held) > 0 {
		return fmt.Errorf("%w: instance %d still has %d processes (process %d among them)", agent.ErrInstanceHeld, id, len(held), held[0])
	}
	if err := lost.Update(id, instanceLost{}, ebpf.UpdateAny); err != nil {
		return fmt.Errorf("clear instance %d's count of lost forks (instance_lost): %w", id, err)
	}
	if err := h.attachInstanceFork(); err != nil {
		return err
	}
	if err := of.Update(tgid, id, ebpf.UpdateNoExist); err != nil {
		if errors.Is(err, ebpf.ErrKeyExist) {
			var cur uint32
			if of.Lookup(tgid, &cur) == nil {
				return fmt.Errorf("process %d is already in keploy instance %d", tgid, cur)
			}
		}
		return fmt.Errorf("put process %d in keploy instance %d (instance_of_tgid): %w", tgid, id, err)
	}
	if gone(tgid) {
		// No live process has the TGID, so no entry under it is anyone's.
		_ = of.Delete(tgid)
		return fmt.Errorf("process %d exited before it could be put in keploy instance %d", tgid, id)
	}
	return nil
}

// EndInstance takes every process out of instance id and says how many were
// in it (agent.InstanceStamper). A member whose exit deleted its entry first
// is counted all the same. A delete the kernel refuses leaves the process in
// the instance, and SetInstance then refuses the id (agent.ErrInstanceHeld).
func (h *Hooks) EndInstance(id uint32) (int, error) {
	if id == 0 || id > agent.MaxInstanceID {
		return 0, fmt.Errorf("keploy instance %d is not one the kernel keeps (1 to %d)", id, agent.MaxInstanceID)
	}
	h.objectsMutex.RLock()
	defer h.objectsMutex.RUnlock()
	of := h.objects.InstanceOfTgid
	if of == nil {
		return 0, errors.New("the hooks are not loaded: instance_of_tgid is not open")
	}
	members, err := instanceMembers(of, id)
	if err != nil {
		return 0, err
	}
	for _, tgid := range members {
		_ = of.Delete(tgid)
	}
	return len(members), nil
}

// InstanceLost is what the kernel could not put in instance id since
// SetInstance last gave it (agent.InstanceStamper).
func (h *Hooks) InstanceLost(id uint32) (agent.InstanceLoss, error) {
	if id == 0 || id > agent.MaxInstanceID {
		return agent.InstanceLoss{}, fmt.Errorf("keploy instance %d is not one the kernel keeps (1 to %d)", id, agent.MaxInstanceID)
	}
	h.objectsMutex.RLock()
	defer h.objectsMutex.RUnlock()
	lost := h.objects.InstanceLost
	if lost == nil {
		return agent.InstanceLoss{}, errors.New("the hooks are not loaded: instance_lost is not open")
	}
	var v instanceLost
	if err := lost.Lookup(id, &v); err != nil {
		return agent.InstanceLoss{}, fmt.Errorf("read instance %d's lost forks (instance_lost): %w", id, err)
	}
	return agent.InstanceLoss{Forks: v.Forks, LastTgid: v.LastTgid, LastErr: v.LastErr}, nil
}

// instanceMembers are the TGIDs instance_of_tgid holds in instance id.
func instanceMembers(of *ebpf.Map, id uint32) ([]uint32, error) {
	var out []uint32
	var k, v uint32
	it := of.Iterate()
	for it.Next(&k, &v) {
		if v == id {
			out = append(out, k)
		}
	}
	if err := it.Err(); err != nil {
		return nil, fmt.Errorf("iterate instance_of_tgid: %w", err)
	}
	return out, nil
}

// attachInstanceFork attaches k_instance_fork to sched_process_fork, once
// (a raw tracepoint: no tracefs). The caller holds objectsMutex for reading;
// unLoad closes the link with it held for writing.
func (h *Hooks) attachInstanceFork() error {
	h.instanceMu.Lock()
	defer h.instanceMu.Unlock()
	if h.instanceFork != nil {
		return nil
	}
	if h.objects.K_instanceFork == nil {
		return errors.New("the hooks are not loaded: k_instance_fork is not open")
	}
	l, err := link.AttachRawTracepoint(link.RawTracepointOptions{Name: "sched_process_fork", Program: h.objects.K_instanceFork})
	if err != nil {
		return fmt.Errorf("attach k_instance_fork, which puts each process a test worker forks in its keploy instance, to sched_process_fork: %w", err)
	}
	h.instanceFork = l
	return nil
}

// closeInstanceFork detaches k_instance_fork. The caller holds objectsMutex
// for writing.
func (h *Hooks) closeInstanceFork() error {
	h.instanceMu.Lock()
	defer h.instanceMu.Unlock()
	if h.instanceFork == nil {
		return nil
	}
	err := h.instanceFork.Close()
	h.instanceFork = nil
	return err
}

// hostPIDs refuses an agent outside the initial PID namespace: the kernel's
// instance entries are by kernel TGID, which such an agent cannot tell from
// its own process numbers.
func hostPIDs() error {
	var st unix.Stat_t
	if err := unix.Stat("/proc/self/ns/pid", &st); err != nil {
		return err
	}
	if st.Ino != initialPIDNSInode {
		return fmt.Errorf("keploy instances need an agent in the host PID namespace: %w", errNotHostPIDs)
	}
	return nil
}

// gone says the process tgid (initial PID namespace) has exited: reaped
// (kill says ESRCH), or a zombie with no thread left, whose exit the kernel
// has already run.
func gone(tgid uint32) bool {
	return errors.Is(unix.Kill(int(tgid), 0), unix.ESRCH) || zombieProcess(tgid)
}
